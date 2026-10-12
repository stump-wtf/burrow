package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store/storetest"
)

// swept waits for token's row to disappear: LookupSession answers ErrExpired
// while the row exists and ErrNotFound once the reaper has deleted it.
func swept(t *testing.T, st *store.Store, token string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := st.LookupSession(context.Background(), token)
		if errors.Is(err, store.ErrNotFound) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the expired session was never swept; last lookup: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The reaper sweeps at boot without waiting an interval, sweeps again on
// every tick, and returns when its context ends.
func TestReapExpiredSweepsAtBootThenOnATickAndStops(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	if err := st.UpsertUser(ctx, store.User{ID: "sub-1", Email: "joe@example.com"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	expired := func() string {
		t.Helper()
		token, err := st.CreateSession(ctx, "sub-1", -time.Minute)
		if err != nil {
			t.Fatalf("seed expired session: %v", err)
		}
		return token
	}

	atBoot := expired()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		reapExpired(runCtx, st, 50*time.Millisecond)
	}()
	swept(t, st, atBoot)
	// Written after the boot sweep, so only a tick can remove it.
	swept(t, st, expired())

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reaper must return when its context ends")
	}
}

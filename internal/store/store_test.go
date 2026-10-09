package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedUser(t *testing.T, st *store.Store) store.User {
	t.Helper()
	u := store.User{ID: "sub-123", Email: "joe@example.com", Name: "Joe"}
	if err := st.UpsertUser(context.Background(), u); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	return u
}

// A grant must work exactly once: the second exchange of the same grant is
// refused even though the first succeeded.
func TestGrantIsSingleUse(t *testing.T) {
	st := openTestStore(t)
	user := seedUser(t, st)
	ctx := context.Background()

	grant, err := st.MintGrant(ctx, user.ID, time.Minute)
	if err != nil {
		t.Fatalf("mint grant: %v", err)
	}
	if len(grant) < 12 {
		t.Fatalf("grant must be at least 12 characters for manual paste, got %d", len(grant))
	}

	if _, err := st.ConsumeGrant(ctx, grant); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := st.ConsumeGrant(ctx, grant); !errors.Is(err, store.ErrGrantInvalid) {
		t.Fatalf("second consume must fail with ErrGrantInvalid, got %v", err)
	}
}

func TestExpiredGrantRefused(t *testing.T) {
	st := openTestStore(t)
	user := seedUser(t, st)

	grant, err := st.MintGrant(context.Background(), user.ID, -time.Minute)
	if err != nil {
		t.Fatalf("mint grant: %v", err)
	}
	if _, err := st.ConsumeGrant(context.Background(), grant); !errors.Is(err, store.ErrGrantInvalid) {
		t.Fatalf("expired grant must be refused with ErrGrantInvalid, got %v", err)
	}
}

func TestUnknownGrantRefused(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.ConsumeGrant(context.Background(), "not-a-real-grant"); !errors.Is(err, store.ErrGrantInvalid) {
		t.Fatalf("unknown grant must be refused with ErrGrantInvalid, got %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	st := openTestStore(t)
	user := seedUser(t, st)
	ctx := context.Background()

	token, err := st.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if got, err := st.LookupSession(ctx, token); err != nil || got != user.ID {
		t.Fatalf("lookup session: %v %q", err, got)
	}

	if err := st.DeleteSession(ctx, token); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := st.LookupSession(ctx, token); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted session must be unknown, got %v", err)
	}
}

func TestExpiredSessionDistinguishedFromUnknown(t *testing.T) {
	st := openTestStore(t)
	user := seedUser(t, st)

	token, err := st.CreateSession(context.Background(), user.ID, -time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := st.LookupSession(context.Background(), token); !errors.Is(err, store.ErrExpired) {
		t.Fatalf("expired session must report ErrExpired (the API maps it to session_expired), got %v", err)
	}
}

func TestOIDCStateSingleUse(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.SaveOIDCState(ctx, "state-1", "verifier-1", true, time.Minute); err != nil {
		t.Fatalf("save state: %v", err)
	}
	verifier, desktop, err := st.TakeOIDCState(ctx, "state-1")
	if err != nil || verifier != "verifier-1" || !desktop {
		t.Fatalf("take state: %v %q %v", err, verifier, desktop)
	}
	if _, _, err := st.TakeOIDCState(ctx, "state-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("state must be single-use, got %v", err)
	}
}

func TestMCPTokenRoundTrip(t *testing.T) {
	st := openTestStore(t)
	user := seedUser(t, st)
	ctx := context.Background()

	token, expiresAt, err := st.MintMCPToken(ctx, user.ID, "org_burrow", time.Hour)
	if err != nil {
		t.Fatalf("mint mcp token: %v", err)
	}
	if !expiresAt.After(time.Now()) {
		t.Fatalf("mcp token must expire in the future")
	}
	uid, orgID, err := st.LookupMCPToken(ctx, token)
	if err != nil || uid != user.ID || orgID != "org_burrow" {
		t.Fatalf("lookup mcp token: %v %q %q", err, uid, orgID)
	}
	if _, _, err := st.LookupMCPToken(ctx, "bogus"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown mcp token must be ErrNotFound, got %v", err)
	}
}

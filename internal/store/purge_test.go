package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// Nothing used to delete an expired row: sessions, grants, sign-in states and
// MCP tokens stayed forever, and the sign-in states are written by an
// unauthenticated GET. The sweep removes exactly the expired rows of each
// kind and leaves every live one working.
func TestPurgeExpiredRemovesOnlyExpiredRows(t *testing.T) {
	st := openTestStore(t)
	user := seedUser(t, st)
	ctx := context.Background()

	const expired = 25
	var deadSession, deadMCP string
	for i := 0; i < expired; i++ {
		var err error
		if deadSession, err = st.CreateSession(ctx, user.ID, -time.Minute); err != nil {
			t.Fatalf("seed expired session: %v", err)
		}
		if _, err = st.MintGrant(ctx, user.ID, -time.Minute); err != nil {
			t.Fatalf("seed expired grant: %v", err)
		}
		if err = st.SaveOIDCState(ctx, fmt.Sprintf("expired-%d", i), "verifier", false, -time.Minute); err != nil {
			t.Fatalf("seed expired state: %v", err)
		}
		if deadMCP, _, err = st.MintMCPToken(ctx, user.ID, "org_burrow", "", -time.Minute); err != nil {
			t.Fatalf("seed expired mcp token: %v", err)
		}
	}
	session, err := st.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("seed live session: %v", err)
	}
	// The live grant is accounted for by the exact count below; that it still
	// exchanges after a purge is asserted over HTTP in internal/httpapi.
	if _, err := st.MintGrant(ctx, user.ID, time.Hour); err != nil {
		t.Fatalf("seed live grant: %v", err)
	}
	if err := st.SaveOIDCState(ctx, "live", "verifier-live", true, time.Hour); err != nil {
		t.Fatalf("seed live state: %v", err)
	}
	mcp, _, err := st.MintMCPToken(ctx, user.ID, "org_burrow", "", time.Hour)
	if err != nil {
		t.Fatalf("seed live mcp token: %v", err)
	}

	purged, err := st.PurgeExpired(ctx)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	want := store.Purged{Sessions: expired, Grants: expired, OIDCStates: expired, MCPTokens: expired}
	if purged != want {
		t.Fatalf("purge must remove every expired row and nothing else: got %+v, want %+v", purged, want)
	}
	// The rows are gone, not merely counted: a swept token is unknown, and a
	// second sweep finds nothing left.
	if _, err := st.LookupSession(ctx, deadSession); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a swept session must be gone, got %v", err)
	}
	if _, _, err := st.LookupMCPToken(ctx, deadMCP); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a swept mcp token must be gone, got %v", err)
	}
	if again, err := st.PurgeExpired(ctx); err != nil || again != (store.Purged{}) {
		t.Fatalf("a second purge must find nothing: %+v (err %v)", again, err)
	}

	if got, err := st.LookupSession(ctx, session); err != nil || got != user.ID {
		t.Fatalf("a live session must survive the purge: %v %q", err, got)
	}
	if verifier, desktop, err := st.TakeOIDCState(ctx, "live"); err != nil || verifier != "verifier-live" || !desktop {
		t.Fatalf("a live sign-in state must survive the purge: %v %q %v", err, verifier, desktop)
	}
	if got, org, err := st.LookupMCPToken(ctx, mcp); err != nil || got != user.ID || org != "org_burrow" {
		t.Fatalf("a live mcp token must survive the purge: %v %q %q", err, got, org)
	}
}

// The sweep is housekeeping, not the check: an expired row the sweep has not
// reached yet is still refused by the lookup itself, with the same errors as
// before there was a sweep. (TestExpiredGrantRefused is the grant's case.)
func TestExpiredRowsAreRefusedBeforeAnyPurge(t *testing.T) {
	st := openTestStore(t)
	user := seedUser(t, st)
	ctx := context.Background()

	session, err := st.CreateSession(ctx, user.ID, -time.Minute)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := st.LookupSession(ctx, session); !errors.Is(err, store.ErrExpired) {
		t.Fatalf("an unswept expired session must be ErrExpired, got %v", err)
	}

	mcp, _, err := st.MintMCPToken(ctx, user.ID, "org_burrow", "", -time.Minute)
	if err != nil {
		t.Fatalf("seed mcp token: %v", err)
	}
	if _, _, err := st.LookupMCPToken(ctx, mcp); !errors.Is(err, store.ErrExpired) {
		t.Fatalf("an unswept expired mcp token must be ErrExpired, got %v", err)
	}

	if err := st.SaveOIDCState(ctx, "stale", "verifier", false, -time.Minute); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	if _, _, err := st.TakeOIDCState(ctx, "stale"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unswept expired sign-in state must be ErrNotFound, got %v", err)
	}
}

// Admin API tokens are not part of the sweep: an expired one stays on the
// admin's list until it is revoked by name.
func TestPurgeExpiredLeavesAPITokens(t *testing.T) {
	st := openTestStore(t)
	user := seedUser(t, st)
	ctx := context.Background()

	if _, _, err := st.MintAPIToken(ctx, user.ID, "ci", []string{"llm-admin"}, -time.Minute); err != nil {
		t.Fatalf("mint api token: %v", err)
	}
	if _, err := st.PurgeExpired(ctx); err != nil {
		t.Fatalf("purge: %v", err)
	}
	tokens, err := st.ListAPITokens(ctx)
	if err != nil {
		t.Fatalf("list api tokens: %v", err)
	}
	if len(tokens) != 1 || tokens[0].Name != "ci" {
		t.Fatalf("an expired api token must stay listed, got %#v", tokens)
	}
}

package store_test

// Revocation (issue #41): sign-out must end the session's whole family —
// handoff-derived sessions and the MCP tokens any of them minted — and an
// admin must be able to end everything a member holds without raw SQL.
// Governing: SPEC-0001 REQ "Session revocation", ADR-0008.

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// family mints a handoff session under parent, one generation deep.
func family(t *testing.T, st *store.Store, parent string, ttl time.Duration) string {
	t.Helper()
	child, _ := handoff(t, st, parent, ttl)
	return child
}

// Sign-out deletes one row, and the cascade makes that row stand for the
// family: sessions a desktop handoff derived from it die, and so does every
// MCP token the family minted. The issue reproduced the opposite on main:
// /v1/me with a derived session answering 200 after sign-out.
func TestSignOutRevokesTheSessionFamily(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	alice := seedUser(t, st)
	bob := store.User{ID: "bob-sub", Email: "bob@example.com", Name: "Bob"}
	if err := st.UpsertUser(ctx, bob); err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	root, err := st.CreateSession(ctx, alice.ID, time.Hour)
	if err != nil {
		t.Fatalf("create root session: %v", err)
	}
	child := family(t, st, root, time.Hour)
	grandchild := family(t, st, child, time.Hour)

	rootToken, _, err := st.MintMCPToken(ctx, alice.ID, "org_burrow", root, time.Hour)
	if err != nil {
		t.Fatalf("mint mcp token from root: %v", err)
	}
	childToken, _, err := st.MintMCPToken(ctx, alice.ID, "org_burrow", child, time.Hour)
	if err != nil {
		t.Fatalf("mint mcp token from child: %v", err)
	}

	bobRoot, err := st.CreateSession(ctx, bob.ID, time.Hour)
	if err != nil {
		t.Fatalf("create bob session: %v", err)
	}
	bobToken, _, err := st.MintMCPToken(ctx, bob.ID, "org_burrow", bobRoot, time.Hour)
	if err != nil {
		t.Fatalf("mint bob mcp token: %v", err)
	}

	if err := st.DeleteSession(ctx, root); err != nil {
		t.Fatalf("sign out: %v", err)
	}

	for _, revoked := range []string{root, child, grandchild} {
		if _, err := st.LookupSession(ctx, revoked); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a family session must be gone after sign-out, got %v", err)
		}
	}
	for _, revoked := range []string{rootToken, childToken} {
		if _, _, err := st.LookupMCPToken(ctx, revoked); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a token the family minted must be gone after sign-out, got %v", err)
		}
	}

	// The other member is exactly as untouched as before: the cascade walks
	// the family, not the org.
	if got, err := st.LookupSession(ctx, bobRoot); err != nil || got != bob.ID {
		t.Fatalf("another member's session must survive a sign-out, got %v %q", err, got)
	}
	if got, _, err := st.LookupMCPToken(ctx, bobToken); err != nil || got != bob.ID {
		t.Fatalf("another member's mcp token must survive, got %v %q", err, got)
	}
}

// Signing out an intermediate generation revokes everything under it and
// leaves the generations above it alone: revocation is downward, so signing
// out the desktop does not sign the browser out.
func TestSignOutRevokesDownwardOnly(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	user := seedUser(t, st)

	root, err := st.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	child := family(t, st, root, time.Hour)
	grandchild := family(t, st, child, time.Hour)

	if err := st.DeleteSession(ctx, child); err != nil {
		t.Fatalf("sign out the child: %v", err)
	}
	if _, err := st.LookupSession(ctx, grandchild); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the generation below must be revoked, got %v", err)
	}
	if got, err := st.LookupSession(ctx, root); err != nil || got != user.ID {
		t.Fatalf("the generation above must survive, got %v %q", err, got)
	}
}

// A handoff grant that was minted but not yet exchanged dies with the
// session that minted it: signing out cannot leave a credential lying around
// that buys a session in the signed-out family.
func TestSignOutTakesPendingHandoffGrants(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	user := seedUser(t, st)

	root, err := st.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	grant, err := st.MintHandoffGrant(ctx, root, time.Minute)
	if err != nil {
		t.Fatalf("mint handoff grant: %v", err)
	}
	if err := st.DeleteSession(ctx, root); err != nil {
		t.Fatalf("sign out: %v", err)
	}
	if _, _, err := st.ConsumeGrant(ctx, grant); !errors.Is(err, store.ErrGrantInvalid) {
		t.Fatalf("a grant from a signed-out session must be invalid, got %v", err)
	}
}

// RevokeMember is the admin path the issue asked for: no raw SQL, and it
// ends every credential a member holds — sessions (with their families),
// pending grants, MCP tokens, and machine API tokens — plus the stored group
// snapshot, so nothing is served from groups the issuer no longer asserts.
func TestRevokeMemberEndsEveryCredential(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	alice := seedUser(t, st)
	if err := st.UpsertUser(ctx, store.User{ID: alice.ID, Email: alice.Email, Name: alice.Name, Groups: []string{"eng", "stumpcloud-admins"}}); err != nil {
		t.Fatalf("record alice groups: %v", err)
	}
	bob := store.User{ID: "bob-sub", Email: "bob@example.com", Name: "Bob"}
	if err := st.UpsertUser(ctx, bob); err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	aliceRoot, err := st.CreateSession(ctx, alice.ID, time.Hour)
	if err != nil {
		t.Fatalf("create alice session: %v", err)
	}
	aliceChild := family(t, st, aliceRoot, time.Hour)
	if _, err := st.MintHandoffGrant(ctx, aliceRoot, time.Minute); err != nil {
		t.Fatalf("mint pending grant: %v", err)
	}
	aliceMCP, _, err := st.MintMCPToken(ctx, alice.ID, "org_burrow", aliceChild, time.Hour)
	if err != nil {
		t.Fatalf("mint alice mcp token: %v", err)
	}
	aliceAPI, aliceAPITok, err := st.MintAPIToken(ctx, alice.ID, "ci", []string{"llm-admin"}, time.Hour)
	if err != nil {
		t.Fatalf("mint alice api token: %v", err)
	}

	bobRoot, err := st.CreateSession(ctx, bob.ID, time.Hour)
	if err != nil {
		t.Fatalf("create bob session: %v", err)
	}
	bobMCP, _, err := st.MintMCPToken(ctx, bob.ID, "org_burrow", bobRoot, time.Hour)
	if err != nil {
		t.Fatalf("mint bob mcp token: %v", err)
	}

	rev, err := st.RevokeMember(ctx, alice.ID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if rev.Sessions != 2 || rev.Grants != 1 || rev.MCPTokens != 1 || rev.APITokens != 1 {
		t.Fatalf("revocation counts = %+v, want 2 sessions / 1 grant / 1 mcp / 1 api", rev)
	}
	if !rev.GroupsCleared {
		t.Fatalf("the group snapshot must be cleared")
	}

	for _, s := range []string{aliceRoot, aliceChild} {
		if _, err := st.LookupSession(ctx, s); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("alice session must be gone, got %v", err)
		}
	}
	if _, _, err := st.LookupMCPToken(ctx, aliceMCP); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("alice mcp token must be gone, got %v", err)
	}
	if _, err := st.LookupAPIToken(ctx, aliceAPI); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("alice api token must be gone, got %v", err)
	}
	if u, err := st.GetUser(ctx, alice.ID); err != nil || len(u.Groups) != 0 {
		t.Fatalf("alice's group snapshot must be empty, got %+v %v", u, err)
	}

	// Revoke is idempotent: the second call finds nothing and says so.
	again, err := st.RevokeMember(ctx, alice.ID)
	if err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if again.Sessions != 0 || again.Grants != 0 || again.MCPTokens != 0 || again.APITokens != 0 || again.GroupsCleared {
		t.Fatalf("second revocation must be a no-op, got %+v", again)
	}

	// Bob was never part of the question.
	if got, err := st.LookupSession(ctx, bobRoot); err != nil || got != bob.ID {
		t.Fatalf("bob's session must be untouched, got %v %q", err, got)
	}
	if got, _, err := st.LookupMCPToken(ctx, bobMCP); err != nil || got != bob.ID {
		t.Fatalf("bob's mcp token must be untouched, got %v %q", err, got)
	}
	if aliceAPITok.UserID != alice.ID {
		t.Fatalf("test setup: api token must be alice's")
	}
}

// A typo'd member id must not answer success with nothing revoked.
func TestRevokeUnknownMemberIsNotFound(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.RevokeMember(context.Background(), "nobody-sub"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown member must be ErrNotFound, got %v", err)
	}
}

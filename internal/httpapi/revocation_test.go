package httpapi_test

// Revocation over the wire (issue #41): signing out must end the handoff
// family and the MCP tokens it minted, and an admin must be able to end a
// member's access without raw SQL. Governing: SPEC-0001 REQ
// "Session revocation", ADR-0008.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// mcpCallAs is one tools/call of execute_capability over the MCP endpoint,
// carrying token as the MCP bearer.
func mcpCallAs(t *testing.T, e *testEnv, token, capability string) (*http.Response, []byte) {
	t.Helper()
	return e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{
			"name":      "execute_capability",
			"arguments": map[string]any{"capability": "skill:" + capability},
		},
	})
}

// The issue's reproduction, end to end: sign a session out and nothing that
// descended from it — the desktop session a handoff derived, or an MCP token
// any of them minted — keeps answering.
func TestSignOutRevokesHandoffSessionAndMintedTokens(t *testing.T) {
	e := newTestEnv(t)

	desktop := e.handoff(t, e.token)
	if status, _ := e.callAs(t, desktop, http.MethodGet, "/api/den/v1/me", nil); status != http.StatusOK {
		t.Fatalf("the derived session works while the sign-in is live, got %d", status)
	}

	status, minted := e.callAs(t, desktop, http.MethodPost, "/api/den/v1/mcp/token", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("mcp mint status %d: %v", status, minted)
	}
	mcpToken := nonEmptyString(t, minted, "token")

	resp, body := mcpCallAs(t, e, mcpToken, "code-review")
	if resp.StatusCode != http.StatusOK || !bytesContain(body, "Follow this skill for code-review") {
		t.Fatalf("the skill body is served before sign-out, got %d: %s", resp.StatusCode, body)
	}

	if status, _ := e.callAs(t, e.token, http.MethodPost, "/api/den/api/auth/sign-out", nil); status != http.StatusOK {
		t.Fatalf("sign-out status %d", status)
	}

	if status, got := e.callAs(t, desktop, http.MethodGet, "/api/den/v1/me", nil); status != http.StatusUnauthorized || got["error"] != "invalid_session" {
		t.Fatalf("/v1/me with the derived session after sign-out: %d %v — the family must be revoked", status, got)
	}
	resp, body = mcpCallAs(t, e, mcpToken, "code-review")
	if resp.StatusCode != http.StatusUnauthorized || !bytesContain(body, "invalid_mcp_token") {
		t.Fatalf("tools/call with the family's MCP token after sign-out: %d %s", resp.StatusCode, body)
	}

	// The signed-out session itself is dead like it always was.
	if status, _ := e.callAs(t, e.token, http.MethodGet, "/api/den/v1/me", nil); status != http.StatusUnauthorized {
		t.Fatalf("the signed-out session must be dead, got %d", status)
	}
}

// bytesContain is strings.Contains over a byte slice without the import
// dance at every call site.
func bytesContain(hay []byte, needle string) bool {
	return strings.Contains(string(hay), needle)
}

// seedMember creates a member holding everything a member can hold: a
// session, a handoff-derived session under it, an MCP token minted by that
// family, a machine API token, and a stored group snapshot. It returns each
// value the revoke test must find dead afterwards.
func seedMember(t *testing.T, e *testEnv, id, email string, groups []string) (root, derived, mcp, api string) {
	t.Helper()
	ctx := context.Background()
	if err := e.st.UpsertUser(ctx, store.User{ID: id, Email: email, Name: id, Groups: groups}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	var err error
	if root, err = e.st.CreateSession(ctx, id, time.Hour); err != nil {
		t.Fatalf("%s session: %v", id, err)
	}
	grant, err := e.st.MintHandoffGrant(ctx, root, time.Minute)
	if err != nil {
		t.Fatalf("%s handoff grant: %v", id, err)
	}
	uid, lineage, err := e.st.ConsumeGrant(ctx, grant)
	if err != nil {
		t.Fatalf("%s consume grant: %v", id, err)
	}
	if derived, err = e.st.CreateSessionFrom(ctx, uid, time.Hour, lineage); err != nil {
		t.Fatalf("%s derived session: %v", id, err)
	}
	if mcp, _, err = e.st.MintMCPToken(ctx, id, e.cfg.OrgID(), derived, time.Hour); err != nil {
		t.Fatalf("%s mcp token: %v", id, err)
	}
	if api, _, err = e.st.MintAPIToken(ctx, id, "ci-"+id, []string{"llm-admin"}, time.Hour); err != nil {
		t.Fatalf("%s api token: %v", id, err)
	}
	return root, derived, mcp, api
}

// An admin revokes a member; every credential of theirs stops answering on
// every surface, their group snapshot is cleared, and the next door —
// another member — is untouched.
func TestAdminRevokeMemberEndsAccessAndSparesOthers(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")

	bobRoot, bobDerived, bobMCP, bobAPI := seedMember(t, e, "bob-sub", "bob@example.com", []string{"eng"})
	aliceRoot := e.token

	var out any
	resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/members/bob-sub/revoke", nil, &out, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status %d", resp.StatusCode)
	}
	revoked := mapOf(t, mapOf(t, out)["revoked"])
	if revoked["member"] != "bob-sub" || revoked["sessions"].(float64) != 2 ||
		revoked["mcpTokens"].(float64) != 1 || revoked["apiTokens"].(float64) != 1 ||
		revoked["groupsCleared"] != true {
		t.Fatalf("revoked summary = %#v", revoked)
	}

	for _, session := range []string{bobRoot, bobDerived} {
		if status, body := e.callAs(t, session, http.MethodGet, "/api/den/v1/me", nil); status != http.StatusUnauthorized || body["error"] != "invalid_session" {
			t.Fatalf("bob session after revoke: %d %v", status, body)
		}
	}
	if resp, body := mcpCallAs(t, e, bobMCP, "code-review"); resp.StatusCode != http.StatusUnauthorized || !bytesContain(body, "invalid_mcp_token") {
		t.Fatalf("bob mcp token after revoke: %d %s", resp.StatusCode, body)
	}
	if status, body := e.callAs(t, bobAPI, http.MethodGet, "/api/den/v1/admin/llm-providers", nil); status != http.StatusUnauthorized || body["error"] != "invalid_token" {
		t.Fatalf("bob api token after revoke: %d %v", status, body)
	}

	u, err := e.st.GetUser(context.Background(), "bob-sub")
	if err != nil || len(u.Groups) != 0 {
		t.Fatalf("bob's group snapshot must be cleared, got %+v %v", u, err)
	}

	// The admin is exactly as signed in as before.
	if status, _ := e.callAs(t, aliceRoot, http.MethodGet, "/api/den/v1/me", nil); status != http.StatusOK {
		t.Fatalf("the admin's own session must be untouched, got %d", status)
	}

	// Revoking again finds nothing and still answers success with zeros.
	if resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/members/bob-sub/revoke", nil, &out, "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("second revoke status %d", resp.StatusCode)
	}
	if revoked := mapOf(t, mapOf(t, out)["revoked"]); revoked["sessions"].(float64) != 0 {
		t.Fatalf("second revoke must report nothing left: %#v", revoked)
	}
}

// The gate's three refusals: a bat_ token gets session_required (a leaked
// automation token cannot end anyone's sessions — the #37 rule, ADR-0008);
// a member outside the admin groups gets admin_required; and a typo'd member
// id is a 404, not a success that revoked nothing.
func TestAdminRevokeMemberGateAndErrors(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")

	// Mint an llm-admin token in the admin's name: it reaches providers but
	// not this route.
	raw, _, err := e.st.MintAPIToken(context.Background(), aliceSub, "ci", []string{"llm-admin"}, time.Hour)
	if err != nil {
		t.Fatalf("mint api token: %v", err)
	}
	if status, body := e.callAs(t, raw, http.MethodPost, "/api/den/v1/admin/members/bob-sub/revoke", nil); status != http.StatusForbidden || body["error"] != "session_required" {
		t.Fatalf("bat_ bearer must be refused as the wrong credential kind, got %d %v", status, body)
	}

	e.cfg.AdminGroups = nil
	if status, body := e.callAs(t, e.token, http.MethodPost, "/api/den/v1/admin/members/bob-sub/revoke", nil); status != http.StatusForbidden || body["error"] != "admin_required" {
		t.Fatalf("non-admin session must be refused, got %d %v", status, body)
	}

	e.cfg.AdminGroups = []string{"stumpcloud-admins"}
	if status, body := e.callAs(t, e.token, http.MethodPost, "/api/den/v1/admin/members/nobody-sub/revoke", nil); status != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("unknown member must 404, got %d %v", status, body)
	}
}

// An admin revoking themselves is a legitimate self-lockout: the call
// succeeds, and the session that made it is dead on the next request.
func TestAdminRevokeSelfEndsOwnSession(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")

	var out any
	if resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/members/"+aliceSub+"/revoke", nil, &out, "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("self-revoke status %d", resp.StatusCode)
	}
	if status, _ := e.callAs(t, e.token, http.MethodGet, "/api/den/v1/me", nil); status != http.StatusUnauthorized {
		t.Fatalf("the admin's own session must be revoked by their own call, got %d", status)
	}
}

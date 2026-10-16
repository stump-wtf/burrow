package httpapi_test

// The blast radius of one leaked bat_ token (stump.wtf/burrow#37, ADR-0005):
// it cannot mint a successor, it cannot outlive the admin rights of the member
// who minted it, and it cannot sign an audit row as though a member wrote it.

import (
	"net/http"
	"strings"
	"testing"
)

// A token may not mint, list, or revoke tokens: extending the admin surface is
// a member's decision. Everything else the token may do is unchanged, so
// automation holding a token keeps working.
func TestAdminTokenCannotManageTokens(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")

	var minted any
	if resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/api-tokens",
		map[string]any{"name": "ci"}, &minted, "", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint by session: status %d", resp.StatusCode)
	}
	raw := nonEmptyString(t, mapOf(t, minted), "token")

	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/den/v1/admin/api-tokens"},
		{http.MethodPost, "/api/den/v1/admin/api-tokens"},
		{http.MethodDelete, "/api/den/v1/admin/api-tokens/" + raw},
	} {
		var refused any
		resp := adminDo(t, e, route.method, route.path, nil, &refused, raw, "")
		if resp.StatusCode != http.StatusForbidden || mapOf(t, refused)["error"] != "session_required" {
			t.Fatalf("%s %s with a token: status %d, want 403 session_required, body %#v",
				route.method, route.path, resp.StatusCode, refused)
		}
	}

	// A member's session still manages them.
	var listed any
	if resp := adminDo(t, e, http.MethodGet, "/api/den/v1/admin/api-tokens", nil, &listed, "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("list tokens by session: status %d", resp.StatusCode)
	}

	// The token keeps its provider rights: the gate bounds token management,
	// it does not retire the credential.
	var rows any
	if resp := adminDo(t, e, http.MethodGet, "/api/den/v1/admin/llm-providers", nil, &rows, raw, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("token listing providers: status %d", resp.StatusCode)
	}
}

// Demoting the member who minted a token ends what the token may do, on the
// next request, without deleting the token: a promotion put back restores the
// automation, and revoking it stays an admin's deliberate act.
func TestAdminTokenEndsWithMinterDemotion(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")

	var minted any
	if resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/api-tokens",
		map[string]any{"name": "ci"}, &minted, "", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint: status %d", resp.StatusCode)
	}
	raw := nonEmptyString(t, mapOf(t, minted), "token")

	var rows any
	if resp := adminDo(t, e, http.MethodGet, "/api/den/v1/admin/llm-providers", nil, &rows, raw, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("token before the demotion: status %d", resp.StatusCode)
	}

	signInAs(t, e) // the member's recorded claim now holds no admin group

	var refused any
	resp := adminDo(t, e, http.MethodGet, "/api/den/v1/admin/llm-providers", nil, &refused, raw, "")
	if resp.StatusCode != http.StatusForbidden || mapOf(t, refused)["error"] != "admin_required" {
		t.Fatalf("token of a demoted member: status %d, want 403 admin_required, body %#v", resp.StatusCode, refused)
	}
	var session any
	if resp := adminDo(t, e, http.MethodGet, "/api/den/v1/admin/llm-providers", nil, &session, "", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a demoted member's own session must be refused too, got %d", resp.StatusCode)
	}

	signInAs(t, e, "stumpcloud-admins")
	if resp := adminDo(t, e, http.MethodGet, "/api/den/v1/admin/llm-providers", nil, &rows, raw, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("token after the member holds an admin group again: status %d", resp.StatusCode)
	}
}

// A token named after a member's id writes audit rows that still read as the
// token's: the actor names the kind of principal, the token's own hash, and
// the member whose admin rights it borrows.
func TestAdminAuditActorNamesThePrincipalKind(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")

	var minted any
	if resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/api-tokens",
		map[string]any{"name": aliceSub}, &minted, "", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint: status %d", resp.StatusCode)
	}
	raw := nonEmptyString(t, mapOf(t, minted), "token")
	hash := nonEmptyString(t, mapOf(t, mapOf(t, minted)["apiToken"]), "hash")

	if resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers",
		providerBody("lpr_by_token", "Token Row", "https://gw.example/v1", "KEY_TOK"), nil, raw, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("token create: status %d", resp.StatusCode)
	}
	if resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers",
		providerBody("lpr_by_member", "Member Row", "https://gw.example/v1", "KEY_MEM"), nil, "", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("session create: status %d", resp.StatusCode)
	}

	var audit any
	e.getJSON(t, "/api/den/v1/admin/audit", &audit)
	actors := map[string]string{}
	for _, entry := range listOf(t, mapOf(t, audit)["audit"]) {
		row := mapOf(t, entry)
		actors[row["providerId"].(string)] = row["actor"].(string)
	}

	byToken, byMember := actors["lpr_by_token"], actors["lpr_by_member"]
	if byToken == aliceSub {
		t.Fatalf("a token named after a member must not write that member's actor")
	}
	if !strings.HasPrefix(byToken, "token:"+aliceSub+"#") || !strings.HasSuffix(byToken, "@user:"+aliceSub) {
		t.Fatalf("token actor must name the kind, the name, the token's hash, and the minter: %q", byToken)
	}
	if !strings.Contains(byToken, hash[:12]) {
		t.Fatalf("token actor must name the token by its own hash, not only its name: %q", byToken)
	}
	if byMember != "user:"+aliceSub {
		t.Fatalf("a session write must be audited as the member: %q", byMember)
	}
}

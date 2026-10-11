package httpapi_test

// The admin surface (SPEC-0004): who may write, what a write does to the
// desktop listing, and the optimistic-concurrency contract that makes
// automation safe. These routes are burrow-designed, so the assertions are
// burrow's own codes rather than the client contract's.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/httpapi"
	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// newAdminTestEnv opens the surface to members of stumpcloud-admins, the
// way BURROW_ADMIN_GROUPS does in deployment.
func newAdminTestEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	e.cfg.AdminGroups = []string{"stumpcloud-admins"}
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st,
		skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}),
		providers.NewRuntime(e.st, e.cfg.ModelsTTL), nil).Routes()
	return e
}

// adminDo sends one admin request: bearer "" means the test session's token,
// ifMatch is sent as If-Match when non-empty, and the decoded body lands in
// out.
func adminDo(t *testing.T, e *testEnv, method, path string, body any, out any, bearer, ifMatch string) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		bodyReader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequest(method, e.srv.URL+path, bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer == "" {
		bearer = e.token
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode response: %v", method, path, err)
		}
	}
	return resp
}

// adminRow is one row on the admin wire, as the tests read it.
func adminRow(t *testing.T, payload any) map[string]any {
	t.Helper()
	return mapOf(t, mapOf(t, payload)["llmProvider"])
}

func providerBody(id, name, baseURL, apiKeyEnv string) map[string]any {
	return map[string]any{
		"id": id, "providerId": "litellm", "name": name,
		"baseURL": baseURL, "apiKeyEnv": apiKeyEnv,
	}
}

// With no admin groups and no tokens the surface is closed: a signed-in
// member who is not an admin gets 403, and no credential at all gets 401.
func TestAdminSurfaceClosedUntilOpened(t *testing.T) {
	e := newTestEnv(t) // AdminGroups unset

	var denied any
	if resp := e.doJSON(t, http.MethodGet, "/api/den/v1/admin/llm-providers", nil, &denied, true); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin session must be refused, got %d", resp.StatusCode)
	}
	if mapOf(t, denied)["error"] != "admin_required" {
		t.Fatalf("refusal code must name what is missing, got %#v", denied)
	}

	var anonymous any
	if resp := e.doJSON(t, http.MethodGet, "/api/den/v1/admin/llm-providers", nil, &anonymous, false); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no bearer must be 401, got %d", resp.StatusCode)
	}

	// Setting BURROW_ADMIN_GROUPS is what opens it; the member's own
	// session and token are unchanged.
	e.cfg.AdminGroups = []string{"stumpcloud-admins"}
	signInAs(t, e, "stumpcloud-admins")
	if resp := e.getJSON(t, "/api/den/v1/admin/llm-providers", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin-group member must be served, got %d", resp.StatusCode)
	}
}

// The CRUD loop, the etag contract, and the audit trail an operator reads
// after the fact.
func TestAdminProviderCRUDAndAudit(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")

	var created any
	resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers",
		providerBody("lpr_admin", "Admin Row", "https://gw.example/v1", "KEY_ADMIN"), &created, "", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d", resp.StatusCode)
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatalf("create must return the row's etag")
	}
	row := adminRow(t, created)
	if row["id"] != "lpr_admin" || row["apiKeyEnv"] != "KEY_ADMIN" {
		t.Fatalf("created row: %#v", row)
	}
	if _, hasModels := row["models"]; hasModels {
		t.Fatalf("admin rows carry no model menu — menus are derived: %#v", row)
	}
	etag, _ := row["etag"].(string)
	if etag == "" {
		t.Fatalf("body must carry the etag beside the header: %#v", row)
	}

	// A second create with the same id is a conflict, not an overwrite.
	var dup any
	if resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers",
		providerBody("lpr_admin", "Clash", "https://other.example/v1", "KEY_CLASH"), &dup, "", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate create must 409, got %d", resp.StatusCode)
	}
	if mapOf(t, dup)["error"] != "duplicate_id" {
		t.Fatalf("conflict body: %#v", dup)
	}

	// PUT without If-Match must not guess which version is being replaced.
	var noMatch any
	if resp := adminDo(t, e, http.MethodPut, "/api/den/v1/admin/llm-providers/lpr_admin",
		providerBody("lpr_admin", "Renamed", "https://gw.example/v1", "KEY_ADMIN"), &noMatch, "", ""); resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("PUT without If-Match must 428, got %d", resp.StatusCode)
	}

	// A stale If-Match is 412, and the body carries the current row so the
	// caller can rebase in one round trip.
	var stale any
	resp = adminDo(t, e, http.MethodPut, "/api/den/v1/admin/llm-providers/lpr_admin",
		providerBody("lpr_admin", "Renamed", "https://gw.example/v1", "KEY_ADMIN"), &stale, "", `"1970-01-01T00:00:00Z"`)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match must 412, got %d", resp.StatusCode)
	}
	if adminRow(t, stale)["name"] != "Admin Row" {
		t.Fatalf("412 body must carry the current row: %#v", stale)
	}
	if adminRow(t, stale)["etag"] != etag {
		t.Fatalf("412 body must carry the current etag: %#v", adminRow(t, stale))
	}

	var updated any
	resp = adminDo(t, e, http.MethodPut, "/api/den/v1/admin/llm-providers/lpr_admin",
		providerBody("lpr_admin", "Renamed", "https://gw.example/v1", "KEY_ADMIN"), &updated, "", etag)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT with a fresh If-Match must 200, got %d", resp.StatusCode)
	}
	newEtag := adminRow(t, updated)["etag"].(string)
	if newEtag == etag {
		t.Fatalf("a replace must advance the row's updated_at: %q", newEtag)
	}

	// Unknown id on the write paths.
	if resp := adminDo(t, e, http.MethodGet, "/api/den/v1/admin/llm-providers/lpr_nope", nil, nil, "", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id must 404, got %d", resp.StatusCode)
	}

	// Delete, then the row is gone from both surfaces.
	if resp := adminDo(t, e, http.MethodDelete, "/api/den/v1/admin/llm-providers/lpr_admin", nil, nil, "", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete must 204, got %d", resp.StatusCode)
	}
	if resp := adminDo(t, e, http.MethodGet, "/api/den/v1/admin/llm-providers/lpr_admin", nil, nil, "", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted row must 404, got %d", resp.StatusCode)
	}
	var listed any
	e.getJSON(t, "/api/den/v1/llm-providers", &listed)
	if rows := listOf(t, mapOf(t, listed)["llmProviders"]); len(rows) != 0 {
		t.Fatalf("a deleted row must leave the desktop listing, got %#v", rows)
	}

	// The audit names the acting member and every change, newest first.
	var audit any
	if resp := e.getJSON(t, "/api/den/v1/admin/audit", &audit); resp.StatusCode != http.StatusOK {
		t.Fatalf("audit: status %d", resp.StatusCode)
	}
	entries := listOf(t, mapOf(t, audit)["audit"])
	var actions, actors []string
	for _, e := range entries {
		entry := mapOf(t, e)
		actions = append(actions, entry["action"].(string))
		actors = append(actors, entry["actor"].(string))
	}
	if strings.Join(actions, ",") != "delete,update,create" {
		t.Fatalf("audit must be newest-first over all three writes, got %v", actions)
	}
	for _, actor := range actors {
		if actor != aliceSub {
			t.Fatalf("a session write must be audited as the member, got %q", actor)
		}
	}
	if mapOf(t, entries[0])["providerId"] != "lpr_admin" {
		t.Fatalf("audit must name the row: %#v", entries[0])
	}
}

// Writes are validated before they can reach the served set, and a payload
// that still tries to declare models is rejected rather than ignored: menus
// are derived now (ADR-0004), so a stale field must fail loudly.
func TestAdminWriteValidation(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")

	cases := []struct {
		name string
		body map[string]any
		code string
	}{
		{"id without the lpr_ prefix", providerBody("admin", "X", "https://gw.example/v1", "K"), "invalid_row"},
		{"relative baseURL", providerBody("lpr_x", "X", "gw.example:4000", "K"), "invalid_row"},
		{"apiKeyEnv is not an env name", providerBody("lpr_x", "X", "https://gw.example/v1", "not an env"), "invalid_row"},
		{"empty groups list", func() map[string]any {
			m := providerBody("lpr_x", "X", "https://gw.example/v1", "K")
			m["groups"] = []string{}
			return m
		}(), "invalid_row"},
		{"models field is no longer a thing", func() map[string]any {
			m := providerBody("lpr_x", "X", "https://gw.example/v1", "K")
			m["models"] = []any{map[string]any{"id": "gpt-5"}}
			return m
		}(), "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out any
			resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers", tc.body, &out, "", "")
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d body %#v", resp.StatusCode, out)
			}
			if mapOf(t, out)["error"] != tc.code {
				t.Fatalf("error code: got %#v want %q", out, tc.code)
			}
		})
	}

	// None of them reached the store.
	var listed any
	e.getJSON(t, "/api/den/v1/admin/llm-providers", &listed)
	if rows := listOf(t, mapOf(t, listed)["llmProviders"]); len(rows) != 0 {
		t.Fatalf("rejected writes must not persist, got %#v", rows)
	}
}

// The point of the write API: a row written here is what the desktop lists,
// with the menu its own gateway key reports.
func TestAdminWriteReachesDesktopListing(t *testing.T) {
	gw := newFakeLLMGateway(t, map[string][]string{"sk-admin-key": {"gpt-5", "qwen-27b"}})
	t.Setenv("KEY_ADMIN", "sk-admin-key")

	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins", "family")

	body := providerBody("lpr_admin", "Admin Row", gw.baseURL(), "KEY_ADMIN")
	body["groups"] = []string{"family"}
	body["nameOverrides"] = map[string]string{"qwen-27b": "Qwen 27B (local)"}
	var created any
	if resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers", body, &created, "", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d", resp.StatusCode)
	}

	// Visible to a family member, renamed by the override, and no group
	// names leak to the client.
	if got := visibleModels(t, e); strings.Join(got, ",") != "lpr_admin:gpt-5,lpr_admin:qwen-27b" {
		t.Fatalf("written row must be served with its derived menu, got %v", got)
	}
	var listed any
	e.getJSON(t, "/api/den/v1/llm-providers", &listed)
	row := mapOf(t, listOf(t, mapOf(t, listed)["llmProviders"])[0])
	names := map[string]string{}
	for _, m := range listOf(t, row["models"]) {
		model := mapOf(t, m)
		names[model["id"].(string)] = model["name"].(string)
	}
	if names["qwen-27b"] != "Qwen 27B (local)" || names["gpt-5"] != "gpt-5" {
		t.Fatalf("nameOverrides must rename the derived ids: %#v", names)
	}

	// A member outside the audience does not see it, and cannot connect.
	signInAs(t, e, "friends")
	if got := visibleModels(t, e); len(got) != 0 {
		t.Fatalf("row-level groups must gate the written row, got %v", got)
	}
	var hidden any
	if resp := e.getJSON(t, "/api/den/v1/llm-providers/lpr_admin/connect", &hidden); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("hidden row connect must 404, got %d", resp.StatusCode)
	}
}

// Machine automation authenticates with a bat_ token, is audited by token
// name, and stops the moment it is revoked.
func TestAdminAPITokenLifecycle(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")

	var minted any
	resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/api-tokens",
		map[string]any{"name": "ci"}, &minted, "", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint: status %d", resp.StatusCode)
	}
	raw := nonEmptyString(t, mapOf(t, minted), "token")
	if !strings.HasPrefix(raw, store.AdminTokenPrefix) {
		t.Fatalf("admin tokens must carry the %s prefix, got %q", store.AdminTokenPrefix, raw)
	}

	// The token writes a row, and the audit names the token, not the human
	// who minted it.
	var created any
	if resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers",
		providerBody("lpr_ci", "CI Row", "https://gw.example/v1", "KEY_CI"), &created, raw, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("token create: status %d body %#v", resp.StatusCode, created)
	}
	var audit any
	e.getJSON(t, "/api/den/v1/admin/audit", &audit)
	first := mapOf(t, listOf(t, mapOf(t, audit)["audit"])[0])
	if first["actor"] != "ci" || first["action"] != "create" {
		t.Fatalf("audit must name the machine behind the change: %#v", first)
	}

	// Listing shows hashes, never the value that was just shown once.
	var tokens any
	e.getJSON(t, "/api/den/v1/admin/api-tokens", &tokens)
	body, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), raw) {
		t.Fatalf("a listed token must never carry its raw value")
	}
	listedTok := mapOf(t, listOf(t, mapOf(t, tokens)["apiTokens"])[0])
	if listedTok["name"] != "ci" || nonEmptyString(t, listedTok, "hash") == raw {
		t.Fatalf("token listing: %#v", listedTok)
	}
	if listedTok["lastUsedAt"] == nil {
		t.Fatalf("every use must be stamped: %#v", listedTok)
	}

	// Revocation works by raw value or by hash, and after it the token is
	// invalid_token — never a session-terminating code, so automation does
	// not sign a human out by mistake.
	if resp := adminDo(t, e, http.MethodDelete, "/api/den/v1/admin/api-tokens/"+raw, nil, nil, "", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: status %d", resp.StatusCode)
	}
	var revoked any
	resp = adminDo(t, e, http.MethodGet, "/api/den/v1/admin/llm-providers", nil, &revoked, raw, "")
	if resp.StatusCode != http.StatusUnauthorized || mapOf(t, revoked)["error"] != "invalid_token" {
		t.Fatalf("revoked token: status %d body %#v", resp.StatusCode, revoked)
	}

	// A token without the llm-admin scope is refused the same way.
	var other any
	resp = adminDo(t, e, http.MethodPost, "/api/den/v1/admin/api-tokens",
		map[string]any{"name": "read-only", "scopes": []string{"skills"}}, &other, "", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint second token: status %d", resp.StatusCode)
	}
	var refused any
	resp = adminDo(t, e, http.MethodGet, "/api/den/v1/admin/llm-providers", nil, &refused,
		nonEmptyString(t, mapOf(t, other), "token"), "")
	if resp.StatusCode != http.StatusUnauthorized || mapOf(t, refused)["error"] != "invalid_token" {
		t.Fatalf("out-of-scope token: status %d body %#v", resp.StatusCode, refused)
	}
}

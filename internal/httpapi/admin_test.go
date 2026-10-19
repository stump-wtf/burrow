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
		providers.NewRuntime(e.st, e.cfg.ModelsTTL, providers.Unconstrained()), providers.Unconstrained(), nil).Routes()
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
		if actor != "user:"+aliceSub {
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
		// "/models" must land in the path, and a credential in the URL
		// would be served to every member in providerConfig.
		{"baseURL with a query string", providerBody("lpr_x", "X", "https://gw.example/v1?x=", "K"), "invalid_row"},
		{"baseURL with a fragment", providerBody("lpr_x", "X", "https://gw.example/v1#x", "K"), "invalid_row"},
		{"baseURL with userinfo", providerBody("lpr_x", "X", "https://user:pass@gw.example/v1", "K"), "invalid_row"},
		// burrow's own configuration, secrets included, must never be
		// nameable as a key: /connect would hand it to members and the menu
		// fetch would send it to whatever baseURL the writer chose.
		{"apiKeyEnv names burrow's database URL", providerBody("lpr_x", "X", "https://gw.example/v1", "BURROW_DATABASE_URL"), "invalid_row"},
		{"apiKeyEnv names burrow's OIDC secret", providerBody("lpr_x", "X", "https://gw.example/v1", "BURROW_OIDC_CLIENT_SECRET"), "invalid_row"},
		// The namespace is reserved in any letter case: the check folds
		// the name first, and only upper-case spellings were covered.
		{"apiKeyEnv names burrow's namespace in lower case", providerBody("lpr_x", "X", "https://gw.example/v1", "burrow_oidc_client_secret"), "invalid_row"},
		{"empty groups list", func() map[string]any {
			m := providerBody("lpr_x", "X", "https://gw.example/v1", "K")
			m["groups"] = []string{}
			return m
		}(), "invalid_row"},
		// " family" matches no member: stored, it is a row served to nobody.
		{"group name with surrounding whitespace", func() map[string]any {
			m := providerBody("lpr_x", "X", "https://gw.example/v1", "K")
			m["groups"] = []string{" family"}
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

// A row id is a path segment — /connect on the desktop surface, the admin
// routes, the console's links — so an id that needs escaping, or that reads
// as another route, is refused at the write rather than stored and found
// unaddressable later (SPEC-0004 REQ "Rows in the store").
func TestAdminRejectsUnaddressableRowIDs(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")

	for name, id := range map[string]string{
		"slash":                 "lpr_a/b",
		"bare prefix":           "lpr_",
		"space and newline":     "lpr_x y\n",
		"dot segments":          "lpr_../admin",
		"console action suffix": "lpr_a/delete",
		"100 KB":                "lpr_" + strings.Repeat("a", 100<<10),
	} {
		t.Run(name, func(t *testing.T) {
			var out any
			resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers",
				providerBody(id, "X", "https://gw.example/v1", "K"), &out, "", "")
			if resp.StatusCode != http.StatusBadRequest || mapOf(t, out)["error"] != "invalid_row" {
				t.Fatalf("id %.40q: status %d, want 400 invalid_row", id, resp.StatusCode)
			}
		})
	}

	var listed any
	e.getJSON(t, "/api/den/v1/admin/llm-providers", &listed)
	if rows := listOf(t, mapOf(t, listed)["llmProviders"]); len(rows) != 0 {
		t.Fatalf("rejected ids must not persist, got %d rows", len(rows))
	}
}

// The id rule binds writes, not rows already stored: a row an older build
// accepted must still be read and deleted, or an operator could never clean
// it up.
func TestAdminOddStoredRowIDStaysReadableAndDeletable(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")
	seedProviders(t, e, store.ProviderRow{ID: "lpr_a/b", BaseURL: "https://gw.example/v1", APIKeyEnv: "K"})

	const path = "/api/den/v1/admin/llm-providers/lpr_a%2Fb"
	var got any
	if resp := adminDo(t, e, http.MethodGet, path, nil, &got, "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("a stored odd id must still be readable, got %d", resp.StatusCode)
	}
	if adminRow(t, got)["id"] != "lpr_a/b" {
		t.Fatalf("read row: %#v", got)
	}
	if resp := adminDo(t, e, http.MethodDelete, path, nil, nil, "", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("a stored odd id must still be deletable, got %d", resp.StatusCode)
	}
	if resp := adminDo(t, e, http.MethodGet, path, nil, nil, "", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted row must 404, got %d", resp.StatusCode)
	}
}

// A control character is the writer's mistake, so it is a 400 naming the
// field like any other invalid row — never a value handed to Postgres, which
// refuses a NUL in TEXT and used to surface that as "500 store unavailable".
func TestAdminControlCharactersAre400(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")

	cases := map[string]func(map[string]any){
		"NUL in name":        func(m map[string]any) { m["name"] = "Admin\x00Row" },
		"NUL in providerId":  func(m map[string]any) { m["providerId"] = "lite\x00llm" },
		"newline in name":    func(m map[string]any) { m["name"] = "Admin\nRow" },
		"newline in a group": func(m map[string]any) { m["groups"] = []string{"fam\nily"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			body := providerBody("lpr_x", "X", "https://gw.example/v1", "K")
			mutate(body)
			var out any
			resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers", body, &out, "", "")
			if resp.StatusCode != http.StatusBadRequest || mapOf(t, out)["error"] != "invalid_row" {
				t.Fatalf("status %d body %#v, want 400 invalid_row", resp.StatusCode, out)
			}
		})
	}

	var listed any
	e.getJSON(t, "/api/den/v1/admin/llm-providers", &listed)
	if rows := listOf(t, mapOf(t, listed)["llmProviders"]); len(rows) != 0 {
		t.Fatalf("rejected writes must not persist, got %#v", rows)
	}
}

// One request is one JSON object. Anything after it — a second object, a
// stray token — means the payload is not what its author thinks it is, and
// accepting the first value would be the silent drop that rejecting unknown
// fields exists to prevent.
func TestAdminRejectsTrailingJSON(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")

	post := func(t *testing.T, body string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/den/v1/admin/llm-providers", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e.token)
		resp, err := e.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return resp.StatusCode, mapOf(t, out)
	}
	row := func(id string) string {
		return `{"id":"` + id + `","providerId":"litellm","name":"X","baseURL":"https://gw.example/v1","apiKeyEnv":"K"}`
	}

	for name, body := range map[string]string{
		"a second object":     row("lpr_first") + row("lpr_second"),
		"an unknown field":    row("lpr_first") + ` {"models":[]}`,
		"garbage":             row("lpr_first") + ` garbage`,
		"a closing delimiter": row("lpr_first") + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			status, out := post(t, body)
			if status != http.StatusBadRequest || out["error"] != "invalid_request" {
				t.Fatalf("status %d body %#v, want 400 invalid_request", status, out)
			}
		})
	}
	var listed any
	e.getJSON(t, "/api/den/v1/admin/llm-providers", &listed)
	if rows := listOf(t, mapOf(t, listed)["llmProviders"]); len(rows) != 0 {
		t.Fatalf("rejected writes must not persist, got %#v", rows)
	}

	// Trailing whitespace is not trailing data: `curl -d @row.json` sends
	// the file's final newline.
	if status, out := post(t, row("lpr_first")+"\n \t\n"); status != http.StatusCreated {
		t.Fatalf("a trailing newline must still be accepted, got %d %#v", status, out)
	}
}

// PUT is a write like POST and goes through the same gate: a valid row must
// not be replaceable by one that names burrow's own configuration as its key.
// TestAdminWriteValidation only POSTs, so it stays green with the PUT
// handler's validation removed.
func TestAdminUpdateValidatesTheReplacement(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")

	var created any
	if resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers",
		providerBody("lpr_admin", "Admin Row", "https://gw.example/v1", "KEY_ADMIN"), &created, "", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d", resp.StatusCode)
	}
	etag := adminRow(t, created)["etag"].(string)

	var refused any
	resp := adminDo(t, e, http.MethodPut, "/api/den/v1/admin/llm-providers/lpr_admin",
		providerBody("lpr_admin", "Admin Row", "https://gw.example/v1", "BURROW_DATABASE_URL"), &refused, "", etag)
	if resp.StatusCode != http.StatusBadRequest || mapOf(t, refused)["error"] != "invalid_row" {
		t.Fatalf("PUT naming burrow's database URL as the key: status %d body %#v, want 400 invalid_row", resp.StatusCode, refused)
	}

	var after any
	adminDo(t, e, http.MethodGet, "/api/den/v1/admin/llm-providers/lpr_admin", nil, &after, "", "")
	if row := adminRow(t, after); row["apiKeyEnv"] != "KEY_ADMIN" || row["etag"] != etag {
		t.Fatalf("a refused PUT must leave the row untouched: %#v", row)
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
// identity, and stops the moment it is revoked.
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

	// The token writes a row, and the audit names the token by its own hash
	// beside the member whose admin rights it borrows, so the row cannot be
	// mistaken for that member's own.
	var created any
	if resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers",
		providerBody("lpr_ci", "CI Row", "https://gw.example/v1", "KEY_CI"), &created, raw, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("token create: status %d body %#v", resp.StatusCode, created)
	}
	var audit any
	e.getJSON(t, "/api/den/v1/admin/audit", &audit)
	first := mapOf(t, listOf(t, mapOf(t, audit)["audit"])[0])
	hash := nonEmptyString(t, mapOf(t, mapOf(t, minted)["apiToken"]), "hash")
	actor, _ := first["actor"].(string)
	if first["action"] != "create" || !strings.HasPrefix(actor, "token:ci#") ||
		!strings.Contains(actor, hash[:12]) || !strings.HasSuffix(actor, "@user:"+aliceSub) {
		t.Fatalf("audit must name the machine behind the change, not a bare name: %#v", first)
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

// DELETE honors If-Match when the caller sends one: a stale etag is 412 with
// the current row (the row survives), a malformed one is 400, and a fresh
// one deletes. Without the header DELETE stays unconditional, which is what
// automation that only knows the id relies on.
func TestAdminDeleteHonorsIfMatch(t *testing.T) {
	e := newAdminTestEnv(t)
	signInAs(t, e, "stumpcloud-admins")
	const path = "/api/den/v1/admin/llm-providers/lpr_del"

	var created any
	if resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers",
		providerBody("lpr_del", "Scratch", "https://gw.example/v1", "KEY_DEL"), &created, "", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d", resp.StatusCode)
	}
	shown := adminRow(t, created)["etag"].(string)

	// Another writer replaces the row after the etag was read.
	var updated any
	if resp := adminDo(t, e, http.MethodPut, path,
		providerBody("lpr_del", "Production", "https://gw.example/v1", "KEY_DEL"), &updated, "", shown); resp.StatusCode != http.StatusOK {
		t.Fatalf("update: status %d", resp.StatusCode)
	}
	current := adminRow(t, updated)["etag"].(string)

	resp := adminDo(t, e, http.MethodDelete, path, nil, nil, "", shown)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("DELETE with a stale If-Match must 412, got %d", resp.StatusCode)
	}
	var stale any
	if err := json.NewDecoder(resp.Body).Decode(&stale); err != nil {
		t.Fatalf("412 body: %v", err)
	}
	if row := adminRow(t, stale); row["name"] != "Production" || row["etag"] != current {
		t.Fatalf("412 body must carry the current row: %#v", row)
	}
	if resp := adminDo(t, e, http.MethodGet, path, nil, nil, "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("a refused delete must leave the row, got %d", resp.StatusCode)
	}

	if resp := adminDo(t, e, http.MethodDelete, path, nil, nil, "", `"not-an-etag"`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("DELETE with a malformed If-Match must 400, got %d", resp.StatusCode)
	}
	if resp := adminDo(t, e, http.MethodGet, path, nil, nil, "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("a malformed precondition must leave the row, got %d", resp.StatusCode)
	}

	if resp := adminDo(t, e, http.MethodDelete, path, nil, nil, "", current); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE with a fresh If-Match must 204, got %d", resp.StatusCode)
	}
	if resp := adminDo(t, e, http.MethodGet, path, nil, nil, "", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted row must 404, got %d", resp.StatusCode)
	}

	// No header: unconditional, as before.
	if resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers",
		providerBody("lpr_del", "Scratch", "https://gw.example/v1", "KEY_DEL"), nil, "", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("recreate: status %d", resp.StatusCode)
	}
	if resp := adminDo(t, e, http.MethodDelete, path, nil, nil, "", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE without If-Match must still 204, got %d", resp.StatusCode)
	}

	// Only the two deletes that happened are audited; a refused one is not.
	var audit any
	e.getJSON(t, "/api/den/v1/admin/audit", &audit)
	deletes := 0
	for _, entry := range listOf(t, mapOf(t, audit)["audit"]) {
		if mapOf(t, entry)["action"] == "delete" {
			deletes++
		}
	}
	if deletes != 2 {
		t.Fatalf("audit must record exactly the two deletes that landed, got %d", deletes)
	}
}

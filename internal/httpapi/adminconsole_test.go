package httpapi_test

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/httpapi"
	"gitea.stump.rocks/stump.wtf/burrow/internal/oidc"
	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

const adminGroup = "stumpcloud-admins"

// newConsoleEnv builds a server whose providers runtime is live and whose
// admin gate admits the adminGroup, with alice holding it.
func newConsoleEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	e.cfg.AdminGroups = []string{adminGroup}
	if err := e.st.UpsertUser(context.Background(), store.User{
		ID: aliceSub, Email: aliceEmail, Name: "Alice", Groups: []string{adminGroup},
	}); err != nil {
		t.Fatalf("seed admin member: %v", err)
	}
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st,
		skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}),
		providers.NewRuntime(e.st, time.Minute), nil).Routes()
	return e
}

// consoleGet performs a browser GET with the session cookie.
func (e *testEnv) consoleGet(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "burrow_session", Value: e.token})
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

// consolePost performs a browser form POST with the session cookie.
func (e *testEnv) consolePost(t *testing.T, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "burrow_session", Value: e.token})
	client := e.srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer func() { client.CheckRedirect = nil }()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func seedConsoleRow(t *testing.T, e *testEnv, row store.ProviderRow) {
	t.Helper()
	if err := e.st.CreateProvider(context.Background(), row, "seed"); err != nil {
		t.Fatalf("seed provider %s: %v", row.ID, err)
	}
}

func mustContain(t *testing.T, page, want string) {
	t.Helper()
	if !strings.Contains(page, want) {
		t.Fatalf("page must contain %q:\n%s", want, page)
	}
}

func mustNotContain(t *testing.T, page, want string) {
	t.Helper()
	if strings.Contains(page, want) {
		t.Fatalf("page must not contain %q:\n%s", want, page)
	}
}

// consoleBodyOf returns the page below <head>, so an assertion about what a
// page shows cannot be satisfied by its <title>.
func consoleBodyOf(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, "</head>")
	if i < 0 {
		t.Fatalf("page has no </head>:\n%s", page)
	}
	return page[i:]
}

// The console is gated like the API: no session redirects to sign-in, a
// member outside the admin groups gets a 403 page, and an unset
// BURROW_ADMIN_GROUPS closes the surface to everyone.
func TestConsoleGate(t *testing.T) {
	e := newConsoleEnv(t)

	noRedirect := e.srv.Client()
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	noCookie, err := noRedirect.Get(e.srv.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	noCookie.Body.Close()
	if noCookie.StatusCode != http.StatusFound {
		t.Fatalf("no cookie must redirect to sign-in, got %d", noCookie.StatusCode)
	}
	if loc := noCookie.Header.Get("Location"); loc != "/?mode=sign-in" {
		t.Fatalf("redirect location %q", loc)
	}

	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/admin", nil)
	req.AddCookie(&http.Cookie{Name: "burrow_session", Value: "bogus"})
	r2, _ := e.srv.Client().Do(req)
	r2.Body.Close()
	if r2.StatusCode != http.StatusFound {
		t.Fatalf("unknown cookie must redirect to sign-in, got %d", r2.StatusCode)
	}

	// Alice holds the group: the list page renders.
	resp, page := e.consoleGet(t, "/admin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin list status %d", resp.StatusCode)
	}
	// The body, not the page: the <title> says "LLM providers" too, and a
	// page that rendered nothing below its navigation used to pass here.
	mustContain(t, consoleBodyOf(t, page), ">LLM providers</h1>")

	// A member outside the admin groups gets the API's 403.
	if err := e.st.UpsertUser(context.Background(), store.User{
		ID: aliceSub, Email: aliceEmail, Name: "Alice", Groups: []string{"eng"},
	}); err != nil {
		t.Fatal(err)
	}
	resp, page = e.consoleGet(t, "/admin")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin status %d", resp.StatusCode)
	}
	mustContain(t, page, "admin_required")

	// With no groups configured the surface is closed even to an admin.
	e.cfg.AdminGroups = nil
	resp, page = e.consoleGet(t, "/admin")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("closed surface status %d", resp.StatusCode)
	}
	mustContain(t, page, "admin_required")
}

// A web (non-desktop) OIDC sign-in establishes the browser session the
// console reads; the desktop flow still does not.
func TestConsoleOIDCCallbackSetsSessionCookie(t *testing.T) {
	e := newConsoleEnv(t)
	// newConsoleEnv replaced the handler; rebuild it against the fake issuer.
	issuer := newFakeIssuer(t, "burrow-desktop")
	issuer.groups = []string{adminGroup}
	client := newOIDCClientFor(t, e, issuer)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	start, err := client.Get(e.srv.URL + "/auth/oidc/start")
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	if start.StatusCode != http.StatusFound {
		t.Fatalf("start status %d", start.StatusCode)
	}
	loc, _ := url.Parse(start.Header.Get("Location"))
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("start must carry state")
	}
	client.CheckRedirect = nil

	resp, err := client.Get(e.srv.URL + "/auth/oidc/callback?state=" + url.QueryEscape(state) + "&code=test-code")
	if err != nil {
		t.Fatal(err)
	}
	pageBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(pageBytes)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status %d: %s", resp.StatusCode, page)
	}
	var sessionCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "burrow_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("web callback must set the burrow_session cookie")
	}
	if !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie must be HttpOnly and SameSite=Lax, got %+v", sessionCookie)
	}
	mustContain(t, page, "Open the admin console")

	// The cookie opens the console.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/admin", nil)
	req.AddCookie(sessionCookie)
	list, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(list.Body)
	list.Body.Close()
	if list.StatusCode != http.StatusOK {
		t.Fatalf("console via callback cookie status %d: %s", list.StatusCode, body)
	}

	// The desktop flow must not set the cookie.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	start, err = client.Get(e.srv.URL + "/auth/oidc/start?desktop=1")
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	loc, _ = url.Parse(start.Header.Get("Location"))
	client.CheckRedirect = nil
	resp, err = client.Get(e.srv.URL + "/auth/oidc/callback?state=" + url.QueryEscape(loc.Query().Get("state")) + "&code=test-code")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "burrow_session" {
			t.Fatal("desktop callback must not set the burrow_session cookie")
		}
	}
}

// newOIDCClientFor points the console env's server at a fake issuer and
// returns its HTTP client.
func newOIDCClientFor(t *testing.T, e *testEnv, issuer *fakeIssuer) *http.Client {
	t.Helper()
	client, err := oidc.New(context.Background(), issuer.srv.URL, "burrow-desktop", "secret",
		e.srv.URL+"/auth/oidc/callback", false)
	if err != nil {
		t.Fatalf("oidc client against fake issuer: %v", err)
	}
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st,
		skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}),
		providers.NewRuntime(e.st, time.Minute), client).Routes()
	return e.srv.Client()
}

// The list, editor, create, update, delete, audit, and preview surfaces,
// with every write flowing through the write API's own handlers.
func TestConsoleProviderLifecycle(t *testing.T) {
	e := newConsoleEnv(t)
	t.Setenv("LLM_CONSOLE_KEY", "test-key-value")

	seedConsoleRow(t, e, store.ProviderRow{
		ID: "lpr_family", ProviderID: "litellm", Name: "Family", BaseURL: "http://127.0.0.1:1/v1",
		APIKeyEnv: "LLM_CONSOLE_KEY", Groups: nil, NameOverrides: map[string]string{"m1": "Model One"},
	})
	seedConsoleRow(t, e, store.ProviderRow{
		ID: "lpr_eng", ProviderID: "litellm", Name: "Eng", BaseURL: "http://127.0.0.1:1/v1",
		APIKeyEnv: "LLM_ENG_KEY", Groups: []string{"eng"}, NameOverrides: map[string]string{},
	})

	// List: names, key lights, audience, model count.
	_, page := e.consoleGet(t, "/admin")
	mustContain(t, page, "Family")
	mustContain(t, page, "everyone (org-wide)")
	mustContain(t, page, "LLM_CONSOLE_KEY set")
	mustContain(t, page, "LLM_ENG_KEY unset")
	mustContain(t, page, `<span class="badge">eng</span>`)
	mustContain(t, page, "Add a provider")
	mustContain(t, page, "Visibility preview")

	// Editor for an existing row: etag rides in the form; a null audience
	// warns; the apiKeyEnv name, never a value, is shown.
	_, page = e.consoleGet(t, "/admin/providers/lpr_family")
	mustContain(t, page, `name="etag"`)
	mustContain(t, page, "visible org-wide")
	mustContain(t, page, "LLM_CONSOLE_KEY")
	mustNotContain(t, page, "test-key-value")
	mustContain(t, page, `value="Model One"`)
	mustContain(t, page, `href="/admin/providers/lpr_family/delete"`) // delete goes via the confirm step

	// New row form.
	_, page = e.consoleGet(t, "/admin/providers/new")
	mustContain(t, page, `action="/admin/providers"`)
	mustContain(t, page, `name="id"`)

	// Create through the console → the write API → the store + audit.
	form := url.Values{}
	form.Set("id", "lpr_console")
	form.Set("providerId", "litellm")
	form.Set("name", "Console")
	form.Set("baseURL", "http://127.0.0.1:1/v1")
	form.Set("apiKeyEnv", "LLM_CONSOLE_NEW_KEY")
	form.Set("groups", "eng\r\nqa") // one per line, as a browser submits a textarea
	resp, page := e.consolePost(t, "/admin/providers", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create status %d: %s", resp.StatusCode, page)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin?saved=1" {
		t.Fatalf("create redirect %q", loc)
	}
	created, err := e.st.GetProvider(context.Background(), "lpr_console")
	if err != nil {
		t.Fatalf("created row: %v", err)
	}
	if len(created.Groups) != 2 || created.Groups[0] != "eng" || created.Groups[1] != "qa" {
		t.Fatalf("created groups %v", created.Groups)
	}
	audit := consoleAuditOf(t, e)
	if audit[0].Actor != aliceSub || audit[0].Action != "create" || audit[0].ProviderID != "lpr_console" {
		t.Fatalf("create audit %+v", audit[0])
	}

	// Duplicate id surfaces the API's 409.
	form.Set("name", "Console again")
	_, page = e.consolePost(t, "/admin/providers", form)
	mustContain(t, page, "duplicate_id")
	mustContain(t, page, "Console again") // typed values survive the error

	// An invalid row surfaces the API's 400 with its message.
	form.Set("id", "nope")
	_, page = e.consolePost(t, "/admin/providers", form)
	mustContain(t, page, "invalid_row")

	// Update: fetch the etag from the editor, save, see the new name.
	_, page = e.consoleGet(t, "/admin/providers/lpr_family")
	etag := consoleEtagOf(t, page)
	form = url.Values{}
	form.Set("providerId", "litellm")
	form.Set("name", "Family v2")
	form.Set("baseURL", "http://127.0.0.1:1/v1")
	form.Set("apiKeyEnv", "LLM_CONSOLE_KEY")
	form.Set("etag", etag)
	resp, page = e.consolePost(t, "/admin/providers/lpr_family", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("update status %d: %s", resp.StatusCode, page)
	}
	_, page = e.consoleGet(t, "/admin/providers/lpr_family")
	mustContain(t, page, "Family v2")

	// A stale etag is a 412: the page carries the API's current row.
	form.Set("name", "Family v3")
	resp, page = e.consolePost(t, "/admin/providers/lpr_family", form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stale update status %d: %s", resp.StatusCode, page)
	}
	mustContain(t, page, "changed while you were editing")
	mustContain(t, page, "Family v2") // the CURRENT row, not the typed v3

	// Delete through the console: the confirm step names the row, its model
	// count, and offers a cancel; the confirm POST carries the shown etag.
	_, page = e.consoleGet(t, "/admin/providers/lpr_console/delete")
	mustContain(t, page, "Delete Console?")
	mustContain(t, page, "lpr_console")
	mustContain(t, page, "model count unknown") // LLM_CONSOLE_NEW_KEY is unset: no gateway was asked
	mustContain(t, page, "Cancel")
	form = url.Values{}
	form.Set("etag", consoleEtagOf(t, page))
	resp, page = e.consolePost(t, "/admin/providers/lpr_console/delete", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status %d: %s", resp.StatusCode, page)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin?saved=deleted" {
		t.Fatalf("delete redirect %q", loc)
	}
	if _, err := e.st.GetProvider(context.Background(), "lpr_console"); err != store.ErrNotFound {
		t.Fatalf("deleted row must be gone, got %v", err)
	}

	// The audit view names the actor and the actions.
	_, page = e.consoleGet(t, "/admin/audit")
	mustContain(t, page, aliceSub)
	mustContain(t, page, "delete")
	mustContain(t, page, "lpr_family")
	mustNotContain(t, page, "test-key-value") // secrets never render
}

// The preview answers exactly what the runtime would serve a member of the
// picked group: null-audience rows plus that group's rows, nothing else.
func TestConsoleVisibilityPreview(t *testing.T) {
	e := newConsoleEnv(t)
	seedConsoleRow(t, e, store.ProviderRow{
		ID: "lpr_family", ProviderID: "litellm", Name: "Family", BaseURL: "http://127.0.0.1:1/v1",
		APIKeyEnv: "LLM_CONSOLE_KEY", Groups: nil, NameOverrides: map[string]string{},
	})
	seedConsoleRow(t, e, store.ProviderRow{
		ID: "lpr_eng", ProviderID: "litellm", Name: "Eng", BaseURL: "http://127.0.0.1:1/v1",
		APIKeyEnv: "LLM_CONSOLE_KEY", Groups: []string{"eng"}, NameOverrides: map[string]string{},
	})
	seedConsoleRow(t, e, store.ProviderRow{
		ID: "lpr_qa", ProviderID: "litellm", Name: "QA", BaseURL: "http://127.0.0.1:1/v1",
		APIKeyEnv: "LLM_CONSOLE_KEY", Groups: []string{"qa"}, NameOverrides: map[string]string{},
	})

	_, page := e.consoleGet(t, "/admin/preview?group=eng")
	mustContain(t, page, "Visibility preview")
	mustContain(t, page, "Family") // null audience: everyone
	mustContain(t, page, "Eng")
	mustNotContain(t, page, "QA")

	_, page = e.consoleGet(t, "/admin/preview?group=qa")
	mustContain(t, page, "Family")
	mustContain(t, page, "QA")
	mustNotContain(t, page, "Eng")
}

func consoleEtagOf(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `name="etag" value="`)
	if i < 0 {
		t.Fatalf("editor page has no etag field:\n%s", page)
	}
	rest := page[i+len(`name="etag" value="`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatal("etag value is unterminated")
	}
	return strings.ReplaceAll(rest[:end], "&#34;", `"`)
}

// consoleFieldOf returns the value a browser would submit for the editor's
// field name, whether it is an <input> or a <textarea>.
func consoleFieldOf(t *testing.T, page, name string) string {
	t.Helper()
	i := strings.Index(page, ` name="`+name+`"`)
	if i < 0 {
		t.Fatalf("editor page has no %s field:\n%s", name, page)
	}
	open := strings.LastIndex(page[:i], "<")
	rest := page[i:]
	if strings.HasPrefix(page[open:], "<textarea") {
		start := strings.Index(rest, ">")
		end := strings.Index(rest, "</textarea>")
		if start < 0 || end < start {
			t.Fatalf("%s textarea is unterminated", name)
		}
		// A browser submits a textarea's lines CRLF-separated.
		return strings.ReplaceAll(html.UnescapeString(rest[start+1:end]), "\n", "\r\n")
	}
	const attr = ` value="`
	start := strings.Index(rest, attr)
	if start < 0 {
		t.Fatalf("%s input has no value", name)
	}
	rest = rest[start+len(attr):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("%s value is unterminated", name)
	}
	return html.UnescapeString(rest[:end])
}

func consoleAuditOf(t *testing.T, e *testEnv) []store.AuditEntry {
	t.Helper()
	entries, err := e.st.ListAudit(context.Background(), 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected audit entries")
	}
	return entries
}

// Every console page says what it is and offers its way forward even when it
// has nothing to list: an empty store must still offer "Add a provider", the
// preview must show its picker before a group is picked, and a view with no
// rows says so rather than rendering a bare navigation bar.
func TestConsoleEmptyStates(t *testing.T) {
	e := newConsoleEnv(t)

	// No provider rows at all.
	_, page := e.consoleGet(t, "/admin")
	body := consoleBodyOf(t, page)
	mustContain(t, body, ">LLM providers</h1>")
	mustContain(t, body, `href="/admin/providers/new"`)
	mustContain(t, body, `href="/admin/preview"`)
	mustContain(t, body, "No providers yet")

	// No audit entries.
	_, page = e.consoleGet(t, "/admin/audit")
	body = consoleBodyOf(t, page)
	mustContain(t, body, "Audit — recent provider changes</h1>")
	mustContain(t, body, "No provider changes recorded yet")

	// The preview with no row naming a group: nothing to pick, said plainly.
	_, page = e.consoleGet(t, "/admin/preview")
	body = consoleBodyOf(t, page)
	mustContain(t, body, ">Visibility preview</h1>")
	mustContain(t, body, "No provider names a group yet")

	seedConsoleRow(t, e, store.ProviderRow{
		ID: "lpr_eng", ProviderID: "litellm", Name: "Eng", BaseURL: "http://127.0.0.1:1/v1",
		APIKeyEnv: "LLM_CONSOLE_KEY", Groups: []string{"eng"}, NameOverrides: map[string]string{},
	})

	// The preview as its only link reaches it — no group picked yet — must
	// render the picker, or there is no way to pick one.
	_, page = e.consoleGet(t, "/admin/preview")
	body = consoleBodyOf(t, page)
	mustContain(t, body, ">Visibility preview</h1>")
	mustContain(t, body, `<select id="group" name="group"`)
	mustContain(t, body, `<option value="eng"`)
	mustNotContain(t, body, "<table")

	// A group that sees nothing gets the picker and an explicit answer.
	_, page = e.consoleGet(t, "/admin/preview?group=qa")
	body = consoleBodyOf(t, page)
	mustContain(t, body, `<select id="group" name="group"`)
	mustContain(t, body, "would get no providers")
	mustNotContain(t, body, "<table")
}

// A model count the console could not establish is not a count of zero. Only
// a gateway that answered may put a number on the list or on the delete
// confirm; a refused or unreachable gateway, its retry cooldown, and an
// unset key variable all read as unknown.
func TestConsoleModelCountUnknown(t *testing.T) {
	gw := newFakeLLMGateway(t, map[string][]string{
		"sk-two":   {"m1", "m2"},
		"sk-empty": {},
	})
	t.Setenv("LLM_COUNT_TWO", "sk-two")
	t.Setenv("LLM_COUNT_EMPTY", "sk-empty")
	t.Setenv("LLM_COUNT_REFUSED", "sk-refused") // the gateway answers 401
	e := newConsoleEnv(t)
	for id, env := range map[string]string{
		"lpr_two": "LLM_COUNT_TWO", "lpr_empty": "LLM_COUNT_EMPTY",
		"lpr_refused": "LLM_COUNT_REFUSED", "lpr_nokey": "LLM_COUNT_UNSET",
	} {
		seedConsoleRow(t, e, store.ProviderRow{
			ID: id, ProviderID: "litellm", Name: id, BaseURL: gw.baseURL(),
			APIKeyEnv: env, NameOverrides: map[string]string{},
		})
	}
	const unknown = "model count unknown (gateway unreachable or key unset)"

	_, page := e.consoleGet(t, "/admin/providers/lpr_two/delete")
	mustContain(t, page, "its 2 derived models")
	mustNotContain(t, page, unknown)

	// The gateway answered with an empty menu: zero is the truth here.
	_, page = e.consoleGet(t, "/admin/providers/lpr_empty/delete")
	mustContain(t, page, "its 0 derived models")
	mustNotContain(t, page, unknown)

	// Twice: the first request fails the fetch, the second is inside the
	// failure cooldown and never reaches the gateway.
	for _, when := range []string{"failed fetch", "failure cooldown"} {
		_, page = e.consoleGet(t, "/admin/providers/lpr_refused/delete")
		if !strings.Contains(page, unknown) || strings.Contains(page, "0 derived model") {
			t.Fatalf("%s: the confirm page must say the count is unknown, not zero:\n%s", when, consoleBodyOf(t, page))
		}
	}

	_, page = e.consoleGet(t, "/admin/providers/lpr_nokey/delete")
	mustContain(t, page, unknown)
	mustNotContain(t, page, "0 derived model")

	// The list and the preview show the same distinction per row.
	for _, path := range []string{"/admin", "/admin/preview?group=anyone"} {
		_, page = e.consoleGet(t, path)
		mustContain(t, page, "<td>2</td>")
		mustContain(t, page, "<td>0</td>")
		if got := strings.Count(page, unknown); got != 2 {
			t.Fatalf("%s: two rows have an unknown count, page says so %d times:\n%s", path, got, consoleBodyOf(t, page))
		}
	}
}

// After a save the API rejects, the re-rendered editor still describes the
// stored row truthfully: a set key is not reported unset, and the org-wide
// warning does not vanish. A save that lost its etag is a form error telling
// the admin to reload, not a server error.
func TestConsoleEditorAfterFailedSave(t *testing.T) {
	e := newConsoleEnv(t)
	t.Setenv("LLM_CONSOLE_KEY", "test-key-value")
	seedConsoleRow(t, e, store.ProviderRow{
		ID: "lpr_family", ProviderID: "litellm", Name: "Family", BaseURL: "http://127.0.0.1:1/v1",
		APIKeyEnv: "LLM_CONSOLE_KEY", Groups: nil, NameOverrides: map[string]string{},
	})

	_, page := e.consoleGet(t, "/admin/providers/lpr_family")
	mustContain(t, page, "Currently set.")
	mustContain(t, page, "visible org-wide")

	form := url.Values{}
	form.Set("providerId", "litellm")
	form.Set("name", "Family typed")
	form.Set("baseURL", "not a url")
	form.Set("apiKeyEnv", "LLM_CONSOLE_KEY")
	form.Set("etag", consoleEtagOf(t, page))
	resp, page := e.consolePost(t, "/admin/providers/lpr_family", form)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid save status %d: %s", resp.StatusCode, page)
	}
	mustContain(t, page, "invalid_row")
	mustContain(t, page, "Family typed") // typed values survive
	mustContain(t, page, "Currently set.")
	mustNotContain(t, page, "Currently unset")
	mustContain(t, page, "visible org-wide")

	// The API's 428 (no If-Match) is the form having lost its etag.
	form.Set("baseURL", "http://127.0.0.1:1/v1")
	form.Del("etag")
	resp, page = e.consolePost(t, "/admin/providers/lpr_family", form)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("save without an etag: status %d: %s", resp.StatusCode, consoleBodyOf(t, page))
	}
	mustContain(t, page, "reload the editor")
	mustContain(t, page, "Family typed")
	if got, err := e.st.GetProvider(context.Background(), "lpr_family"); err != nil || got.Name != "Family" {
		t.Fatalf("a save without an etag must not write, got %q, %v", got.Name, err)
	}
}

// Opening a row and pressing Save without touching anything must not change
// it. A group the API accepts may contain a comma — an LDAP DN is the usual
// case — and must come back out of the form as the one group it is.
func TestConsoleSaveKeepsGroupContainingComma(t *testing.T) {
	e := newConsoleEnv(t)
	ctx := context.Background()
	groups := []string{"cn=llm-users,ou=groups,dc=example,dc=com", "eng"}
	seedConsoleRow(t, e, store.ProviderRow{
		ID: "lpr_dn", ProviderID: "litellm", Name: "DN", BaseURL: "http://127.0.0.1:1/v1",
		APIKeyEnv: "LLM_CONSOLE_KEY", Groups: groups, NameOverrides: map[string]string{},
	})

	_, page := e.consoleGet(t, "/admin/providers/lpr_dn")
	form := url.Values{}
	for _, field := range []string{"providerId", "name", "baseURL", "apiKeyEnv", "groups", "etag"} {
		form.Set(field, consoleFieldOf(t, page, field))
	}
	resp, page := e.consolePost(t, "/admin/providers/lpr_dn", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("untouched save status %d: %s", resp.StatusCode, page)
	}
	saved, err := e.st.GetProvider(ctx, "lpr_dn")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(saved.Groups, "|") != strings.Join(groups, "|") {
		t.Fatalf("an untouched save must keep the audience %q, got %q", groups, saved.Groups)
	}
}

package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// consoleDo performs one browser-shaped request against the console: an
// optional form body, the session cookie when cookie is non-empty, and the
// request headers a browser would attach (Origin, Sec-Fetch-Site). Redirects
// are returned, not followed.
func (e *testEnv) consoleDo(t *testing.T, method, path string, form url.Values, cookie string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "burrow_session", Value: cookie})
	}
	client := e.srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer func() { client.CheckRedirect = nil }()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(page)
}

// The session cookie is SameSite=Lax, and SameSite is scoped to the SITE, not
// the origin: a form on any sibling subdomain of burrow's registrable domain
// is same-site, so the browser attaches the cookie to its POST. The console
// must therefore refuse a non-safe request a browser marks as coming from
// another origin, whatever cookie rides along. The forged create is the one
// that matters most: a row naming an environment variable burrow already
// holds, with a baseURL the attacker controls, makes burrow send that key as
// a bearer the next time the list page derives the row's menu.
func TestConsoleRefusesCrossOriginPosts(t *testing.T) {
	e := newConsoleEnv(t)
	t.Setenv("LLM_FORGED_KEY", "forged-test-key")

	var gatewayHits, keyLeaks atomic.Int64
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayHits.Add(1)
		if r.Header.Get("Authorization") == "Bearer forged-test-key" {
			keyLeaks.Add(1)
		}
		writeJSONString(w, `{"data":[{"id":"m1"}]}`)
	}))
	t.Cleanup(attacker.Close)

	family := store.ProviderRow{
		ID: "lpr_family", ProviderID: "litellm", Name: "Family", BaseURL: "http://127.0.0.1:1/v1",
		APIKeyEnv: "LLM_UNSET_KEY", Groups: nil, NameOverrides: map[string]string{},
	}
	seedConsoleRow(t, e, family)

	forgeries := []struct {
		name    string
		headers map[string]string
	}{
		{"sibling subdomain (same-site)", map[string]string{"Origin": "https://sibling.example", "Sec-Fetch-Site": "same-site"}},
		{"another site (cross-site)", map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}},
		{"old browser, Origin only", map[string]string{"Origin": "https://sibling.example"}},
	}
	for _, forgery := range forgeries {
		t.Run(forgery.name, func(t *testing.T) {
			create := url.Values{}
			create.Set("id", "lpr_forged")
			create.Set("providerId", "litellm")
			create.Set("name", "Forged")
			create.Set("baseURL", attacker.URL+"/v1")
			create.Set("apiKeyEnv", "LLM_FORGED_KEY")
			resp, page := e.consoleDo(t, http.MethodPost, "/admin/providers", create, e.token, forgery.headers)
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(page, "cross_origin") {
				t.Errorf("forged create must be refused with a 403 cross_origin page, got %d", resp.StatusCode)
			}
			if _, err := e.st.GetProvider(context.Background(), "lpr_forged"); err != store.ErrNotFound {
				t.Errorf("forged create must not write the row, got err=%v", err)
			}
			// The admin's next visit to the list derives every row's menu.
			e.consoleGet(t, "/admin")
			if gatewayHits.Load() != 0 || keyLeaks.Load() != 0 {
				t.Errorf("the forged row made burrow call the attacker's gateway %d time(s), %d carrying the key",
					gatewayHits.Load(), keyLeaks.Load())
			}

			skillsForm := url.Values{}
			skillsForm.Set("groupCount", "1")
			skillsForm.Set("group.0", adminGroup)
			skillsForm.Set("declare.0", "1")
			skillsForm.Set("skill.0", "code-review")
			resp, _ = e.consoleDo(t, http.MethodPost, "/admin/skills", skillsForm, e.token, forgery.headers)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("forged skill-pack save must be refused with 403, got %d", resp.StatusCode)
			}
			packs, err := e.st.ListSkillPacks(context.Background())
			if err != nil {
				t.Fatalf("list packs: %v", err)
			}
			if _, declared := packs[adminGroup]; declared {
				t.Errorf("forged skill-pack save must not write a rule, got %v", packs)
			}

			resp, _ = e.consoleDo(t, http.MethodPost, "/admin/providers/lpr_family/delete", url.Values{}, e.token, forgery.headers)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("forged delete must be refused with 403, got %d", resp.StatusCode)
			}
			if _, err := e.st.GetProvider(context.Background(), "lpr_family"); err != nil {
				t.Errorf("forged delete must leave the row, got err=%v", err)
			}

			resp, _ = e.consoleDo(t, http.MethodPost, "/admin/providers/lpr_family", url.Values{"name": {"Forged"}}, e.token, forgery.headers)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("forged update must be refused with 403, got %d", resp.StatusCode)
			}

			// Put back whatever a forgery got through, so each case starts
			// from the same state and reports on its own.
			_ = e.st.DeleteProvider(context.Background(), "lpr_forged", time.Time{}, "test")
			_ = e.st.ForgetSkillPack(context.Background(), adminGroup)
			if _, err := e.st.GetProvider(context.Background(), "lpr_family"); err == store.ErrNotFound {
				seedConsoleRow(t, e, family)
			}
			gatewayHits.Store(0)
			keyLeaks.Store(0)
		})
	}
}

// The console's own forms keep working: a browser marks them same-origin, an
// older browser sends an Origin that matches the Host, and a non-browser
// client (curl, the tests' own helper) sends neither header.
func TestConsoleAcceptsSameOriginPosts(t *testing.T) {
	e := newConsoleEnv(t)
	accepted := []struct {
		id      string
		headers map[string]string
	}{
		{"lpr_sameorigin", map[string]string{"Origin": e.srv.URL, "Sec-Fetch-Site": "same-origin"}},
		{"lpr_typed", map[string]string{"Sec-Fetch-Site": "none"}},
		{"lpr_oldbrowser", map[string]string{"Origin": e.srv.URL}},
		{"lpr_noheaders", nil},
	}
	for _, c := range accepted {
		form := url.Values{}
		form.Set("id", c.id)
		form.Set("providerId", "litellm")
		form.Set("name", "Same origin")
		form.Set("baseURL", "http://127.0.0.1:1/v1")
		form.Set("apiKeyEnv", "LLM_UNSET_KEY")
		resp, page := e.consoleDo(t, http.MethodPost, "/admin/providers", form, e.token, c.headers)
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: a same-origin create must be accepted, got %d: %s", c.id, resp.StatusCode, page)
		}
		if _, err := e.st.GetProvider(context.Background(), c.id); err != nil {
			t.Fatalf("%s: created row: %v", c.id, err)
		}
	}

	// A cross-origin GET is a navigation (a link from anywhere), not a forged
	// write: it still renders.
	resp, _ := e.consoleDo(t, http.MethodGet, "/admin", nil, e.token,
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a cross-site navigation to the console must render, got %d", resp.StatusCode)
	}
}

// The JSON API authenticates by bearer only, which a browser never attaches
// on its own, so it carries no cross-origin check: a client that happens to
// send Origin or Sec-Fetch-Site (an Electron renderer does) must not be
// refused.
func TestWriteAPIIgnoresRequestOrigin(t *testing.T) {
	e := newConsoleEnv(t)
	req, err := http.NewRequest(http.MethodPost, e.apiURL+"/v1/admin/llm-providers", strings.NewReader(
		`{"id":"lpr_api","providerId":"litellm","name":"API","baseURL":"http://127.0.0.1:1/v1","apiKeyEnv":"LLM_UNSET_KEY"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Origin", "https://elsewhere.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("the bearer-only API must not apply the console's origin check, got %d", resp.StatusCode)
	}
}

// mustNotBeStored asserts a response that carries a credential (or a page
// built from one) tells every cache between burrow and the member to keep no
// copy.
func mustNotBeStored(t *testing.T, what string, resp *http.Response) {
	t.Helper()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s: Cache-Control must be no-store, got %q", what, got)
	}
}

// mustNotBeFramed asserts an authenticated page refuses to render inside
// another origin's frame, by both the header every browser knows and the one
// that supersedes it.
func mustNotBeFramed(t *testing.T, what string, resp *http.Response) {
	t.Helper()
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("%s: X-Frame-Options must be DENY, got %q", what, got)
	}
	if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") {
		t.Errorf("%s: Content-Security-Policy must carry frame-ancestors 'none', got %q", what, got)
	}
}

// Authenticated HTML must not be framed (a transparent frame over a decoy
// page turns one click into a console write) and nothing that carries a
// credential may be kept by a cache: the console's pages, the OIDC callback
// page that shows a live one-time grant, the exchange response and the MCP
// token response.
func TestCredentialResponsesAreNotStoredOrFramed(t *testing.T) {
	e := newConsoleEnv(t)

	resp, _ := e.consoleGet(t, "/admin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("console list status %d", resp.StatusCode)
	}
	mustNotBeStored(t, "console page", resp)
	mustNotBeFramed(t, "console page", resp)

	resp, _ = e.consoleGet(t, "/admin/providers/lpr_missing")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("console error page status %d", resp.StatusCode)
	}
	mustNotBeStored(t, "console error page", resp)
	mustNotBeFramed(t, "console error page", resp)

	// The exchange answers with a session token.
	grant, err := e.st.MintGrant(context.Background(), aliceSub, e.cfg.GrantTTL)
	if err != nil {
		t.Fatalf("mint grant: %v", err)
	}
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/auth/desktop-handoff/exchange",
		map[string]string{"grant": grant}, nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exchange status %d", resp.StatusCode)
	}
	mustNotBeStored(t, "exchange response", resp)

	// The MCP token mint answers with an MCP token.
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/mcp/token", map[string]any{}, nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mcp token status %d", resp.StatusCode)
	}
	mustNotBeStored(t, "mcp token response", resp)

	// The callback page shows the one-time grant, in both flows.
	issuer := newFakeIssuer(t, "burrow-desktop")
	issuer.groups = []string{adminGroup}
	client := newOIDCClientFor(t, e, issuer)
	for _, start := range []string{"/auth/oidc/start", "/auth/oidc/start?desktop=1"} {
		// Walk the real flow: the state cookie from start must ride along,
		// and the issuer's code is bound to the flow's PKCE challenge, so a
		// fabricated code cannot redeem (the strict fake issuer refuses it).
		started, err := client.Get(e.srv.URL + start)
		if err != nil {
			t.Fatal(err)
		}
		started.Body.Close()
		if started.StatusCode != http.StatusFound {
			t.Fatalf("%s: start must redirect to the issuer, status %d", start, started.StatusCode)
		}
		authorizeURL, _ := url.Parse(started.Header.Get("Location"))
		callback, err := client.Get(approve(t, authorizeURL.String()))
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(callback.Body)
		callback.Body.Close()
		if callback.StatusCode != http.StatusOK || !strings.Contains(string(page), `<code id="grant">`) {
			t.Fatalf("%s: callback must render the grant, status %d", start, callback.StatusCode)
		}
		mustNotBeStored(t, "callback page via "+start, callback)
		mustNotBeFramed(t, "callback page via "+start, callback)
	}
}

// sessionCookieOf returns the burrow_session cookie a response sets, or nil.
func sessionCookieOf(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == "burrow_session" {
			return c
		}
	}
	return nil
}

// signOutForm is the opening tag of the console nav's sign-out form.
const signOutForm = `<form method="post" action="/admin/sign-out">`

// A browser must be able to end its own session. The cookie is HttpOnly, so
// no script can drop it, and the API's sign-out reads a bearer the browser
// never sends: without a console sign-out the session outlives the member
// leaving the machine, for its whole TTL. Signing out deletes the session in
// the store, so the token stops working everywhere it was valid - the console
// and, as a bearer, the API.
func TestConsoleSignOut(t *testing.T) {
	e := newConsoleEnv(t)

	// The nav offers it as a form POST, never a link: a GET that ends a
	// session can be fired by any page that embeds an image.
	_, page := e.consoleGet(t, "/admin")
	if !strings.Contains(page, signOutForm) {
		t.Errorf("the console nav must offer sign-out as a form POST (%s)", signOutForm)
	}

	// A forged sign-out is refused like any other cross-origin console POST,
	// and the session survives it.
	resp, _ := e.consoleDo(t, http.MethodPost, "/admin/sign-out", url.Values{}, e.token,
		map[string]string{"Origin": "https://sibling.example", "Sec-Fetch-Site": "same-site"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("forged sign-out must be refused with 403, got %d", resp.StatusCode)
	}
	if resp, _ := e.consoleGet(t, "/admin"); resp.StatusCode != http.StatusOK {
		t.Fatalf("a forged sign-out must not end the session, console answered %d", resp.StatusCode)
	}

	resp, _ = e.consoleDo(t, http.MethodPost, "/admin/sign-out", url.Values{}, e.token,
		map[string]string{"Origin": e.srv.URL, "Sec-Fetch-Site": "same-origin"})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-out status %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("sign-out must land on the sign-in page, got Location %q", loc)
	}
	expired := sessionCookieOf(resp)
	if expired == nil || expired.MaxAge >= 0 || expired.Value != "" {
		t.Fatalf("sign-out must expire the burrow_session cookie, got %+v", expired)
	}
	if !expired.HttpOnly || expired.Path != "/" {
		t.Fatalf("the expiring cookie must match the one sign-in set (Path=/, HttpOnly), got %+v", expired)
	}

	// The old cookie value is dead in the console...
	resp, _ = e.consoleDo(t, http.MethodGet, "/admin", nil, e.token, nil)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/?mode=sign-in" {
		t.Fatalf("the signed-out cookie must be sent to sign-in, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// ...and as a bearer against the API.
	var body any
	me := e.getJSON(t, "/api/den/v1/me", &body)
	if me.StatusCode != http.StatusUnauthorized || mapOf(t, body)["error"] != "invalid_session" {
		t.Fatalf("the signed-out token must be an invalid_session bearer, got %d %v", me.StatusCode, body)
	}

	// Signing out twice, or with no cookie at all, is not an error.
	resp, _ = e.consoleDo(t, http.MethodPost, "/admin/sign-out", url.Values{}, e.token, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a repeated sign-out must still redirect, got %d", resp.StatusCode)
	}
	resp, _ = e.consoleDo(t, http.MethodPost, "/admin/sign-out", url.Values{}, "", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a sign-out with no cookie must still redirect, got %d", resp.StatusCode)
	}
}

// Sign-out is not an admin action. A member outside BURROW_ADMIN_GROUPS gets
// the cookie at sign-in like anyone else and sees only the console's 403; that
// page must still let them end the session.
func TestConsoleSignOutDoesNotRequireAdmin(t *testing.T) {
	e := newConsoleEnv(t)
	ctx := context.Background()
	if err := e.st.UpsertUser(ctx, store.User{ID: "bob-sub-1", Email: "bob@example.com", Name: "Bob", Groups: []string{"eng"}}); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	bob, err := e.st.CreateSession(ctx, "bob-sub-1", e.cfg.SessionTTL)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}

	resp, page := e.consoleDo(t, http.MethodGet, "/admin", nil, bob, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin console status %d", resp.StatusCode)
	}
	if !strings.Contains(page, signOutForm) {
		t.Errorf("the non-admin's 403 page must offer sign-out as a form POST (%s)", signOutForm)
	}

	resp, _ = e.consoleDo(t, http.MethodPost, "/admin/sign-out", url.Values{}, bob, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("a non-admin sign-out must redirect to /, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, err := e.st.LookupSession(ctx, bob); err != store.ErrNotFound {
		t.Fatalf("the non-admin's session must be gone from the store, got err=%v", err)
	}
	// Alice's session is untouched.
	if resp, _ := e.consoleGet(t, "/admin"); resp.StatusCode != http.StatusOK {
		t.Fatalf("another member's sign-out must not end this session, got %d", resp.StatusCode)
	}
}

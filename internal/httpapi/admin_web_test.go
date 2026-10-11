package httpapi_test

// The /admin console (SPEC-0004 REQ "Admin console surface"): the gate's
// three answers, the row fields, and the rule that a key value never
// reaches the rendered page.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

const consoleSecret = "sk-console-key-value-must-never-render"

// consoleGateway is the row's LiteLLM: two models, and it rejects a bearer
// that is not the row's key.
func consoleGateway(t *testing.T) *httptest.Server {
	t.Helper()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+consoleSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5"},{"id":"claude-opus-5"}]}`))
	}))
	t.Cleanup(gw.Close)
	return gw
}

// getAdminPage fetches /admin without following redirects, and returns the
// response and the rendered body.
func getAdminPage(t *testing.T, e *testEnv, bearer string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

// seedConsoleRows inserts one row that derives a two-model menu from the
// gateway (its key set in the environment) and one row whose key is not
// provisioned: no menu, no gateway call.
func seedConsoleRows(t *testing.T, e *testEnv, gwURL string) {
	t.Helper()
	rows := []store.ProviderRow{
		{ID: "lpr_console_a", ProviderID: "litellm", Name: "Console Gateway",
			BaseURL: gwURL + "/v1", APIKeyEnv: "CONSOLE_KEY_A"},
		{ID: "lpr_console_b", ProviderID: "litellm", Name: "Quiet Gateway",
			BaseURL: "http://127.0.0.1:9", APIKeyEnv: "CONSOLE_KEY_B",
			Groups: []string{"ops", "sre"}},
	}
	for _, row := range rows {
		if err := e.st.CreateProvider(context.Background(), row, "console-test"); err != nil {
			t.Fatalf("seed row %s: %v", row.ID, err)
		}
	}
	t.Setenv("CONSOLE_KEY_A", consoleSecret)
}

// With BURROW_ADMIN_GROUPS unset the console is closed the way the write API
// is: a plain "not enabled" page (200, not 404), for everyone.
func TestAdminConsoleClosedWhenGroupsUnset(t *testing.T) {
	e := newTestEnv(t) // AdminGroups unset
	e.cfg.AdminGroups = nil

	resp, body := getAdminPage(t, e, e.token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("closed console must be a plain 200 page, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "Admin console not enabled") {
		t.Fatalf("closed page must say it is not enabled, got: %.200s", body)
	}

	// Even a would-be admin sees the closed page: groups unset beats
	// credentials, exactly as the write API stays closed.
	signInAs(t, e, "stumpcloud-admins")
	if resp, _ = getAdminPage(t, e, e.token); resp.StatusCode != http.StatusOK || !strings.Contains(resp.Status, "200") {
		t.Fatalf("closed console must stay 200, got %s", resp.Status)
	}
}

// With groups configured: no credential is the sign-in page (a redirect to
// /), a non-admin session is the refusal page, and an admin session is the
// provider list.
func TestAdminConsoleGate(t *testing.T) {
	e := newAdminTestEnv(t)
	gw := consoleGateway(t)
	seedConsoleRows(t, e, gw.URL)

	// No credential: on to the sign-in page, not a 404 and not the list.
	resp, body := getAdminPage(t, e, "")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("no credential must redirect to the sign-in page, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if strings.Contains(body, "lpr_console_a") {
		t.Fatalf("the unauthenticated must not see the list: %.200s", body)
	}

	// Signed in, not an admin: the refusal page.
	signInAs(t, e, "plain-members")
	resp, body = getAdminPage(t, e, e.token)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin session must be refused with 403, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "isn&#39;t for you") {
		t.Fatalf("refusal must say the surface is not for them, got: %.200s", body)
	}
	if strings.Contains(body, "lpr_console_a") {
		t.Fatalf("the refusal must not carry the list: %.200s", body)
	}

	// An admin-group member: the console opens.
	signInAs(t, e, "stumpcloud-admins")
	resp, _ = getAdminPage(t, e, e.token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin session must be served, got %d", resp.StatusCode)
	}
}

// The index rows: the stored fields, the key presence light, the audience,
// and the derived menu's count — "deriving…" until the gateway has answered
// for a row. The key's value never appears in the rendered HTML.
func TestAdminConsoleRowRendering(t *testing.T) {
	e := newAdminTestEnv(t)
	gw := consoleGateway(t)
	seedConsoleRows(t, e, gw.URL)
	signInAs(t, e, "stumpcloud-admins")

	// Warm the derived menu the way a desktop poll would.
	if resp := e.getJSON(t, "/api/den/v1/llm-providers", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("warm the menu via the desktop listing, got %d", resp.StatusCode)
	}

	resp, body := getAdminPage(t, e, e.token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("console must be served, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("the console is server-rendered html, got %q", ct)
	}

	// Row A: name, id, baseURL, the open audience, the derived count.
	for _, want := range []string{"Console Gateway", "lpr_console_a", gw.URL + "/v1", "org-wide", "2 models"} {
		if !strings.Contains(body, want) {
			t.Fatalf("console must carry %q, got: %.400s", want, body)
		}
	}

	// Row B: a restricted audience, a light off, no menu yet.
	for _, want := range []string{"Quiet Gateway", "lpr_console_b", "ops, sre", "deriving…"} {
		if !strings.Contains(body, want) {
			t.Fatalf("console must carry %q, got: %.400s", want, body)
		}
	}

	// The key-presence lights: one on, one off.
	if !strings.Contains(body, `aria-label="API key provisioned"`) || !strings.Contains(body, `aria-label="API key not provisioned"`) {
		t.Fatalf("both key states must render, got: %.400s", body)
	}

	// The key's value is never rendered — the light is the whole of what a
	// page may show of a key.
	if strings.Contains(body, consoleSecret) {
		t.Fatalf("the key value leaked into the rendered page")
	}
}

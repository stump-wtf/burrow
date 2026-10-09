package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/httpapi"
	"gitea.stump.rocks/stump.wtf/burrow/internal/oidc"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
)

// newOIDCTestEnv builds a burrow server whose OIDC client is the production
// one pointed at a local fake issuer, so the whole sign-in path — start,
// callback, exchange, me — runs for real.
func newOIDCTestEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t)

	issuer := newFakeIssuer(t, "burrow-desktop")
	client, err := oidc.New(context.Background(), issuer.srv.URL, "burrow-desktop", "secret",
		e.srv.URL+"/auth/oidc/callback")
	if err != nil {
		t.Fatalf("oidc client against fake issuer: %v", err)
	}
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st, skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}), nil, client).Routes()
	return e
}

var grantRE = regexp.MustCompile(`<code id="grant">([^<]+)</code>`)

// The full desktop sign-in: browser start → issuer → callback mints a grant
// shown as copyable text and handed over via an openwork:// deep link.
func TestSignInFlowMintsGrantAndDeepLink(t *testing.T) {
	e := newOIDCTestEnv(t)
	client := e.srv.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }

	// The desktop app opens / with ?desktopAuth=1.
	resp, err := client.Get(e.srv.URL + "/?mode=sign-in&desktopAuth=1&desktopScheme=openwork")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("root page status %d", resp.StatusCode)
	}

	// Starting the flow redirects to the issuer with PKCE.
	resp, err = client.Get(e.srv.URL + "/auth/oidc/start?desktop=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start status %d", resp.StatusCode)
	}
	authorizeURL, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("start location: %v", err)
	}
	state := authorizeURL.Query().Get("state")
	if state == "" || authorizeURL.Query().Get("code_challenge") == "" ||
		authorizeURL.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize URL must carry state and an S256 challenge: %v", authorizeURL)
	}

	// The callback consumes state+code (the fake issuer's token endpoint
	// mints the ID token) and renders the grant plus the deep link.
	resp, err = client.Get(e.srv.URL + "/auth/oidc/callback?state=" + url.QueryEscape(state) + "&code=test-code")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status %d: %s", resp.StatusCode, page)
	}
	match := grantRE.FindSubmatch(page)
	if match == nil {
		t.Fatalf("callback page must show the grant for manual paste: %s", page)
	}
	grant := string(match[1])
	if len(grant) < 12 {
		t.Fatalf("grant must be pasteable (12+ chars), got %d", len(grant))
	}
	if !strings.Contains(string(page), "openwork://den-auth?") || !strings.Contains(string(page), "grant=") {
		t.Fatalf("desktop flow must produce the openwork://den-auth deep link")
	}
	if !strings.Contains(string(page), "denBaseUrl="+url.QueryEscape(e.srv.URL)) {
		t.Fatalf("deep link must always include denBaseUrl")
	}

	// Exchanging the grant once yields a session; twice is refused.
	var first any
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/auth/desktop-handoff/exchange",
		map[string]string{"grant": grant}, &first, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exchange status %d", resp.StatusCode)
	}
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/auth/desktop-handoff/exchange",
		map[string]string{"grant": grant}, nil, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second exchange must 401, got %d", resp.StatusCode)
	}

	root := mapOf(t, first)
	exchangedToken := nonEmptyString(t, root, "token")
	user := mapOf(t, root["user"])
	nonEmptyString(t, user, "id")
	if nonEmptyString(t, user, "email") != "signer@example.com" {
		t.Fatalf("user.email must come from the ID token claims, got %#v", user)
	}
	org := mapOf(t, root["organization"])
	nonEmptyString(t, org, "id")
	nonEmptyString(t, org, "slug")
	if root["connectEnabled"] != true {
		t.Fatalf("exchange must advertise connectEnabled: true")
	}

	// The minted session works on /v1/me.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/den/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+exchangedToken)
	meResp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	meResp.Body.Close()
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("me with the exchanged token: %d", meResp.StatusCode)
	}
}

// The exact 401 error codes the client signs the user out on.
func TestSessionErrorCodesAreExact(t *testing.T) {
	e := newTestEnv(t)

	codeFor := func(path, token string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := e.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s with token %q: want 401, got %d", path, token, resp.StatusCode)
		}
		var payload any
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return nonEmptyString(t, mapOf(t, payload), "error")
	}

	if got := codeFor("/api/den/v1/me", ""); got != "unauthorized" {
		t.Fatalf("missing header must be unauthorized, got %q", got)
	}
	if got := codeFor("/api/den/v1/me", "not-a-real-token"); got != "invalid_session" {
		t.Fatalf("unknown token must be invalid_session, got %q", got)
	}

	expired, err := e.st.CreateSession(context.Background(), aliceSub, -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := codeFor("/api/den/v1/me", expired); got != "session_expired" {
		t.Fatalf("expired session must be session_expired, got %q", got)
	}
}

// A transient store failure must read as "unavailable" to the client, not as
// a session error: the desktop app signs the user out on the exact
// session-terminating codes and keeps the token on anything else.
func TestSessionStoreFailureIsNotASessionError(t *testing.T) {
	e := newTestEnv(t)
	token, err := e.st.CreateSession(context.Background(), aliceSub, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	e.st.Close()

	req, _ := http.NewRequest(http.MethodGet, e.apiURL+"/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("closed store must 500, got %d", resp.StatusCode)
	}
	var payload any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	code := nonEmptyString(t, mapOf(t, payload), "error")
	for _, terminating := range []string{
		"unauthorized", "invalid_session", "session_expired",
		"session_revoked", "session_not_found", "invalid_token",
		"token_expired", "token_revoked",
	} {
		if code == terminating {
			t.Fatalf("store failure must not answer with session-terminating code %q", code)
		}
	}
}

// The pre-provisioning mint requires a session — an unauthenticated mint
// would hand out sign-in grants to anyone.
func TestHandoffMintRequiresSession(t *testing.T) {
	e := newTestEnv(t)

	resp := e.doJSON(t, http.MethodPost, "/api/den/v1/auth/desktop-handoff",
		map[string]string{"desktopScheme": "openwork"}, nil, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated mint must 401, got %d", resp.StatusCode)
	}

	var payload any
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/auth/desktop-handoff",
		map[string]string{"desktopScheme": "openwork"}, &payload, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated mint status %d", resp.StatusCode)
	}
	grant := nonEmptyString(t, mapOf(t, payload), "grant")

	// The minted grant exchanges for the calling member's session.
	var exchange any
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/auth/desktop-handoff/exchange",
		map[string]string{"grant": grant}, &exchange, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exchange of minted grant: %d", resp.StatusCode)
	}
	if got := mapOf(t, mapOf(t, exchange)["user"]); nonEmptyString(t, got, "id") != aliceSub {
		t.Fatalf("minted grant must belong to the minting member, got %#v", got)
	}
}

// A callback with an unknown state (expired or replayed) is refused.
func TestCallbackRejectsUnknownState(t *testing.T) {
	e := newOIDCTestEnv(t)
	resp, err := e.srv.Client().Get(e.srv.URL + "/auth/oidc/callback?state=bogus&code=x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown state must 400, got %d", resp.StatusCode)
	}
}

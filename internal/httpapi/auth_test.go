package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
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
		e.srv.URL+"/auth/oidc/callback", false)
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

// stubOIDC stands in for the issuer when a test needs the callback to get
// past the token exchange without a real one, or to fail at a chosen step.
type stubOIDC struct {
	exchangeErr error
	// beforeClaims runs once the ID token has "verified", just before the
	// callback goes back to the store.
	beforeClaims func()
}

func (stubOIDC) AuthCodeURL(state, codeVerifier string) string { return "/authorize?state=" + state }

func (c stubOIDC) Exchange(ctx context.Context, code, codeVerifier string) (string, error) {
	if c.exchangeErr != nil {
		return "", c.exchangeErr
	}
	return "raw-id-token", nil
}

func (c stubOIDC) Verify(ctx context.Context, rawIDToken string) (oidc.Claims, error) {
	if c.beforeClaims != nil {
		c.beforeClaims()
	}
	return oidc.Claims{Subject: "signer-sub-1", Email: "signer@example.com", Name: "Signer"}, nil
}

// withOIDC rebinds the server under test to client.
func (e *testEnv) withOIDC(client httpapi.OIDCClient) {
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st, skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}), nil, client).Routes()
}

// get performs an unauthenticated GET without following redirects.
func (e *testEnv) get(t *testing.T, path string) *http.Response {
	t.Helper()
	client := *e.srv.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// A refused sign-in must be visible in the log at the level production runs
// at: before this, an unknown state and a bad grant left nothing behind, so
// "nobody can sign in" could not be told from "nobody tried". The line names
// the outcome and never the state or the grant.
func TestSignInRefusalsAreLogged(t *testing.T) {
	e := newTestEnv(t)
	logs := captureLogs(t)

	const state = "state-value-that-must-not-be-logged"
	resp := e.get(t, "/auth/oidc/callback?state="+state+"&code=x")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown state must 400, got %d", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "invalid_state" {
		t.Fatalf("unknown state must be invalid_state, got %q", code)
	}
	wantLogged(t, logs, []string{"level=WARN", `msg="sign-in refused"`, "reason=invalid_state"}, state)

	const grant = "grant-value-that-must-not-be-logged"
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/auth/desktop-handoff/exchange",
		map[string]string{"grant": grant}, nil, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown grant must 401, got %d", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "invalid_grant" {
		t.Fatalf("unknown grant must be invalid_grant, got %q", code)
	}
	wantLogged(t, logs, []string{"level=WARN", `msg="grant exchange refused"`, "reason=invalid_grant"}, grant)
}

// A database outage is not a bad credential. The callback used to answer it
// 400 invalid_state ("start again") and the exchange 401 invalid_grant, both
// silently — so an outage read, to the member and to the log, as everyone
// mistyping at once. Each step must answer 500 and say so at Error.
func TestSignInStoreFailureIsNotABadCredential(t *testing.T) {
	e := newTestEnv(t)
	logs := captureLogs(t)
	e.st.Close()

	resp := e.get(t, "/auth/oidc/start?desktop=1")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("start with the store down must 500, got %d", resp.StatusCode)
	}
	wantLogged(t, logs, []string{"level=ERROR", `msg="sign-in state save failed"`})

	const state = "state-value-that-must-not-be-logged"
	resp = e.get(t, "/auth/oidc/callback?state="+state+"&code=x")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("callback with the store down must 500, got %d", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "server_error" {
		t.Fatalf("callback with the store down must be server_error, got %q", code)
	}
	wantLogged(t, logs, []string{"level=ERROR", `msg="sign-in state lookup failed"`}, state)

	const grant = "grant-value-that-must-not-be-logged"
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/auth/desktop-handoff/exchange",
		map[string]string{"grant": grant}, nil, false)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("exchange with the store down must 500, got %d", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "server_error" {
		t.Fatalf("exchange with the store down must be server_error, got %q", code)
	}
	wantLogged(t, logs, []string{"level=ERROR", `msg="grant lookup failed"`}, grant)
}

// The later steps of the callback: an issuer that refuses the code, and a
// store that fails once the member is known. Both already answered with the
// right status and logged nothing.
func TestCallbackFailuresAfterStateAreLogged(t *testing.T) {
	const state = "state-value-that-must-not-be-logged"
	const verifier = "verifier-value-that-must-not-be-logged"

	t.Run("the issuer refuses the code", func(t *testing.T) {
		e := newTestEnv(t)
		e.withOIDC(stubOIDC{exchangeErr: errors.New("issuer said no")})
		logs := captureLogs(t)
		if err := e.st.SaveOIDCState(context.Background(), state, verifier, true, time.Minute); err != nil {
			t.Fatal(err)
		}
		resp := e.get(t, "/auth/oidc/callback?state="+state+"&code=x")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("a refused code must 502, got %d", resp.StatusCode)
		}
		wantLogged(t, logs, []string{"level=ERROR", `msg="oidc exchange failed"`, "issuer said no"}, state, verifier)
	})

	t.Run("the store fails recording the member", func(t *testing.T) {
		e := newTestEnv(t)
		e.withOIDC(stubOIDC{beforeClaims: func() { e.st.Close() }})
		logs := captureLogs(t)
		if err := e.st.SaveOIDCState(context.Background(), state, verifier, true, time.Minute); err != nil {
			t.Fatal(err)
		}
		resp := e.get(t, "/auth/oidc/callback?state="+state+"&code=x")
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("a failed upsert must 500, got %d", resp.StatusCode)
		}
		wantLogged(t, logs, []string{"level=ERROR", `msg="sign-in user upsert failed"`, "user=signer-sub-1"}, state, verifier)
	})
}

// A refused session leaves a line too, so a client stuck signing in and out
// shows up in the log with the code it was given — and never with the token.
func TestSessionRefusalsAreLogged(t *testing.T) {
	e := newTestEnv(t)
	logs := captureLogs(t)

	refuse := func(token string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.apiURL+"/v1/me", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := e.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", resp.StatusCode)
		}
	}

	refuse("")
	wantLogged(t, logs, []string{"level=WARN", `msg="session refused"`, "reason=unauthorized", "path=/v1/me"})

	const unknown = "session-token-that-must-not-be-logged"
	refuse(unknown)
	wantLogged(t, logs, []string{"level=WARN", `msg="session refused"`, "reason=invalid_session"}, unknown)

	expired, err := e.st.CreateSession(context.Background(), aliceSub, -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	refuse(expired)
	wantLogged(t, logs, []string{"level=WARN", `msg="session refused"`, "reason=session_expired"}, unknown, expired)
}

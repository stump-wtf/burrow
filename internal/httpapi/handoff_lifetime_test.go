package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// callAs performs one JSON request carrying token as the Bearer session (or
// none when token is empty) and returns the status and the decoded body.
func (e *testEnv) callAs(t *testing.T, token, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	return resp.StatusCode, payload
}

// handoff mints a grant with token and exchanges it, returning the session
// the exchange issued — one turn of the mint → exchange loop.
func (e *testEnv) handoff(t *testing.T, token string) string {
	t.Helper()
	status, minted := e.callAs(t, token, http.MethodPost, "/api/den/v1/auth/desktop-handoff",
		map[string]string{"desktopScheme": "openwork"})
	if status != http.StatusOK {
		t.Fatalf("mint status %d: %v", status, minted)
	}
	status, exchanged := e.callAs(t, "", http.MethodPost, "/api/den/v1/auth/desktop-handoff/exchange",
		map[string]string{"grant": nonEmptyString(t, minted, "grant")})
	if status != http.StatusOK {
		t.Fatalf("exchange status %d: %v", status, exchanged)
	}
	return nonEmptyString(t, exchanged, "token")
}

// A session must not be able to mint its own successor: a member removed from
// the identity provider (or from a provider's or the admin group) would
// otherwise keep access forever on the groups recorded at their last real
// sign-in, by looping mint → exchange before each session lapsed. Every
// generation of the loop dies with the sign-in it descends from.
func TestHandoffLoopCannotOutliveTheSignIn(t *testing.T) {
	t.Parallel() // waits out a real session expiry
	e := newTestEnv(t)
	const life = 3 * time.Second
	deadline := time.Now().Add(life)
	original, err := e.st.CreateSession(context.Background(), aliceSub, life)
	if err != nil {
		t.Fatal(err)
	}

	token := original
	for generation := 1; generation <= 3; generation++ {
		token = e.handoff(t, token)
		if status, _ := e.callAs(t, token, http.MethodGet, "/api/den/v1/me", nil); status != http.StatusOK {
			t.Fatalf("generation %d must work while the sign-in is live, got %d", generation, status)
		}
	}
	if time.Now().After(deadline) {
		t.Fatalf("the loop took longer than the %s session it tests; nothing was proven", life)
	}

	time.Sleep(time.Until(deadline) + 500*time.Millisecond)

	status, body := e.callAs(t, original, http.MethodGet, "/api/den/v1/me", nil)
	if status != http.StatusUnauthorized || body["error"] != "session_expired" {
		t.Fatalf("the original session must have expired, got %d %v", status, body["error"])
	}
	status, body = e.callAs(t, token, http.MethodGet, "/api/den/v1/me", nil)
	if status != http.StatusUnauthorized || body["error"] != "session_expired" {
		t.Fatalf("a third-generation handoff session outlived the sign-in it descends from: got %d %v, want 401 session_expired",
			status, body["error"])
	}
}

// The cap follows the lineage, not the member: a real sign-in through the
// identity provider starts a fresh lifetime even while the same member holds
// an older session that is about to lapse. Without this, capping handoffs
// would also shorten every honest re-authentication.
func TestFreshSignInIsNotCappedByAnOlderSession(t *testing.T) {
	t.Parallel() // waits out a real session expiry
	e := newOIDCTestEnv(t)
	const life = 3 * time.Second
	deadline := time.Now().Add(life)
	client := e.srv.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	signIn := func() string {
		t.Helper()
		resp, err := client.Get(e.srv.URL + "/auth/oidc/start?desktop=1")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		authorizeURL, err := url.Parse(resp.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err = client.Get(e.srv.URL + "/auth/oidc/callback?state=" + url.QueryEscape(authorizeURL.Query().Get("state")) + "&code=test-code")
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		match := grantRE.FindSubmatch(page)
		if match == nil {
			t.Fatalf("callback page must show the grant: %s", page)
		}
		return string(match[1])
	}

	// The first sign-in creates the member the fake issuer signs in; the
	// short session stands in for one they have held for almost a full
	// BURROW_SESSION_TTL.
	status, first := e.callAs(t, "", http.MethodPost, "/api/den/v1/auth/desktop-handoff/exchange",
		map[string]string{"grant": signIn()})
	if status != http.StatusOK {
		t.Fatalf("first exchange: %d %v", status, first)
	}
	sub := nonEmptyString(t, mapOf(t, first["user"]), "id")
	older, err := e.st.CreateSession(context.Background(), sub, time.Until(deadline))
	if err != nil {
		t.Fatal(err)
	}

	// A grant handed over by the older session expires with it …
	descended := e.handoff(t, older)
	// … while a grant from a new callback buys a full session.
	status, second := e.callAs(t, "", http.MethodPost, "/api/den/v1/auth/desktop-handoff/exchange",
		map[string]string{"grant": signIn()})
	if status != http.StatusOK {
		t.Fatalf("second exchange: %d %v", status, second)
	}
	fresh := nonEmptyString(t, second, "token")
	if time.Now().After(deadline) {
		t.Fatalf("setup took longer than the %s session it tests; nothing was proven", life)
	}

	time.Sleep(time.Until(deadline) + 500*time.Millisecond)

	if status, body := e.callAs(t, descended, http.MethodGet, "/api/den/v1/me", nil); status != http.StatusUnauthorized || body["error"] != "session_expired" {
		t.Fatalf("a session handed over by a nearly-expired session must expire with it, got %d %v", status, body["error"])
	}
	if status, body := e.callAs(t, fresh, http.MethodGet, "/api/den/v1/me", nil); status != http.StatusOK {
		t.Fatalf("a session from a fresh sign-in must get a full BURROW_SESSION_TTL, got %d %v", status, body["error"])
	}
}

package httpapi_test

// Stage C: shared MCP connections (stump.wtf/burrow#4). These cover the five
// routes the client contract names, the state the listing answers for each
// credential mode, and the admin surface that writes a connection row. The
// proxy path the same feature adds to search_capabilities and
// execute_capability is in mcpproxy_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/config"
	"gitea.stump.rocks/stump.wtf/burrow/internal/httpapi"
	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/seal"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

const (
	mcpAdminGroup = "mcp-admins"
	mcpSharedKey  = "BURROW_MCP_KEY_ECHO"
)

// fakeUpstream is one stand-in for everything Stage C reaches out to: an MCP
// server on /mcp, and an authorization server on /authorize and /token. One
// host:port serves all of them so a test names one host in the allowlist.
type fakeUpstream struct {
	t *testing.T
	srv *httptest.Server

	mu           sync.Mutex
	authSeen     []string
	toolCalls    []string
	grantTypes   []string
	listCalls    int
	refreshToken string // access token handed out by a refresh
	denyTools    bool   // answers tools/call with 401: the credential is refused
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{t: t, refreshToken: "access-refreshed"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcp":
			f.mcp(w, r)
		case "/authorize":
			f.authorize(w, r)
		case "/token":
			f.token(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) host() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeUpstream) mcp(w http.ResponseWriter, r *http.Request) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		f.t.Errorf("upstream decode: %v", err)
	}
	f.mu.Lock()
	f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
	f.mu.Unlock()

	if msg.Method == "notifications/initialized" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if msg.Method == "tools/call" {
		f.mu.Lock()
		deny := f.denyTools
		f.mu.Unlock()
		if deny {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	answer := map[string]any{"jsonrpc": "2.0"}
	if len(msg.ID) > 0 {
		answer["id"] = json.RawMessage(msg.ID)
	}
	switch msg.Method {
	case "initialize":
		answer["result"] = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
	case "tools/list":
		f.mu.Lock()
		f.listCalls++
		f.mu.Unlock()
		answer["result"] = map[string]any{"tools": []map[string]any{
			{"name": "echo", "title": "Echo", "description": "Echoes its input"},
			{"name": "boom", "description": "Always fails"},
		}}
	case "tools/call":
		f.mu.Lock()
		f.toolCalls = append(f.toolCalls, msg.Params.Name)
		f.mu.Unlock()
		if msg.Params.Name == "boom" {
			answer["error"] = map[string]any{"code": -32603, "message": "tool failed"}
		} else {
			answer["result"] = map[string]any{"content": []map[string]any{{"type": "text", "text": "hello from upstream"}}}
		}
	default:
		answer["error"] = map[string]any{"code": -32601, "message": "method not found"}
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		f.t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(encoded)
}

// authorize plays the authorization server: it sends the browser back to
// burrow's callback with the code and the state it was shown.
func (f *fakeUpstream) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		f.t.Errorf("bad redirect_uri %q: %v", q.Get("redirect_uri"), err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		f.t.Errorf("authorize without PKCE: %v", q)
	}
	out := redirect.Query()
	out.Set("code", "code-1")
	out.Set("state", q.Get("state"))
	redirect.RawQuery = out.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (f *fakeUpstream) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Error(err)
	}
	grant := r.FormValue("grant_type")
	f.mu.Lock()
	f.grantTypes = append(f.grantTypes, grant)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch grant {
	case "authorization_code":
		if r.FormValue("code") != "code-1" || r.FormValue("code_verifier") == "" {
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"access-1","refresh_token":"refresh-1","expires_in":3600}`)
	case "refresh_token":
		if r.FormValue("refresh_token") == "" {
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"refresh-2","expires_in":3600}`, f.refreshToken)
	default:
		fmt.Fprint(w, `{"error":"unsupported_grant_type"}`)
	}
}

// grants reports the grant types the token endpoint has seen.
func (f *fakeUpstream) grants() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.grantTypes...)
}

func (f *fakeUpstream) toolCallNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.toolCalls...)
}

func (f *fakeUpstream) countListCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

// mcpEnv is a burrow whose deployment allows one upstream host and can seal a
// credential, with alice holding the admin group so the write surface is open.
// An empty credentialKey stands in for a server started without
// BURROW_MCP_CREDENTIAL_KEY.
func mcpEnv(t *testing.T, up *fakeUpstream, credentialKey string) *testEnv {
	t.Helper()
	var hosts []string
	if up != nil {
		hosts = []string{up.host()}
	}
	e := newTestEnvWith(t, func(cfg *config.Config) {
		cfg.MCPURLHosts = hosts
		cfg.MCPCredentialKey = credentialKey
		cfg.AdminGroups = []string{mcpAdminGroup}
	})
	e.setGroups(t, []string{mcpAdminGroup})
	return e
}

// setGroups rewrites alice's groups claim, the way a sign-in would.
func (e *testEnv) setGroups(t *testing.T, groups []string) {
	t.Helper()
	if err := e.st.UpsertUser(context.Background(), store.User{
		ID: aliceSub, Email: aliceEmail, Name: "Alice", Groups: groups,
	}); err != nil {
		t.Fatalf("seed groups: %v", err)
	}
}

// createMCPConn writes one connection through the admin surface and returns its
// body as a map.
func createMCPConn(t *testing.T, e *testEnv, body map[string]any) map[string]any {
	t.Helper()
	var out map[string]any
	resp := e.doJSON(t, http.MethodPost, "/api/den/v1/admin/mcp-connections", body, &out, true)
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create connection: status %d: %s", resp.StatusCode, raw)
	}
	return out
}

func mcpConnBody(name, upstreamURL string, fields map[string]any) map[string]any {
	body := map[string]any{"name": name, "url": upstreamURL}
	for k, v := range fields {
		body[k] = v
	}
	return body
}

// listConnections reads GET /v1/mcp-connections and returns the entries keyed
// by id.
func listConnections(t *testing.T, e *testEnv, query string) map[string]map[string]any {
	t.Helper()
	path := "/api/den/v1/mcp-connections"
	if query != "" {
		path += "?" + query
	}
	var payload map[string]any
	resp := e.getJSON(t, path, &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status %d", resp.StatusCode)
	}
	out := map[string]map[string]any{}
	entries, _ := payload["connections"].([]any)
	for _, item := range entries {
		entry := mapOf(t, item)
		id, _ := entry["id"].(string)
		out[id] = entry
	}
	return out
}

func boolField(t *testing.T, entry map[string]any, key string, want bool) {
	t.Helper()
	got, ok := entry[key].(bool)
	if !ok || got != want {
		t.Fatalf("%s = %v, want %v", key, entry[key], want)
	}
}

// sealFor seals a value the way the server would with the given
// BURROW_MCP_CREDENTIAL_KEY, so a test can seed a credential row directly.
func sealFor(t *testing.T, key, value string) string {
	t.Helper()
	sealer, err := seal.New(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.Seal(value)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// past is a time an access token would already have expired at.
func past() time.Time { return time.Now().UTC().Add(-time.Hour) }

// finishOAuthConnect plays the member's browser: open the authorize URL burrow
// handed the client, follow the issuer's redirect back to burrow's callback, and
// report the page burrow answers with.
func finishOAuthConnect(t *testing.T, authorizeURL string) (int, string) {
	t.Helper()
	browser := newBrowser(t)
	resp, err := browser.Get(authorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("issuer redirect = %d", resp.StatusCode)
	}
	callback := resp.Header.Get("Location")
	if callback == "" {
		t.Fatal("issuer gave no redirect location")
	}
	resp, err = browser.Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestMCPConnectionsListingReflectsEachCredentialMode(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "test-credential-key")
	t.Setenv(mcpSharedKey, "shared-upstream-key")

	shared := createMCPConn(t, e, mcpConnBody("Shared echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "apikey", "credentialMode": "shared", "apiKeyEnv": mcpSharedKey,
	}))
	perMember := createMCPConn(t, e, mcpConnBody("My echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "apikey", "credentialMode": "per_member",
	}))
	sharedOAuth := createMCPConn(t, e, mcpConnBody("Signed echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "oauth", "credentialMode": "shared",
		"oauthAuthorizeUrl": up.srv.URL + "/authorize", "oauthTokenUrl": up.srv.URL + "/token",
		"oauthClientId": "burrow-public",
	}))
	anonymous := createMCPConn(t, e, mcpConnBody("Open echo", up.srv.URL+"/mcp", map[string]any{}))
	crewOnly := createMCPConn(t, e, mcpConnBody("Crew echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "none", "groups": []string{"crew"},
	}))

	entries := listConnections(t, e, "")
	if len(entries) != 4 {
		t.Fatalf("usable listing = %d connections, want the 4 addressed to every member", len(entries))
	}
	// A shared apikey connection reads its key from the environment, so it is
	// connected for the org and for whoever may use it.
	sharedEntry := entries[shared["id"].(string)]
	boolField(t, sharedEntry, "connected", true)
	boolField(t, sharedEntry, "connectedForMe", true)
	nonEmptyString(t, sharedEntry, "connectedAt")
	if _, ok := sharedEntry["apiKey"]; ok {
		t.Fatal("the listing must never carry a credential")
	}
	if _, ok := sharedEntry["apiKeyEnv"]; ok {
		t.Fatal("the listing must not name where the org's key lives")
	}

	// A per-member connection nobody has connected is not connected for
	// anybody.
	boolField(t, entries[perMember["id"].(string)], "connected", false)
	boolField(t, entries[perMember["id"].(string)], "connectedForMe", false)

	// A shared OAuth connection waits for a sign-in.
	boolField(t, entries[sharedOAuth["id"].(string)], "connected", false)

	// authType none needs no credential at all.
	boolField(t, entries[anonymous["id"].(string)], "connected", true)
	boolField(t, entries[anonymous["id"].(string)], "connectedForMe", true)

	// A connection addressed to a group alice is not in is not in her listing,
	// and answers as unknown on the route that names it.
	if _, ok := entries[crewOnly["id"].(string)]; ok {
		t.Fatal("a connection outside the member's groups must not be listed")
	}
	resp := e.doJSON(t, http.MethodGet, "/api/den/v1/mcp-connections/"+crewOnly["id"].(string)+"/connect/start", nil, nil, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("connect/start outside the audience = %d, want 404", resp.StatusCode)
	}

	// scope=manageable is the admin's view: the crew-only row appears even
	// though alice may not use it.
	managed := listConnections(t, e, "scope=manageable")
	if _, ok := managed[crewOnly["id"].(string)]; !ok {
		t.Fatal("scope=manageable must show a connection the admin cannot use")
	}

	// A member outside the audience sees only the rows addressed to nobody.
	e.setGroups(t, nil)
	entries = listConnections(t, e, "")
	if _, ok := entries[crewOnly["id"].(string)]; ok {
		t.Fatal("scope=usable must stay audenced for a non-admin")
	}
	if len(entries) != 4 {
		t.Fatalf("non-admin listing = %d, want 4", len(entries))
	}

	var badScope map[string]any
	resp = e.getJSON(t, "/api/den/v1/mcp-connections?scope=whatever", &badScope)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown scope = %d, want 400", resp.StatusCode)
	}
}

func TestMCPSharedKeyMissingMakesTheConnectionNotConnected(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "test-credential-key")
	// The variable is named but not set: the connection exists and is not
	// connected, which is what the listing must say rather than 500.
	t.Setenv(mcpSharedKey, "")
	conn := createMCPConn(t, e, mcpConnBody("Shared echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "apikey", "credentialMode": "shared", "apiKeyEnv": mcpSharedKey,
	}))
	entries := listConnections(t, e, "")
	boolField(t, entries[conn["id"].(string)], "connected", false)
	boolField(t, entries[conn["id"].(string)], "connectedForMe", false)
}

func TestMCPConnectStartAnswersPerMode(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "test-credential-key")
	t.Setenv(mcpSharedKey, "shared-upstream-key")

	sharedKey := createMCPConn(t, e, mcpConnBody("Shared echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "apikey", "credentialMode": "shared", "apiKeyEnv": mcpSharedKey,
	}))
	perMember := createMCPConn(t, e, mcpConnBody("My echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "apikey", "credentialMode": "per_member",
	}))
	oauthConn := createMCPConn(t, e, mcpConnBody("Signed echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "oauth", "credentialMode": "per_member",
		"oauthAuthorizeUrl": up.srv.URL + "/authorize", "oauthTokenUrl": up.srv.URL + "/token",
		"oauthClientId": "burrow-public",
	}))

	var payload map[string]any
	resp := e.getJSON(t, "/api/den/v1/mcp-connections/"+sharedKey["id"].(string)+"/connect/start", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect/start status %d", resp.StatusCode)
	}
	if payload["status"] != "connected" {
		t.Fatalf("a connected connection must answer connected: %#v", payload)
	}

	// A member's own key needs no browser: needs_auth with no URL is the cue to
	// prompt for the key.
	payload = map[string]any{}
	e.getJSON(t, "/api/den/v1/mcp-connections/"+perMember["id"].(string)+"/connect/start", &payload)
	if payload["status"] != "needs_auth" {
		t.Fatalf("an unconnected per-member connection = %#v", payload)
	}
	if url := payload["authorizeUrl"]; url != "" && url != nil {
		t.Fatalf("an apikey connection must not hand back a URL: %v", url)
	}

	// An OAuth connection hands back the URL to open, carrying PKCE and a state.
	payload = map[string]any{}
	e.getJSON(t, "/api/den/v1/mcp-connections/"+oauthConn["id"].(string)+"/connect/start", &payload)
	if payload["status"] != "needs_auth" {
		t.Fatalf("an unconnected oauth connection = %#v", payload)
	}
	authURL := nonEmptyString(t, payload, "authorizeUrl")
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("authorizeUrl %q: %v", authURL, err)
	}
	q := parsed.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("authorizeUrl without PKCE: %v", q)
	}
	state := q.Get("state")
	if state == "" {
		t.Fatal("authorizeUrl carries no state")
	}
	if redirect := q.Get("redirect_uri"); !strings.HasPrefix(redirect, e.srv.URL) ||
		!strings.HasSuffix(redirect, "/oauth/callback") {
		t.Fatalf("redirect_uri = %q, want burrow's own callback", redirect)
	}

	// The browser comes back, and the connect is finished by the state alone:
	// the callback needs no session cookie, so burrow's answer cannot depend on
	// whose browser arrived.
	status, page := finishOAuthConnect(t, authURL)
	if status != http.StatusOK {
		t.Fatalf("callback status %d: %s", status, page)
	}
	if !strings.Contains(page, "Connected") {
		t.Fatalf("callback page = %s", page)
	}

	// The next poll — connect/start again — is what tells the client it worked.
	payload = map[string]any{}
	e.getJSON(t, "/api/den/v1/mcp-connections/"+oauthConn["id"].(string)+"/connect/start", &payload)
	if payload["status"] != "connected" {
		t.Fatalf("after the callback connect/start = %#v", payload)
	}

	// The state is single use: the same callback replayed is refused.
	replay := e.srv.URL + "/mcp-connections/" + oauthConn["id"].(string) + "/oauth/callback?code=code-1&state=" + url.QueryEscape(state)
	resp, err = e.srv.Client().Get(replay)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed state = %d, want 400: %s", resp.StatusCode, body)
	}

	// A state burrow never minted is answered the way a forgery would be.
	resp, err = e.srv.Client().Get(e.srv.URL + "/mcp-connections/" + oauthConn["id"].(string) + "/oauth/callback?code=code-1&state=forged-state")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown state = %d, want 400", resp.StatusCode)
	}
	if strings.Contains(string(body), "forged-state") {
		t.Fatalf("the notice page must not echo the query back: %s", body)
	}
}

func TestMCPOAuthCallbackRefusesWithoutASealingKey(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "") // no BURROW_MCP_CREDENTIAL_KEY
	conn := createMCPConn(t, e, mcpConnBody("Signed echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "oauth", "credentialMode": "shared",
		"oauthAuthorizeUrl": up.srv.URL + "/authorize", "oauthTokenUrl": up.srv.URL + "/token",
		"oauthClientId": "burrow-public",
	}))
	var payload map[string]any
	e.getJSON(t, "/api/den/v1/mcp-connections/"+conn["id"].(string)+"/connect/start", &payload)
	if payload["status"] != "needs_auth" {
		t.Fatalf("connect/start = %#v", payload)
	}
	status, page := finishOAuthConnect(t, nonEmptyString(t, payload, "authorizeUrl"))
	if status != http.StatusServiceUnavailable {
		t.Fatalf("callback without a sealing key = %d, want 503: %s", status, page)
	}
	for _, grant := range up.grants() {
		if grant != "" {
			t.Fatalf("a server that cannot store a credential must not spend the code: grants %v", up.grants())
		}
	}
}

func TestMCPOAuthRefreshHappensBeforeTheConnectionIsUsed(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "test-credential-key")
	conn := createMCPConn(t, e, mcpConnBody("Signed echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "oauth", "credentialMode": "shared",
		"oauthAuthorizeUrl": up.srv.URL + "/authorize", "oauthTokenUrl": up.srv.URL + "/token",
		"oauthClientId": "burrow-public",
	}))
	sealer, err := seal.New("test-credential-key")
	if err != nil {
		t.Fatal(err)
	}
	refreshSealed, err := sealer.Seal("refresh-1")
	if err != nil {
		t.Fatal(err)
	}
	// The stored access token expired: the listing must refresh it rather than
	// report the connection disconnected.
	if err := e.st.PutMCPCredential(context.Background(), store.MCPCredential{
		ConnectionID: conn["id"].(string), MemberID: store.SharedSlot, Kind: store.MCPAuthOAuth,
		Hash: seal.Fingerprint("access-expired"), Sealed: sealFor(t, "test-credential-key", "access-expired"),
		RefreshSealed: refreshSealed, ExpiresAt: past(),
	}); err != nil {
		t.Fatal(err)
	}
	entries := listConnections(t, e, "")
	boolField(t, entries[conn["id"].(string)], "connected", true)
	grants := up.grants()
	if len(grants) == 0 || grants[len(grants)-1] != "refresh_token" {
		t.Fatalf("want a refresh, got %v", grants)
	}
}

func TestMCPPutMyCredentialBranches(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "test-credential-key")
	t.Setenv(mcpSharedKey, "shared-upstream-key")

	perMember := createMCPConn(t, e, mcpConnBody("My echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "apikey", "credentialMode": "per_member",
	}))
	shared := createMCPConn(t, e, mcpConnBody("Shared echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "apikey", "credentialMode": "shared", "apiKeyEnv": mcpSharedKey,
	}))
	oauthConn := createMCPConn(t, e, mcpConnBody("Signed echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "oauth", "credentialMode": "per_member",
		"oauthAuthorizeUrl": up.srv.URL + "/authorize", "oauthTokenUrl": up.srv.URL + "/token",
		"oauthClientId": "burrow-public",
	}))
	path := func(id string) string {
		return "/api/den/v1/mcp-connections/" + id + "/my-credential"
	}

	resp := e.doJSON(t, http.MethodPut, path(shared["id"].(string)), map[string]any{"apiKey": "sk-mine"}, nil, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a member key on a shared connection = %d, want 409", resp.StatusCode)
	}
	resp = e.doJSON(t, http.MethodPut, path(oauthConn["id"].(string)), map[string]any{"apiKey": "sk-mine"}, nil, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a member key on an oauth connection = %d, want 409", resp.StatusCode)
	}
	resp = e.doJSON(t, http.MethodPut, path(perMember["id"].(string)), map[string]any{"apiKey": "  "}, nil, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an empty key = %d, want 400", resp.StatusCode)
	}
	resp = e.doJSON(t, http.MethodPut, path(perMember["id"].(string)), map[string]any{"apiKey": strings.Repeat("k", 5000)}, nil, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an oversized key = %d, want 400", resp.StatusCode)
	}
	resp = e.doJSON(t, http.MethodPut, path(perMember["id"].(string)), map[string]any{"apiKey": "sk-mine", "extra": true}, nil, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unrecognised field = %d, want 400", resp.StatusCode)
	}

	// The happy path: stored, and the listing now says it is connected for her.
	resp = e.doJSON(t, http.MethodPut, path(perMember["id"].(string)), map[string]any{"apiKey": "sk-mine"}, nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("store member key = %d", resp.StatusCode)
	}
	entries := listConnections(t, e, "")
	boolField(t, entries[perMember["id"].(string)], "connected", true)
	boolField(t, entries[perMember["id"].(string)], "connectedForMe", true)

	// What landed in the database is ciphertext plus a fingerprint, never the key.
	cred, err := e.st.GetMCPCredential(context.Background(), perMember["id"].(string), aliceSub)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cred.Sealed, "sk-mine") {
		t.Fatal("a member key must be stored sealed")
	}
	if cred.Hash != seal.Fingerprint("sk-mine") {
		t.Fatal("the stored fingerprint must identify the stored key")
	}
}

func TestMCPCredentialRoutesRefuseWithoutASealingKey(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "")
	conn := createMCPConn(t, e, mcpConnBody("My echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "apikey", "credentialMode": "per_member",
	}))
	resp := e.doJSON(t, http.MethodPut, "/api/den/v1/mcp-connections/"+conn["id"].(string)+"/my-credential",
		map[string]any{"apiKey": "sk-mine"}, nil, true)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("storing a key without a sealing key = %d, want 503", resp.StatusCode)
	}
}

func TestMCPDisconnectRoutes(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "test-credential-key")
	t.Setenv(mcpSharedKey, "shared-upstream-key")

	perMember := createMCPConn(t, e, mcpConnBody("My echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "apikey", "credentialMode": "per_member",
	}))
	sharedOAuth := createMCPConn(t, e, mcpConnBody("Signed echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "oauth", "credentialMode": "shared",
		"oauthAuthorizeUrl": up.srv.URL + "/authorize", "oauthTokenUrl": up.srv.URL + "/token",
		"oauthClientId": "burrow-public",
	}))
	sharedKey := createMCPConn(t, e, mcpConnBody("Shared echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "apikey", "credentialMode": "shared", "apiKeyEnv": mcpSharedKey,
	}))
	if err := e.st.PutMCPCredential(context.Background(), store.MCPCredential{
		ConnectionID: perMember["id"].(string), MemberID: aliceSub, Kind: store.MCPAuthAPIKey,
		Hash: seal.Fingerprint("sk-mine"), Sealed: sealFor(t, "test-credential-key", "sk-mine"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.PutMCPCredential(context.Background(), store.MCPCredential{
		ConnectionID: sharedOAuth["id"].(string), MemberID: store.SharedSlot, Kind: store.MCPAuthOAuth,
		Hash: seal.Fingerprint("access-1"), Sealed: sealFor(t, "test-credential-key", "access-1"),
	}); err != nil {
		t.Fatal(err)
	}
	entries := listConnections(t, e, "")
	boolField(t, entries[perMember["id"].(string)], "connectedForMe", true)
	boolField(t, entries[sharedOAuth["id"].(string)], "connected", true)

	resp := e.doJSON(t, http.MethodPost,
		"/api/den/v1/mcp-connections/"+perMember["id"].(string)+"/disconnect-my-account", nil, nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disconnect-my-account = %d", resp.StatusCode)
	}
	entries = listConnections(t, e, "")
	boolField(t, entries[perMember["id"].(string)], "connectedForMe", false)
	// Disconnecting twice is not an error the client has to understand.
	resp = e.doJSON(t, http.MethodPost,
		"/api/den/v1/mcp-connections/"+perMember["id"].(string)+"/disconnect-my-account", nil, nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second disconnect = %d, want 200", resp.StatusCode)
	}
	// On a shared connection it would sign everyone else out, so it is refused.
	resp = e.doJSON(t, http.MethodPost,
		"/api/den/v1/mcp-connections/"+sharedKey["id"].(string)+"/disconnect-my-account", nil, nil, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("disconnect-my-account on a shared connection = %d, want 409", resp.StatusCode)
	}

	// The org-wide disconnect is POST /v1/oauth-providers/:id/disconnect, and it
	// takes a connection that is shared *and* oauth.
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/oauth-providers/"+sharedOAuth["id"].(string)+"/disconnect", nil, nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shared disconnect = %d", resp.StatusCode)
	}
	entries = listConnections(t, e, "")
	boolField(t, entries[sharedOAuth["id"].(string)], "connected", false)

	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/oauth-providers/"+perMember["id"].(string)+"/disconnect", nil, nil, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("org disconnect of a per-member connection = %d, want 409", resp.StatusCode)
	}
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/oauth-providers/mcc_missing/disconnect", nil, nil, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("org disconnect of an unknown connection = %d, want 404", resp.StatusCode)
	}

	// It is the admin surface's move.
	e.setGroups(t, nil)
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/oauth-providers/"+sharedOAuth["id"].(string)+"/disconnect", nil, nil, true)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("org disconnect by a non-admin = %d, want 403", resp.StatusCode)
	}
}

func TestAdminMCPConnectionSurface(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "test-credential-key")

	// A row may name only a host the deployment allows: otherwise a write would
	// turn burrow into a proxy that carries a stored credential anywhere.
	resp := e.doJSON(t, http.MethodPost, "/api/den/v1/admin/mcp-connections",
		mcpConnBody("Elsewhere", "https://mcp.evil.test/mcp", map[string]any{"authType": "apikey", "credentialMode": "shared"}), nil, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a host outside the allowlist = %d, want 400", resp.StatusCode)
	}
	// ...and a shared key may be named only by a variable the deployment allows.
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/admin/mcp-connections",
		mcpConnBody("Leaky", up.srv.URL+"/mcp", map[string]any{
			"authType": "apikey", "credentialMode": "shared", "apiKeyEnv": "AWS_SECRET_ACCESS_KEY",
		}), nil, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an apiKeyEnv outside the prefixes = %d, want 400", resp.StatusCode)
	}
	// A row must describe something burrow can act on.
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/admin/mcp-connections",
		mcpConnBody("Nonsense", up.srv.URL+"/mcp", map[string]any{"authType": "oauth"}), nil, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oauth without an authorize URL = %d, want 400", resp.StatusCode)
	}
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/admin/mcp-connections",
		mcpConnBody("Nonsense", up.srv.URL+"/mcp", map[string]any{"authType": "bearer"}), nil, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown authType = %d, want 400", resp.StatusCode)
	}
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/admin/mcp-connections",
		mcpConnBody("", up.srv.URL+"/mcp", nil), nil, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a missing name = %d, want 400", resp.StatusCode)
	}

	// A duplicate id is refused rather than silently replacing a row that may
	// carry credentials.
	created := createMCPConn(t, e, mcpConnBody("Echo", up.srv.URL+"/mcp", map[string]any{"authType": "none"}))
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/admin/mcp-connections",
		mcpConnBody("Echo again", up.srv.URL+"/mcp", map[string]any{"id": created["id"], "authType": "none"}), nil, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a duplicate id = %d, want 409", resp.StatusCode)
	}

	// An update needs the timestamp the editor read, and refuses a stale one.
	updatedAt := nonEmptyString(t, created, "updatedAt")
	resp = e.doJSON(t, http.MethodPut, "/api/den/v1/admin/mcp-connections/"+created["id"].(string),
		mcpConnBody("Echo renamed", up.srv.URL+"/mcp", map[string]any{"authType": "none", "updatedAt": "2020-01-01T00:00:00Z"}), nil, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a stale update = %d, want 409", resp.StatusCode)
	}
	var updated map[string]any
	resp = e.doJSON(t, http.MethodPut, "/api/den/v1/admin/mcp-connections/"+created["id"].(string),
		mcpConnBody("Echo renamed", up.srv.URL+"/mcp", map[string]any{"authType": "none", "updatedAt": updatedAt}), &updated, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update = %d", resp.StatusCode)
	}
	if updated["name"] != "Echo renamed" {
		t.Fatalf("updated name = %v", updated["name"])
	}

	// The admin listing carries the row's shape but never a credential value.
	var listed map[string]any
	e.getJSON(t, "/api/den/v1/admin/mcp-connections", &listed)
	for _, item := range listOf(t, listed["connections"]) {
		row := mapOf(t, item)
		if _, ok := row["apiKey"]; ok {
			t.Fatal("the admin listing must not carry a credential")
		}
	}

	resp = e.doJSON(t, http.MethodDelete, "/api/den/v1/admin/mcp-connections/"+created["id"].(string), nil, nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	resp = e.doJSON(t, http.MethodDelete, "/api/den/v1/admin/mcp-connections/"+created["id"].(string), nil, nil, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete of a gone row = %d, want 404", resp.StatusCode)
	}

	// The whole surface is the admin gate.
	e.setGroups(t, nil)
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/admin/mcp-connections",
		mcpConnBody("Echo", up.srv.URL+"/mcp", map[string]any{"authType": "none"}), nil, true)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create by a non-admin = %d, want 403", resp.StatusCode)
	}
}

// TestMCPUpstreamHostAllowlistIsEnforcedOnUse closes the hole a written row
// would otherwise leave: a connection whose host leaves the allowlist after it
// was saved must not be spoken to.
func TestMCPUpstreamHostAllowlistIsEnforcedOnUse(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "test-credential-key")
	conn := createMCPConn(t, e, mcpConnBody("Loopback echo", "http://"+up.host()+"/mcp", map[string]any{"authType": "none"}))
	entries := listConnections(t, e, "")
	boolField(t, entries[conn["id"].(string)], "connected", true)

	// Rebuild the server with an allowlist that no longer names that host: the
	// row survives, and nothing may reach it.
	e.cfg.MCPURLHosts = []string{"mcp.allowed.test"}
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st,
		skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}),
		providers.NewRuntime(e.st, e.cfg.ModelsTTL, providers.Unconstrained()),
		providers.Unconstrained(), nil).Routes()

	token := e.mcpToken(t)
	_, body := e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "search_capabilities", "arguments": map[string]any{"query": ""}},
	})
	if strings.Contains(string(body), "echo") {
		t.Fatalf("a connection outside the allowlist must offer no tools: %s", body)
	}
}

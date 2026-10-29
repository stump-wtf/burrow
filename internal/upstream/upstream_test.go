package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer is a stand-in upstream MCP server: Streamable HTTP, JSON or SSE
// answers, and it records what it was asked so a test can assert on the headers
// burrow presented.
type fakeServer struct {
	t       *testing.T
	sse     bool
	noAuth  bool // answers 401 to anything
	calls   []string
	auths   []string
	session string
}

func newFakeServer(t *testing.T, sse bool) (*fakeServer, *httptest.Server) {
	t.Helper()
	f := &fakeServer{t: t, sse: sse, session: "sess-1"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Errorf("decode: %v", err)
		}
		f.calls = append(f.calls, msg.Method)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		if f.noAuth && msg.Method != "initialize" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if msg.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		content := "application/json"
		if sse {
			content = "text/event-stream"
		}
		w.Header().Set("Mcp-Session-Id", f.session)
		w.Header().Set("Content-Type", content)
		// One JSON-RPC answer, assembled as a value so the braces cannot
		// drift from the shape the client decodes.
		answer := map[string]any{"jsonrpc": "2.0"}
		if len(msg.ID) > 0 {
			answer["id"] = json.RawMessage(msg.ID)
		}
		switch msg.Method {
		case "initialize":
			answer["result"] = map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{}}
		case "tools/list":
			answer["result"] = map[string]any{"tools": []map[string]any{{
				"name": "echo", "title": "Echo", "description": "Echoes its input",
				"inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			if msg.Params.Name == "boom" {
				answer["error"] = map[string]any{"code": -32603, "message": "tool failed"}
			} else {
				answer["result"] = map[string]any{"content": []map[string]any{{"type": "text", "text": "hello"}}}
			}
		default:
			answer["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		encoded, err := json.Marshal(answer)
		if err != nil {
			t.Fatalf("encode answer: %v", err)
		}
		if sse {
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", encoded)
			return
		}
		w.Write(encoded)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func TestListAndCallTools(t *testing.T) {
	for _, sse := range []bool{false, true} {
		fake, srv := newFakeServer(t, sse)
		client := NewClient(srv.Client(), Unconstrained(), "test")
		tools, err := client.ListTools(context.Background(), srv.URL, Headers{"Authorization": "Bearer shared-key"})
		if err != nil {
			t.Fatalf("ListTools (sse=%v): %v", sse, err)
		}
		if len(tools) != 1 || tools[0].Name != "echo" || tools[0].Title != "Echo" {
			t.Fatalf("tools = %#v", tools)
		}
		result, err := client.CallTool(context.Background(), srv.URL, Headers{"Authorization": "Bearer shared-key"}, "echo", map[string]any{"input": "hi"})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !strings.Contains(string(result), "hello") {
			t.Fatalf("result = %s", result)
		}
		// The handshake runs once per call, and every request carries the
		// credential the caller handed over.
		for _, method := range fake.calls {
			if method != "initialize" && method != "notifications/initialized" && method != "tools/list" && method != "tools/call" {
				t.Fatalf("unexpected method %q", method)
			}
		}
		for _, auth := range fake.auths {
			if auth != "Bearer shared-key" {
				t.Fatalf("upstream saw Authorization %q", auth)
			}
		}
	}
}

func TestCallToolSurfacesRefusalAndError(t *testing.T) {
	fake, srv := newFakeServer(t, false)
	fake.noAuth = true
	client := NewClient(srv.Client(), Unconstrained(), "test")
	if _, err := client.ListTools(context.Background(), srv.URL, nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}

	fake.noAuth = false
	if _, err := client.CallTool(context.Background(), srv.URL, nil, "boom", nil); err == nil ||
		!strings.Contains(err.Error(), "tool failed") {
		t.Fatalf("want the jsonrpc error, got %v", err)
	}
}

func TestPolicyChecksEveryURL(t *testing.T) {
	p := NewPolicy([]string{"mcp.example.test", "127.0.0.1:8080"})
	for _, ok := range []string{"https://mcp.example.test/agent", "http://127.0.0.1:8080/mcp"} {
		if _, err := p.CheckURL(ok); err != nil {
			t.Fatalf("%s should be allowed: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "https://evil.test/mcp", "ftp://mcp.example.test", "https://user:pa55@mcp.example.test", "https://mcp.example.test.evil.test"} {
		if _, err := p.CheckURL(bad); err == nil {
			t.Fatalf("%s should be refused", bad)
		}
	}
	if got, err := p.CheckURL("https://MCP.example.test/agent"); err != nil || got != "https://mcp.example.test/agent" {
		t.Fatalf("CheckURL normalized to %q (err %v)", got, err)
	}
	if _, err := p.CheckURL("https://mcp.example.test/x"); err != nil {
		t.Fatalf("a path must not change the verdict: %v", err)
	}
}

func TestAuthorizeURLCarriesPKCEAndState(t *testing.T) {
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusFound)
	}))
	defer authSrv.Close()

	verifier, err := PKCEVerifier()
	if err != nil {
		t.Fatal(err)
	}
	out, err := AuthorizeURL(Unconstrained(), authSrv.URL, "burrow-client", "https://burrow.test/mcp-connections/mcc_1/oauth/callback",
		"state-abc", verifier, "mcp:read mcp:write")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != "burrow-client" || q.Get("state") != "state-abc" {
		t.Fatalf("query = %v", q)
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("code_challenge") == verifier {
		t.Fatalf("pkce = %q / %q", q.Get("code_challenge"), q.Get("code_challenge_method"))
	}
	if q.Get("scope") != "mcp:read mcp:write" {
		t.Fatalf("scope = %q", q.Get("scope"))
	}
	if q.Get("redirect_uri") != "https://burrow.test/mcp-connections/mcc_1/oauth/callback" {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
}

func TestTokenExchangeAndRefresh(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		got = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"access-1","refresh_token":"refresh-1","expires_in":"3600"}`)
	}))
	defer srv.Close()

	token, err := ExchangeCode(context.Background(), srv.Client(), Unconstrained(), srv.URL, "code-1", "verifier-1", "burrow-client", "https://burrow.test/cb")
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "access-1" || token.RefreshToken != "refresh-1" || token.ExpiresIn != time.Hour {
		t.Fatalf("token = %#v", token)
	}
	if got.Get("grant_type") != "authorization_code" || got.Get("code_verifier") != "verifier-1" {
		t.Fatalf("form = %v", got)
	}

	if _, err := Refresh(context.Background(), srv.Client(), Unconstrained(), srv.URL, "refresh-1", "burrow-client"); err != nil {
		t.Fatal(err)
	}
	if got.Get("grant_type") != "refresh_token" || got.Get("refresh_token") != "refresh-1" {
		t.Fatalf("refresh form = %v", got)
	}
}

func TestRefusedTokenEndpointIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()
	if _, err := ExchangeCode(context.Background(), srv.Client(), Unconstrained(), srv.URL, "c", "v", "cid", "https://burrow.test/cb"); !strings.Contains(fmt.Sprint(err), "invalid_grant") {
		t.Fatalf("want the oauth error, got %v", err)
	}
}

func TestToolCacheExpiryAndForget(t *testing.T) {
	cache := NewToolCache(time.Minute)
	start := time.Now()
	key := CacheKey("mcc_1", "hash-a")
	cache.Put(key, []Tool{{Name: "echo"}}, start)
	if tools, ok := cache.Get(key, start.Add(30*time.Second)); !ok || tools[0].Name != "echo" {
		t.Fatalf("want the cached list, got %#v ok=%v", tools, ok)
	}
	if _, ok := cache.Get(key, start.Add(2*time.Minute)); ok {
		t.Fatal("an entry older than the ttl must not be trusted")
	}
	if _, ok := cache.Get(CacheKey("mcc_1", "hash-b"), start); ok {
		t.Fatal("a different credential must not read another's tool list")
	}
	cache.Forget("mcc_1")
	if _, ok := cache.Get(key, start); ok {
		t.Fatal("Forget should drop the connection's entries")
	}
	if NewToolCache(0) != nil {
		t.Fatal("a zero ttl means no cache")
	}
}

func TestUpstreamCallsDoNotFollowRedirects(t *testing.T) {
	var hops int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hops, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	client := NewHTTPClient(30 * time.Second)
	if _, err := ExchangeCode(context.Background(), client, Unconstrained(), srv.URL, "code-1", "verifier-1", "burrow-client", "https://burrow.test/cb"); err == nil {
		t.Fatal("the token exchange followed a 307 instead of refusing it")
	}
	pol := NewClient(client, Unconstrained(), "test")
	if _, err := pol.CallTool(context.Background(), srv.URL, Headers{"Authorization": "Bearer k"}, "tool", nil); err == nil {
		t.Fatal("the tool call followed a 307 instead of refusing it")
	}
	if atomic.LoadInt32(&hops) != 0 {
		t.Fatalf("the redirect target saw %d requests it must never see", hops)
	}
}

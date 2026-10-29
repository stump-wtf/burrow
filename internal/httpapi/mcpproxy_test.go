package httpapi_test

// The proxy half of Stage C (stump.wtf/burrow#4): the agent's two tools,
// bridged to the org's MCP connections. What these tests hold burrow to is that
// a connection is reachable over MCP exactly when it is visible on the listing,
// that the credential which runs a tool is the one this member's connection is
// connected with, and that one upstream server having a bad day does not turn
// into a burrow outage.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// searchEntries runs search_capabilities over the MCP endpoint and returns the
// entries it answered with.
func searchEntries(t *testing.T, e *testEnv, query string) []map[string]any {
	t.Helper()
	token := e.mcpToken(t)
	_, body := e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "search_capabilities", "arguments": map[string]any{"query": query}},
	})
	var answer struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode search answer: %v (%s)", err, body)
	}
	if answer.Error != nil {
		t.Fatalf("search_capabilities failed: %s", answer.Error.Message)
	}
	if len(answer.Result.Content) != 1 {
		t.Fatalf("search content = %d blocks", len(answer.Result.Content))
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(answer.Result.Content[0].Text), &entries); err != nil {
		t.Fatalf("decode entries: %v (%s)", err, answer.Result.Content[0].Text)
	}
	return entries
}

// capabilityOf finds the upstream tool entry of one connection, by the tool name
// it should carry.
func capabilityOf(t *testing.T, entries []map[string]any, connectionID, tool string) (string, bool) {
	t.Helper()
	want := "mcp:" + connectionID + ":" + tool
	for _, entry := range entries {
		if capability, _ := entry["capability"].(string); capability == want {
			return want, true
		}
	}
	return "", false
}

func TestMCPProxySearchesAndRunsUpstreamTools(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "test-credential-key")
	t.Setenv(mcpSharedKey, "shared-upstream-key")
	conn := createMCPConn(t, e, mcpConnBody("Echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "apikey", "credentialMode": "shared", "apiKeyEnv": mcpSharedKey,
	}))

	entries := searchEntries(t, e, "")
	capability, ok := capabilityOf(t, entries, conn["id"].(string), "echo")
	if !ok {
		t.Fatalf("search must offer the connection's tools: %#v", entries)
	}
	for _, entry := range entries {
		if entry["capability"] == capability {
			if name, _ := entry["name"].(string); !strings.Contains(name, "Echo") {
				t.Fatalf("entry name = %q, want it to name the connection", name)
			}
			if urlField, _ := entry["url"].(string); urlField != "mcp://"+conn["id"].(string)+"/echo" {
				t.Fatalf("entry url = %q", urlField)
			}
		}
	}

	// The agent's call runs upstream, with the connection's credential, and the
	// server's own content comes back unchanged.
	token := e.mcpToken(t)
	_, body := e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "execute_capability", "arguments": map[string]any{
			"capability": capability, "input": `{"text":"hi"}`,
		}},
	})
	if !strings.Contains(string(body), "hello from upstream") {
		t.Fatalf("execute_capability must pass the upstream result through: %s", body)
	}
	var called []string
	for _, name := range up.toolCallNames() {
		called = append(called, name)
	}
	if len(called) == 0 || called[len(called)-1] != "echo" {
		t.Fatalf("upstream tool calls = %v", called)
	}
	for _, auth := range up.authSeen {
		if auth != "Bearer shared-upstream-key" {
			t.Fatalf("upstream saw Authorization %q, want the connection's own key", auth)
		}
	}

	// An upstream refusal is the server's answer, not a burrow outage.
	if _, ok := capabilityOf(t, entries, conn["id"].(string), "boom"); !ok {
		t.Fatalf("search must offer the failing tool too: %#v", entries)
	}
	_, body = e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "execute_capability", "arguments": map[string]any{
			"capability": "mcp:" + conn["id"].(string) + ":boom",
		}},
	})
	if !strings.Contains(string(body), "tool failed") {
		t.Fatalf("an upstream jsonrpc error must reach the agent: %s", body)
	}
}

func TestMCPProxyHonoursTheConnectionAudience(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "test-credential-key")
	conn := createMCPConn(t, e, mcpConnBody("Crew echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "none", "groups": []string{"crew"},
	}))

	entries := searchEntries(t, e, "")
	if _, ok := capabilityOf(t, entries, conn["id"].(string), "echo"); ok {
		t.Fatal("a connection outside the member's groups must offer no tools over MCP")
	}
	token := e.mcpToken(t)
	_, body := e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "execute_capability", "arguments": map[string]any{
			"capability": "mcp:" + conn["id"].(string) + ":echo",
		}},
	})
	if !strings.Contains(string(body), "unknown capability") {
		t.Fatalf("a member outside the audience is answered as an unknown capability: %s", body)
	}
	if strings.Contains(string(body), "Crew echo") {
		t.Fatalf("the refusal must not name a connection they cannot see: %s", body)
	}

	// Give them the group and the same call goes through.
	e.setGroups(t, []string{mcpAdminGroup, "crew"})
	entries = searchEntries(t, e, "")
	if _, ok := capabilityOf(t, entries, conn["id"].(string), "echo"); !ok {
		t.Fatalf("with the group the tools must appear: %#v", entries)
	}
	_, body = e.mcpPost(t, e.mcpToken(t), map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "execute_capability", "arguments": map[string]any{
			"capability": "mcp:" + conn["id"].(string) + ":echo",
		}},
	})
	if !strings.Contains(string(body), "hello from upstream") {
		t.Fatalf("execute through the connection = %s", body)
	}
}

func TestMCPProxyOnlyOffersConnectedConnections(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "test-credential-key")
	perMember := createMCPConn(t, e, mcpConnBody("My echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "apikey", "credentialMode": "per_member",
	}))
	entries := searchEntries(t, e, "")
	if _, ok := capabilityOf(t, entries, perMember["id"].(string), "echo"); ok {
		t.Fatal("a connection nobody has connected must offer no tools")
	}
	token := e.mcpToken(t)
	_, body := e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "execute_capability", "arguments": map[string]any{
			"capability": "mcp:" + perMember["id"].(string) + ":echo",
		}},
	})
	if !strings.Contains(string(body), "needs you to connect it first") {
		t.Fatalf("an unconnected connection = %s", body)
	}
}

func TestMCPProxyReportsARefusedCredential(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "test-credential-key")
	t.Setenv(mcpSharedKey, "shared-upstream-key")
	conn := createMCPConn(t, e, mcpConnBody("Echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "apikey", "credentialMode": "shared", "apiKeyEnv": mcpSharedKey,
	}))
	// The tools listed while the key was still accepted; now the server refuses
	// it, and the agent must be told to connect again rather than that burrow is
	// broken.
	token := e.mcpToken(t)
	if _, ok := capabilityOf(t, searchEntries(t, e, ""), conn["id"].(string), "echo"); !ok {
		t.Fatal("the connection must offer its tools before the refusal")
	}
	up.mu.Lock()
	up.denyTools = true
	up.mu.Unlock()

	_, body := e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "execute_capability", "arguments": map[string]any{
			"capability": "mcp:" + conn["id"].(string) + ":echo",
		}},
	})
	if !strings.Contains(string(body), "refused the stored credential") {
		t.Fatalf("a refused credential = %s", body)
	}
}

func TestMCPToolListIsCachedUntilTheConnectionChanges(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "test-credential-key")
	conn := createMCPConn(t, e, mcpConnBody("Echo", up.srv.URL+"/mcp", map[string]any{"authType": "none"}))

	// Two searches in a row ask the upstream server once: an agent searches on
	// every prompt, and the answer only changes when the row does.
	for range 2 {
		if _, ok := capabilityOf(t, searchEntries(t, e, ""), conn["id"].(string), "echo"); !ok {
			t.Fatal("the connection must offer its tools")
		}
	}
	if got := up.countListCalls(); got != 1 {
		t.Fatalf("tools/list calls = %d, want the cached answer to be reused", got)
	}

	// An operator's edit drops the cache: the next search asks the server they
	// just changed.
	var updated map[string]any
	e.doJSON(t, http.MethodPut, "/api/den/v1/admin/mcp-connections/"+conn["id"].(string),
		mcpConnBody("Echo renamed", up.srv.URL+"/mcp", map[string]any{
			"authType": "none", "updatedAt": nonEmptyString(t, conn, "updatedAt"),
		}), &updated, true)
	if _, ok := capabilityOf(t, searchEntries(t, e, ""), conn["id"].(string), "echo"); !ok {
		t.Fatal("the renamed connection must still offer its tools")
	}
	if got := up.countListCalls(); got != 2 {
		t.Fatalf("tools/list calls after an edit = %d, want 2", got)
	}
}

func TestMCPProxyPerMemberCredentialIsNotShared(t *testing.T) {
	up := newFakeUpstream(t)
	e := mcpEnv(t, up, "test-credential-key")
	conn := createMCPConn(t, e, mcpConnBody("My echo", up.srv.URL+"/mcp", map[string]any{
		"authType": "apikey", "credentialMode": "per_member",
	}))
	if err := e.st.PutMCPCredential(t.Context(), store.MCPCredential{
		ConnectionID: conn["id"].(string), MemberID: aliceSub, Kind: store.MCPAuthAPIKey,
		Hash: "hash-of-alices-key", Sealed: sealFor(t, "test-credential-key", "sk-alice"),
	}); err != nil {
		t.Fatal(err)
	}
	entries := searchEntries(t, e, "")
	capability, ok := capabilityOf(t, entries, conn["id"].(string), "echo")
	if !ok {
		t.Fatalf("a member's own connection must be theirs to search: %#v", entries)
	}
	if _, body := e.mcpPost(t, e.mcpToken(t), map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "execute_capability", "arguments": map[string]any{
			"capability": capability,
		}},
	}); !strings.Contains(string(body), "hello from upstream") {
		t.Fatalf("execute with the member's own key = %s", body)
	}
	for _, auth := range up.authSeen {
		if auth != "Bearer sk-alice" {
			t.Fatalf("upstream saw %q, want the member's own key", auth)
		}
	}
}

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// mcpEnv mints an MCP token through the real endpoint.
func (e *testEnv) mcpToken(t *testing.T) string {
	t.Helper()
	var payload any
	resp := e.doJSON(t, http.MethodPost, "/api/den/v1/mcp/token",
		map[string]any{"scopes": []string{"mcp:read", "mcp:write"}}, &payload, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mcp token status %d", resp.StatusCode)
	}
	root := mapOf(t, payload)
	nonEmptyString(t, root, "token")
	nonEmptyString(t, root, "expiresAt")
	nonEmptyString(t, root, "organizationId")
	resource := nonEmptyString(t, root, "resource")
	// The MCP URL is <resource>/agent and must end in /mcp/agent.
	if !strings.HasSuffix(resource+"/agent", "/mcp/agent") {
		t.Fatalf("resource %q must yield an agent URL ending in /mcp/agent", resource)
	}
	return root["token"].(string)
}

// mcpPost sends one JSON-RPC message to /mcp/agent.
func (e *testEnv) mcpPost(t *testing.T, token string, message map[string]any) (*http.Response, []byte) {
	t.Helper()
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/mcp/agent", bytes.NewReader(encoded))
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
	t.Cleanup(func() { resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// notifications/initialized must answer HTTP 202 with an empty body — the
// client's transport fails the whole session otherwise.
func TestMCPNotificationsInitializedIs202Empty(t *testing.T) {
	e := newTestEnv(t)
	token := e.mcpToken(t)

	resp, body := e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params":  map[string]any{},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d (body %s)", resp.StatusCode, body)
	}
	if len(bytes.TrimSpace(body)) != 0 {
		t.Fatalf("want an empty body, got %q", body)
	}
}

func TestMCPInitializeHandshake(t *testing.T) {
	e := newTestEnv(t)
	token := e.mcpToken(t)

	resp, body := e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "1"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var envelope struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if envelope.Result.ProtocolVersion != "2025-06-18" && envelope.Result.ProtocolVersion != "2025-03-26" {
		t.Fatalf("protocolVersion must be 2025-06-18 or 2025-03-26, got %q", envelope.Result.ProtocolVersion)
	}
	if envelope.Result.ServerInfo.Name != "burrow" {
		t.Fatalf("serverInfo.name, got %q", envelope.Result.ServerInfo.Name)
	}
}

func TestMCPToolsListHasRequiredTools(t *testing.T) {
	e := newTestEnv(t)
	token := e.mcpToken(t)

	_, body := e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{},
	})
	var envelope struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	found := map[string]bool{}
	for _, tool := range envelope.Result.Tools {
		found[tool.Name] = true
		if tool.InputSchema["type"] != "object" {
			t.Fatalf("tool %s inputSchema must be an object schema", tool.Name)
		}
	}
	// search_capabilities and execute_capability are required by name;
	// get_skill is optional but burrow serves it.
	for _, required := range []string{"search_capabilities", "execute_capability", "get_skill"} {
		if !found[required] {
			t.Fatalf("tool %s missing from tools/list: %v", required, found)
		}
	}
}

func TestMCPSkillIndexValidAgainstDiscoverySchema(t *testing.T) {
	e := newTestEnv(t)
	token := e.mcpToken(t)

	_, body := e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "resources/read",
		"params": map[string]any{"uri": "skill://index.json"},
	})
	var envelope struct {
		Result struct {
			Contents []struct {
				URI      string `json:"uri"`
				MimeType string `json:"mimeType"`
				Text     string `json:"text"`
			} `json:"contents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(envelope.Result.Contents) != 1 {
		t.Fatalf("one content entry expected")
	}
	content := envelope.Result.Contents[0]
	if content.URI != "skill://index.json" || content.MimeType != "application/json" {
		t.Fatalf("content descriptor wrong: %+v", content)
	}

	// Enforce the schema fields the client validates (connect-skill-catalog.ts).
	var index struct {
		Schema string `json:"$schema"`
		Skills []struct {
			Name        string `json:"name"`
			Type        string `json:"type"`
			Title       string `json:"title"`
			Description string `json:"description"`
			URL         string `json:"url"`
			Capability  string `json:"capability"`
		} `json:"skills"`
	}
	if err := json.Unmarshal([]byte(content.Text), &index); err != nil {
		t.Fatalf("index is not JSON: %v", err)
	}
	if index.Schema != "https://schemas.agentskills.io/discovery/0.2.0/schema.json" {
		t.Fatalf("$schema must be the discovery 0.2.0 literal, got %q", index.Schema)
	}
	if len(index.Skills) != 2 {
		t.Fatalf("two skills expected, got %d", len(index.Skills))
	}
	nameRE := regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	capabilityRE := regexp.MustCompile(`^(?:skill:[^:]+|plugin:[^:]+:[^:]+)$`)
	for _, skill := range index.Skills {
		if !nameRE.MatchString(skill.Name) || len(skill.Name) > 64 {
			t.Fatalf("skill name %q violates the kebab-case/64-byte rule", skill.Name)
		}
		if skill.Type != "skill-md" {
			t.Fatalf("skill %s type must be skill-md, got %q", skill.Name, skill.Type)
		}
		if strings.TrimSpace(skill.Description) == "" || len(skill.Description) > 1024 {
			t.Fatalf("skill %s description must be 1..1024 chars", skill.Name)
		}
		if !strings.HasPrefix(skill.URL, "skill://") {
			t.Fatalf("skill %s url must be skill://, got %q", skill.Name, skill.URL)
		}
		if !capabilityRE.MatchString(skill.Capability) {
			t.Fatalf("skill %s capability %q violates the capability grammar", skill.Name, skill.Capability)
		}
	}
}

func TestMCPToolsCall(t *testing.T) {
	e := newTestEnv(t)
	token := e.mcpToken(t)

	// search_capabilities finds one of the two fixture skills.
	_, body := e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "search_capabilities", "arguments": map[string]any{"query": "deploy"}},
	})
	var search struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &search); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(search.Result.Content) != 1 || !strings.Contains(search.Result.Content[0].Text, "deploy-helper") {
		t.Fatalf("search must surface deploy-helper: %s", body)
	}

	// execute_capability returns the SKILL.md body.
	_, body = e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{"name": "execute_capability", "arguments": map[string]any{"capability": "skill:code-review"}},
	})
	if !strings.Contains(string(body), "Follow this skill for code-review") {
		t.Fatalf("execute_capability must return the SKILL.md source: %s", body)
	}

	// get_skill by name, and the plugin: capability form.
	_, body = e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 6, "method": "tools/call",
		"params": map[string]any{"name": "get_skill", "arguments": map[string]any{"name": "code-review"}},
	})
	if !strings.Contains(string(body), "Follow this skill for code-review") {
		t.Fatalf("get_skill must return the SKILL.md source: %s", body)
	}

	_, body = e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": "execute_capability", "arguments": map[string]any{"capability": "plugin:plg_code-review:cfo_code-review"}},
	})
	if !strings.Contains(string(body), "Follow this skill for code-review") {
		t.Fatalf("plugin: capability form must resolve: %s", body)
	}
}

func TestMCPAuthFailuresCarryRemintCodes(t *testing.T) {
	e := newTestEnv(t)

	resp, body := e.mcpPost(t, "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token must 401, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "missing_mcp_token") {
		t.Fatalf("missing token must carry missing_mcp_token, got %s", body)
	}

	resp, body = e.mcpPost(t, "bogus-mcp-token", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token must 401, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "invalid_mcp_token") {
		t.Fatalf("bad token must carry invalid_mcp_token, got %s", body)
	}

	// An expired token has its own code: the client remints on it, and
	// folding it into invalid_mcp_token would hide which of the two happened.
	expired, _, err := e.st.MintMCPToken(context.Background(), aliceSub, e.cfg.OrgID(), "", -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resp, body = e.mcpPost(t, expired, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired token must 401, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "expired_mcp_token") {
		t.Fatalf("expired token must carry expired_mcp_token, got %s", body)
	}
}

func TestMCPUnknownMethodAndResource(t *testing.T) {
	e := newTestEnv(t)
	token := e.mcpToken(t)

	_, body := e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 8, "method": "no/such/method",
	})
	if !strings.Contains(string(body), "-32601") {
		t.Fatalf("unknown method must be a JSON-RPC -32601 error, got %s", body)
	}

	_, body = e.mcpPost(t, token, map[string]any{
		"jsonrpc": "2.0", "id": 9, "method": "resources/read",
		"params": map[string]any{"uri": "skill://nope.json"},
	})
	if !strings.Contains(string(body), "-32002") {
		t.Fatalf("unknown resource must be a JSON-RPC error, got %s", body)
	}
}

// stump.wtf/burrow#16: the MCP path must leave enough in the log to tell
// whether a client's load attempt ever reached burrow, and must never carry a
// token value.
func TestMCPPathLogsOutcomeWithoutTokens(t *testing.T) {
	e := newTestEnv(t)
	var logs bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	token := e.mcpToken(t)

	// A request with no token at all, then an expired/unknown one, then a
	// served initialize.
	e.mcpPost(t, "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	e.mcpPost(t, "not-a-real-token", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "initialize"})
	if resp, _ := e.mcpPost(t, token, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "initialize"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	written := logs.String()
	for _, want := range []string{
		`reason=missing_mcp_token token_present=false`,
		`reason=invalid_mcp_token token_present=true`,
		`msg="mcp: request served" method=initialize`,
		`msg="mcp: token minted"`,
	} {
		if !strings.Contains(written, want) {
			t.Fatalf("log must contain %q, got:\n%s", want, written)
		}
	}
	if strings.Contains(written, token) || strings.Contains(written, e.token) {
		t.Fatalf("log must never contain a token value:\n%s", written)
	}
}

// A database outage on the MCP token lookup is not a bad token. Answered as
// invalid_mcp_token it sends the client off to re-mint (the contract's remint
// codes), which fails the same way; it must be a 500 the client reads as
// burrow being unavailable, logged at Error.
func TestMCPTokenStoreFailureIsNotAnInvalidToken(t *testing.T) {
	e := newTestEnv(t)
	token := e.mcpToken(t)
	logs := captureLogs(t)
	e.st.Close()

	resp, body := e.mcpPost(t, token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("token lookup with the store down must 500, got %d: %s", resp.StatusCode, body)
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("the 500 must still be a JSON-RPC error body: %v: %s", err, body)
	}
	rpcErr := mapOf(t, mapOf(t, payload)["error"])
	// The client re-mints on these codes and on any code containing
	// "expired" or "auth" (docs/client-contract.md, the MCP endpoint).
	answer := strings.ToLower(string(body))
	for _, remint := range []string{"invalid_mcp_token", "missing_mcp_token", "expired", "auth"} {
		if strings.Contains(answer, remint) {
			t.Fatalf("a store failure must not answer with a remint code (%q): %s", remint, body)
		}
	}
	if code := nonEmptyString(t, mapOf(t, rpcErr["data"]), "code"); code != "internal" {
		t.Fatalf("data.code must be internal, got %q", code)
	}
	wantLogged(t, logs, []string{"level=ERROR", `msg="mcp: token lookup failed"`}, token)
}

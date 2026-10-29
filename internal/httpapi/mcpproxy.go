// The MCP proxy: burrow's own tools, bridged to the org's upstream servers
// (Stage C, stump.wtf/burrow#4).
//
// The agent's side does not change: it still calls search_capabilities and
// execute_capability on burrow (contract, "The MCP endpoint"). What Stage C adds
// is that those two answers now include the org's MCP connections, and that a
// tool call runs upstream with the calling member's credential. That is the whole
// reason Stage C exists — the client is told not to register these as local MCP
// entries — and it means three things are decided here and nowhere else:
//
//   - who sees a connection's tools: its audience, the same rule the listing
//     uses, so a connection cannot be reached through the tool path when it is
//     invisible on the list one;
//   - which credential runs it: mcpconnections.go's answer for this member, so a
//     shared connection is shared and a per-member one never borrows anyone else's
//     key;
//   - what a failure looks like: an upstream server that is down, slow, or
//     refuses the credential is reported as what it is, and never as a burrow
//     outage an agent could fix by re-minting its token.
//
// Governing: ADR-0010, ADR-0005 (the host allowlist, enforced again here),
// SPEC-0003 REQ "Visibility".
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
	"gitea.stump.rocks/stump.wtf/burrow/internal/upstream"
)

// mcpCapabilityPrefix marks a capability that lives upstream. A skill is
// skill:<name>, a plugin config object is plugin:<id>:<id>, and an upstream tool
// is mcp:<connection id>:<tool name> — the connection id carries no colon, so
// the tool name is whatever follows the first one, spaces and all.
const mcpCapabilityPrefix = "mcp:"

func mcpSplitCapability(capability string) (connectionID, tool string, ok bool) {
	rest, found := cutPrefix(capability, mcpCapabilityPrefix)
	if !found {
		return "", "", false
	}
	connectionID, tool, found = strings.Cut(rest, ":")
	if !found || connectionID == "" || tool == "" {
		return "", "", false
	}
	return connectionID, tool, true
}

// mcpUpstreamEntries is the connections' tools, in the shape search_capabilities
// answers. A connection that is not usable right now contributes nothing and is
// logged once: an agent's search must not fail because one upstream server is
// having a bad day.
func (s *Server) mcpUpstreamEntries(r *http.Request) []map[string]any {
	groups, err := s.memberGroups(r)
	if err != nil {
		slog.Warn("mcp: could not resolve groups for a capability search", "error", err)
		return nil
	}
	connections, err := s.store.ListMCPConnections(r.Context())
	if err != nil {
		slog.Warn("mcp: could not list connections for a capability search", "error", err)
		return nil
	}
	var out []map[string]any
	for _, c := range connections {
		if !mcpAudienceAllows(c.Groups, groups) {
			continue
		}
		access, err := s.mcpAccess(r.Context(), c, userID(r))
		if err != nil {
			slog.Warn("mcp: connection credential unusable for a capability search", "connection", c.ID, "error", err)
			continue
		}
		if !access.Connected {
			continue
		}
		key := upstream.CacheKey(c.ID, access.Fingerprint)
		if tools, ok := s.mcpCache.Get(key, time.Now()); ok {
			out = append(out, mcpToolEntries(c, tools)...)
			continue
		}
		tools, err := s.mcpUpstream.ListTools(r.Context(), c.URL, access.Present)
		if err != nil {
			slog.Warn("mcp: upstream tool list failed", "connection", c.ID, "url", c.URL, "error", err)
			continue
		}
		s.mcpCache.Put(key, tools, time.Now())
		out = append(out, mcpToolEntries(c, tools)...)
	}
	return out
}

func mcpToolEntries(c store.MCPConnection, tools []upstream.Tool) []map[string]any {
	entries := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		title := tool.Title
		if strings.TrimSpace(title) == "" {
			title = c.Name + ": " + tool.Name
		}
		entries = append(entries, map[string]any{
			"name":        c.Name + " " + tool.Name,
			"title":       title,
			"description": tool.Description,
			"capability":  mcpCapabilityPrefix + c.ID + ":" + tool.Name,
			"url":         "mcp://" + c.ID + "/" + tool.Name,
		})
	}
	return entries
}

// mcpUpstreamCall runs one upstream tool and hands its result to the agent
// verbatim, so a server's own content blocks and isError flag survive the hop.
func (s *Server) mcpUpstreamCall(r *http.Request, capability, input string) (any, *jsonrpcError) {
	connectionID, tool, ok := mcpSplitCapability(capability)
	if !ok {
		return nil, &jsonrpcError{Code: -32602, Message: "malformed mcp capability: " + capability}
	}
	c, err := s.store.GetMCPConnection(r.Context(), connectionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &jsonrpcError{Code: -32002, Message: "unknown capability: " + capability}
		}
		slog.Error("mcp: could not read the connection behind a capability", "capability", capability, "error", err)
		return nil, &jsonrpcError{Code: -32003, Message: "could not read that connection"}
	}
	groups, err := s.memberGroups(r)
	if err != nil || !mcpAudienceAllows(c.Groups, groups) {
		// A member outside the audience is answered exactly as an unknown
		// capability, which is what the listing already promises.
		return nil, &jsonrpcError{Code: -32002, Message: "unknown capability: " + capability}
	}
	access, err := s.mcpAccess(r.Context(), c, userID(r))
	if err != nil {
		slog.Warn("mcp: connection credential unusable", "connection", c.ID, "error", err)
		return nil, &jsonrpcError{Code: -32001, Message: "this connection needs you to connect it first"}
	}
	if !access.Connected {
		return nil, &jsonrpcError{Code: -32001, Message: "this connection needs you to connect it first"}
	}
	args, err := mcpToolArgs(input)
	if err != nil {
		return nil, &jsonrpcError{Code: -32602, Message: err.Error()}
	}
	result, err := s.mcpUpstream.CallTool(r.Context(), c.URL, access.Present, tool, args)
	if err != nil {
		if errors.Is(err, upstream.ErrUnauthorized) {
			slog.Warn("mcp: upstream refused the credential", "connection", c.ID, "tool", tool, "error", err)
			return nil, &jsonrpcError{Code: -32001, Message: c.Name + " refused the stored credential; it needs to be connected again"}
		}
		if errors.Is(err, upstream.ErrHostNotAllowed) {
			slog.Error("mcp: connection host is outside the allowlist", "connection", c.ID, "error", err)
			return nil, &jsonrpcError{Code: -32003, Message: "that connection points outside the hosts burrow is allowed to reach"}
		}
		// An error the server itself answered with is the server's own news, and
		// the agent can act on it; a transport failure is burrow's, and gets
		// burrow's sentence.
		var refused *upstream.JSONRPCError
		if errors.As(err, &refused) {
			slog.Info("mcp: upstream refused the call", "connection", c.ID, "tool", tool, "code", refused.Code, "error", refused.Message)
			return nil, &jsonrpcError{Code: -32003, Message: "the upstream server refused that call: " + clampUpstreamMessage(refused.Message)}
		}
		slog.Warn("mcp: upstream tool call failed", "connection", c.ID, "tool", tool, "error", err)
		return nil, &jsonrpcError{Code: -32003, Message: "the upstream server could not run that tool"}
	}
	return mcpResultContent(result)
}

// mcpToolArgs turns the contract's optional input string into tool arguments: a
// JSON object is passed through as the arguments, anything else becomes the
// input of a one-field call.
func mcpToolArgs(input string) (map[string]any, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(trimmed), &args); err == nil {
		if args == nil {
			args = map[string]any{}
		}
		return args, nil
	}
	return map[string]any{"input": trimmed}, nil
}

// clampUpstreamMessage keeps an upstream server's own error sentence short: it
// is another operator's words, shown to a member's agent, and a server can
// answer with anything.
func clampUpstreamMessage(message string) string {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return "no reason given"
	}
	if runes := []rune(trimmed); len(runes) > 200 {
		return string(runes[:200]) + "…"
	}
	return trimmed
}

// mcpResultContent passes an upstream tools/call result through. A payload that
// is not the usual {content:[…]} shape is handed back as one text block rather
// than dropped, so nothing an upstream server returns is lost on the way.
func mcpResultContent(result json.RawMessage) (any, *jsonrpcError) {
	if len(result) == 0 {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": "(no result)"}}}, nil
	}
	var shaped struct {
		Content json.RawMessage `json:"content"`
		IsError bool            `json:"isError"`
	}
	if err := json.Unmarshal(result, &shaped); err == nil && len(shaped.Content) > 0 {
		var content []map[string]any
		if err := json.Unmarshal(shaped.Content, &content); err == nil && len(content) > 0 {
			out := map[string]any{"content": content}
			if shaped.IsError {
				out["isError"] = true
			}
			return out, nil
		}
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(result)}}}, nil
}

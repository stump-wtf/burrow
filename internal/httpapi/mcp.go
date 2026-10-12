package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// The MCP endpoint: how the agent gets skills (contract, "The MCP endpoint").
// The client mints a token via POST /v1/mcp/token, then talks Streamable HTTP
// JSON-RPC to <resource>/agent, which must end in /mcp/agent.

// mcpProtocolVersion is the newest revision the client's transport accepts
// (apps/server/src/connect-mcp-transport.ts).
const mcpProtocolVersion = "2025-06-18"

// skillIndexURI is the discovery resource the client reads to build the
// agent's skill catalog.
const skillIndexURI = "skill://index.json"

// handleMCPToken answers POST /v1/mcp/token: mint an org-scoped MCP token.
// resource is the origin's /mcp, so <resource>/agent ends in /mcp/agent.
func (s *Server) handleMCPToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scopes []string `json:"scopes"`
	}
	_ = decodeBody(r, &body)
	token, expiresAt, err := s.store.MintMCPToken(r.Context(), userID(r), s.cfg.OrgID(), s.cfg.MCPTokenTTL)
	if err != nil {
		slog.Error("mcp: token mint failed", "error", err)
		writeError(w, http.StatusInternalServerError, "server_error", "could not mint an MCP token")
		return
	}
	// The minted value is never logged: this line is the answer to "did the
	// client ever get a token at all", which is the first question when an
	// MCP load fails on the client side and says nothing about the server.
	slog.Info("mcp: token minted", "user", userID(r), "expires_at", expiresAt.Format(time.RFC3339))
	noStore(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"token":          token,
		"expiresAt":      expiresAt.Format(time.RFC3339),
		"organizationId": s.cfg.OrgID(),
		"resource":       s.origin(r) + "/mcp",
		"scopes":         []string{"mcp:read", "mcp:write"},
	})
}

// jsonrpcMessage is one decoded JSON-RPC message; ID is nil for notifications.
type jsonrpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (s *Server) writeMCPError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string, data any) {
	writeJSON(w, status, map[string]any{
		"jsonrpc": "2.0",
		"id":      idOrNull(id),
		"error":   jsonrpcError{Code: code, Message: message, Data: data},
	})
}

func idOrNull(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return id
}

// handleMCPAgent is the Streamable HTTP MCP endpoint. JSON responses only —
// the client's transport accepts either JSON or SSE.
// Logging on this path exists because of stump.wtf/burrow#16: the desktop app
// reported "couldn't load the MCP openwork-cloud" during an edge-TLS outage,
// and nothing burrow logged could show whether the client's attempts ever
// arrived. Log the method, the outcome, and whether a token was presented —
// never a token value.
func (s *Server) handleMCPAgent(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	token := bearerToken(r)
	if token == "" {
		slog.Warn("mcp: request refused", "reason", "missing_mcp_token", "token_present", false)
		s.writeMCPError(w, http.StatusUnauthorized, nil, -32001, "missing_mcp_token", map[string]string{"code": "missing_mcp_token"})
		return
	}
	mcpUser, _, err := s.store.LookupMCPToken(r.Context(), token)
	if err != nil {
		var code string
		switch {
		case errors.Is(err, store.ErrExpired):
			code = "expired_mcp_token"
		case errors.Is(err, store.ErrNotFound):
			code = "invalid_mcp_token"
		default:
			// A store failure is not a token problem. The token codes
			// send the client off to re-mint (docs/client-contract.md,
			// "The MCP endpoint"), which cannot fix an outage.
			slog.Error("mcp: token lookup failed", "token_present", true, "error", err)
			s.writeMCPError(w, http.StatusInternalServerError, nil, -32003, "token store unavailable", map[string]string{"code": "internal"})
			return
		}
		slog.Warn("mcp: request refused", "reason", code, "token_present", true, "error", err)
		s.writeMCPError(w, http.StatusUnauthorized, nil, -32001, code, map[string]string{"code": code})
		return
	}
	// The token's member goes onto the request context, so the Library filter
	// in library.go serves the agent exactly what that member sees signed in.
	r = r.WithContext(context.WithValue(r.Context(), userIDKey, mcpUser))

	var msg jsonrpcMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&msg); err != nil {
		slog.Warn("mcp: unreadable request body", "error", err)
		s.writeMCPError(w, http.StatusBadRequest, nil, -32700, "parse error", nil)
		return
	}

	// Notifications (no id) get HTTP 202 with an empty body — the client's
	// transport requires exactly that for notifications/initialized.
	if len(msg.ID) == 0 {
		slog.Debug("mcp: notification", "method", msg.Method)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rpcErr := s.dispatchMCP(r, msg)
	if rpcErr != nil {
		slog.Warn("mcp: request failed", "method", msg.Method, "code", rpcErr.Code, "message", rpcErr.Message, "elapsed", time.Since(start))
		s.writeMCPError(w, http.StatusOK, msg.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		return
	}
	slog.Info("mcp: request served", "method", msg.Method, "elapsed", time.Since(start))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      msg.ID,
		"result":  result,
	})
}

func (s *Server) dispatchMCP(r *http.Request, msg jsonrpcMessage) (any, *jsonrpcError) {
	switch msg.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": false},
				"resources": map[string]any{"subscribe": false, "listChanged": false},
			},
			"serverInfo": map[string]any{
				"name":    "burrow",
				"version": buildinfo.Version,
			},
			"instructions": "burrow serves this organization's skills. Call search_capabilities to find a skill, execute_capability to load one, get_skill to read a skill by name, or read the skill://index.json resource for the full catalog.",
		}, nil

	case "tools/list":
		return map[string]any{"tools": mcpToolDefs()}, nil

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return nil, &jsonrpcError{Code: -32602, Message: "invalid tools/call params"}
		}
		return s.callTool(r, params.Name, params.Arguments)

	case "resources/list":
		return map[string]any{
			"resources": []map[string]any{{
				"uri":         skillIndexURI,
				"name":        "Skill index",
				"description": "The org's skill catalog (agentskills.io discovery schema).",
				"mimeType":    "application/json",
			}},
		}, nil

	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return nil, &jsonrpcError{Code: -32602, Message: "invalid resources/read params"}
		}
		if params.URI != skillIndexURI {
			return nil, &jsonrpcError{Code: -32002, Message: "unknown resource: " + params.URI}
		}
		index, err := s.skillIndex(r)
		if err != nil {
			slog.Error("mcp: skill index failed", "error", err)
			return nil, &jsonrpcError{Code: -32003, Message: "could not load the skill index"}
		}
		return map[string]any{
			"contents": []map[string]any{{
				"uri":      skillIndexURI,
				"mimeType": "application/json",
				"text":     string(index),
			}},
		}, nil

	case "ping":
		return map[string]any{}, nil

	default:
		return nil, &jsonrpcError{Code: -32601, Message: "method not found: " + msg.Method}
	}
}

// mcpToolDefs describes burrow's tools. The client requires the first two by
// name; their schemas are burrow's own design (the contract leaves them
// unspecified).
func mcpToolDefs() []map[string]any {
	return []map[string]any{
		{
			"name":        "search_capabilities",
			"description": "Search the org's skills. Every whitespace-separated term must appear in the skill's name, title, or description (case-insensitive). An empty query returns the whole catalog.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Search terms; empty for all skills."},
				},
			},
		},
		{
			"name":        "execute_capability",
			"description": "Load a skill so it can be followed. capability is the string from the skill index (skill:<name> or plugin:<pluginId>:<configObjectId>). Returns the full SKILL.md source; read it and follow it for the rest of the task.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"capability"},
				"properties": map[string]any{
					"capability": map[string]any{"type": "string", "description": "The capability string from skill://index.json."},
					"input":      map[string]any{"type": "string", "description": "Optional context to pass along."},
				},
			},
		},
		{
			"name":        "get_skill",
			"description": "Read one skill's full SKILL.md source by its kebab-case name.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"name"},
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "description": "The skill's kebab-case name."},
				},
			},
		},
	}
}

func (s *Server) callTool(r *http.Request, name string, args json.RawMessage) (any, *jsonrpcError) {
	switch name {
	case "search_capabilities":
		var params struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(args, &params)
		visible, err := s.library(r)
		if err != nil {
			slog.Error("mcp: Library search failed", "error", err)
			return nil, &jsonrpcError{Code: -32003, Message: "could not load the Library"}
		}
		matches := skills.Search(visible, params.Query)
		entries := make([]map[string]any, 0, len(matches))
		for _, skill := range matches {
			entries = append(entries, map[string]any{
				"name":        skill.Name,
				"title":       skill.Title,
				"description": skills.ClampDescription(skill.Description),
				"capability":  "skill:" + skill.Name,
				"url":         "skill://" + skill.Name,
			})
		}
		return textResult(entries, len(matches))

	case "execute_capability":
		var params struct {
			Capability string `json:"capability"`
		}
		if err := json.Unmarshal(args, &params); err != nil || params.Capability == "" {
			return nil, &jsonrpcError{Code: -32602, Message: "execute_capability requires a capability"}
		}
		skill, ok, err := s.skillByCapability(r, params.Capability)
		if err != nil {
			slog.Error("mcp: Library lookup failed", "capability", params.Capability, "error", err)
			return nil, &jsonrpcError{Code: -32003, Message: "could not load the Library"}
		}
		if !ok {
			return nil, &jsonrpcError{Code: -32002, Message: "unknown capability: " + params.Capability}
		}
		return map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": "The skill follows. Read it and follow it for the rest of the task.\n\n" + skill.Body,
			}},
		}, nil

	case "get_skill":
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(args, &params); err != nil || params.Name == "" {
			return nil, &jsonrpcError{Code: -32602, Message: "get_skill requires a name"}
		}
		skill, ok, err := s.skillByName(r, params.Name)
		if err != nil {
			slog.Error("mcp: skill read failed", "skill", params.Name, "error", err)
			return nil, &jsonrpcError{Code: -32003, Message: "could not load the Library"}
		}
		if !ok {
			return nil, &jsonrpcError{Code: -32002, Message: "unknown skill: " + params.Name}
		}
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": skill.Body}},
		}, nil

	default:
		return nil, &jsonrpcError{Code: -32601, Message: "unknown tool: " + name}
	}
}

func textResult(entries []map[string]any, count int) (any, *jsonrpcError) {
	encoded, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil, &jsonrpcError{Code: -32003, Message: "could not encode results"}
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(encoded)}},
	}, nil
}

// skillByCapability resolves a capability string from the discovery index:
// skill:<name> or plugin:<pluginId>:<configObjectId>.
func (s *Server) skillByCapability(r *http.Request, capability string) (skills.Skill, bool, error) {
	list, err := s.library(r)
	if err != nil {
		return skills.Skill{}, false, err
	}
	rest, ok := cutPrefix(capability, "skill:")
	if !ok {
		if _, isPlugin := cutPrefix(capability, "plugin:"); isPlugin {
			for _, skill := range list {
				if "plugin:"+pluginID(skill.Name)+":"+configObjectID(skill.Name) == capability {
					return skill, true, nil
				}
			}
		}
		return skills.Skill{}, false, nil
	}
	for _, skill := range list {
		if skill.Name == rest {
			return skill, true, nil
		}
	}
	return skills.Skill{}, false, nil
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

// skillIndex builds the skill://index.json document against the
// agentskills.io discovery schema the client enforces (name kebab-case ≤64,
// type skill-md, description ≤1024, url skill://, capability skill:/plugin:).
func (s *Server) skillIndex(r *http.Request) ([]byte, error) {
	list, err := s.library(r)
	if err != nil {
		return nil, err
	}
	indexSkills := make([]map[string]any, 0, len(list))
	for _, skill := range list {
		entry := map[string]any{
			"name":            skill.Name,
			"type":            "skill-md",
			"title":           skill.Title,
			"description":     skills.ClampDescription(skill.Description),
			"url":             "skill://" + skill.Name,
			"capability":      "skill:" + skill.Name,
			"marketplaceName": s.cfg.OrgName + " Library",
			"pluginName":      skill.Title,
		}
		indexSkills = append(indexSkills, entry)
	}
	return json.MarshalIndent(map[string]any{
		"$schema": "https://schemas.agentskills.io/discovery/0.2.0/schema.json",
		"skills":  indexSkills,
	}, "", "  ")
}

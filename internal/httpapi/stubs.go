package httpapi

import (
	"net/http"
)

// Stage A stubs: endpoints the client polls but whose stage has not landed.
// Each returns exactly the empty the client contract prescribes.

// handleLLMProviders answers GET /v1/llm-providers (polled). Stage B fills it
// with LiteLLM-backed providers (ADR-0001).
func (s *Server) handleLLMProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"llmProviders": []any{}})
}

// handleMCPConnections answers GET /v1/mcp-connections: an empty connections
// list until Stage C.
func (s *Server) handleMCPConnections(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"connections": []any{}})
}

// handleInferenceProviders answers GET /v1/inference-providers with a 404 —
// the client treats the miss as "none" (contract, Stage A table).
func (s *Server) handleInferenceProviders(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "no inference providers")
}

// handleRuntimeConfig answers GET /api/runtime-config at the origin root:
// the client reads only denApiUrl, with a 2-second timeout.
func (s *Server) handleRuntimeConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"denApiUrl": s.origin(r) + "/api/den",
	})
}

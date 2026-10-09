package httpapi

import (
	"net/http"
)

// Org LLM providers (contract, Stage B): rows the desktop client syncs into
// its model config, plus the /connect response that hands over the key.
//
// Governing: SPEC stage-b-org-models. The strict rules (lpr_ ids, source
// custom, complete model rows) are validated at load time by the providers
// package so burrow can never serve a row that fails the client's sync.

// handleLLMProviders answers GET /v1/llm-providers (and ?scope=usable —
// every org provider is usable). With no providers file configured this
// returns an empty list, the Stage A behavior.
func (s *Server) handleLLMProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"llmProviders": s.providers.Rows()})
}

// handleLLMProviderConnect answers GET /v1/llm-providers/:id/connect:
// {llmProvider:{…row…, apiKey}} where apiKey is present only when the
// provider's environment variable is set (the client writes it into env).
func (s *Server) handleLLMProviderConnect(w http.ResponseWriter, r *http.Request) {
	row, apiKey, ok := s.providers.Connect(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such llm provider")
		return
	}
	payload := map[string]any{
		"id":             row.ID,
		"providerId":     row.ProviderID,
		"name":           row.Name,
		"source":         row.Source,
		"providerConfig": row.ProviderConfig,
		"hasApiKey":      row.HasAPIKey,
		"models":         row.Models,
		"updatedAt":      row.UpdatedAt,
	}
	if apiKey != nil {
		payload["apiKey"] = *apiKey
	}
	writeJSON(w, http.StatusOK, map[string]any{"llmProvider": payload})
}

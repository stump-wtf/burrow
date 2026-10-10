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
// every provider a member can see is usable). Each member gets only the rows
// and models their groups allow (SPEC-0003 REQ "Visibility"). With no
// providers file configured this returns an empty list, the Stage A behavior.
func (s *Server) handleLLMProviders(w http.ResponseWriter, r *http.Request) {
	groups, err := s.memberGroups(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not load the member")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"llmProviders": s.providers.RowsFor(groups)})
}

// handleLLMProviderConnect answers GET /v1/llm-providers/:id/connect:
// {llmProvider:{…row…, apiKey}} where apiKey is present only when the
// provider's environment variable is set (the client writes it into env).
// A row the member cannot see answers 404 like an unknown id, so its key
// never leaves burrow (SPEC-0003 REQ "Connect follows visibility").
func (s *Server) handleLLMProviderConnect(w http.ResponseWriter, r *http.Request) {
	groups, err := s.memberGroups(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not load the member")
		return
	}
	row, apiKey, ok := s.providers.ConnectFor(r.PathValue("id"), groups)
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

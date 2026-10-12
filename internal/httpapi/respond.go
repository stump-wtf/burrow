// Package httpapi implements burrow's HTTP surface: the sign-in pages, the
// desktop client's /api/den control-plane routes, and the MCP endpoint that
// delivers skills to the agent.
//
// Governing: ADR-0001 (clean-room control plane), the stage-a-control-plane
// spec, and docs/client-contract.md (the client contract — exact field names
// and error codes below come from it).
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
)

// writeJSON writes v as a JSON response body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError is the error body the client contract specifies:
// {error:<code>, message?, details?}.
type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// writeError writes a contract-shaped API error.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Error: code, Message: message})
}

// bodyTooLarge reports whether a body read failed on the cap Routes puts on
// every request (limitBody), as opposed to on what the body said.
func bodyTooLarge(err error) bool {
	var tooLarge *http.MaxBytesError
	return errors.As(err, &tooLarge)
}

// writeBodyTooLarge answers a request whose body ran past that cap. It is a
// 413 of its own so an oversized body never reads as a malformed one or as a
// refused credential.
func writeBodyTooLarge(w http.ResponseWriter) {
	writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
}

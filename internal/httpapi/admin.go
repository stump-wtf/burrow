package httpapi

// Admin surface: CRUD for provider rows, the audit trail, and machine API
// tokens. These routes are burrow-designed (outside the client contract):
// error codes are burrow's own, unknown JSON fields are rejected rather than
// ignored, and etags are the row's quoted updated_at (SPEC-0004 REQ "Admin
// surface", "Admin authorization", "Audit").
//
// Governing: SPEC-0004, ADR-0004.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

const (
	// ScopeLLMAdmin is the only scope with meaning today: it lets an API
	// token reach the provider admin surface.
	ScopeLLMAdmin = "llm-admin"

	adminAuditDefaultLimit = 50
	adminAuditMaxLimit     = 500
)

const (
	adminActorKey ctxKey = iota + 1 // audit identity: user id or token name
	adminUserKey                    // user id for the api_tokens FK lineage
)

// requireAdmin gates the admin surface with one middleware and two
// credentials (SPEC-0004 design): a bat_-prefixed bearer is an API token and
// fails as invalid_token — automation must never mistake a revoked token for
// a dead session — anything else is a session, keeping the contract's
// session-terminating vocabulary for sessions only, plus the
// BURROW_ADMIN_GROUPS check. With no groups configured and no tokens minted
// the surface is closed to everyone.
func (s *Server) requireAdmin(next func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "invalid_token", "missing bearer token")
			return
		}
		if strings.HasPrefix(token, store.AdminTokenPrefix) {
			tok, err := s.store.LookupAPIToken(r.Context(), token)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrExpired) {
					writeError(w, http.StatusUnauthorized, "invalid_token", "API token is unknown, revoked, or expired")
					return
				}
				slog.Error("api token lookup failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal", "token store unavailable")
				return
			}
			if !hasScope(tok.Scopes, ScopeLLMAdmin) {
				writeError(w, http.StatusUnauthorized, "invalid_token", "API token lacks the "+ScopeLLMAdmin+" scope")
				return
			}
			ctx := context.WithValue(r.Context(), adminActorKey, tok.Name)
			ctx = context.WithValue(ctx, adminUserKey, tok.UserID)
			next(w, r.WithContext(ctx))
			return
		}
		userID, err := s.store.LookupSession(r.Context(), token)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrExpired):
				writeError(w, http.StatusUnauthorized, "session_expired", "session has expired")
			case errors.Is(err, store.ErrNotFound):
				writeError(w, http.StatusUnauthorized, "invalid_session", "session not found")
			default:
				slog.Error("session lookup failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal", "session store unavailable")
			}
			return
		}
		user, err := s.store.GetUser(r.Context(), userID)
		if err != nil {
			slog.Error("load member for admin check", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "member store unavailable")
			return
		}
		if !holdsAny(user.Groups, s.cfg.AdminGroups) {
			writeError(w, http.StatusForbidden, "admin_required", "not a member of an admin group")
			return
		}
		ctx := context.WithValue(r.Context(), adminActorKey, userID)
		ctx = context.WithValue(ctx, adminUserKey, userID)
		next(w, r.WithContext(ctx))
	})
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// holdsAny is the intersection test behind the group gate: an empty allow
// list (BURROW_ADMIN_GROUPS unset) admits nobody — that is how the surface
// stays closed until an operator opens it.
func holdsAny(groups, allow []string) bool {
	for _, have := range groups {
		for _, want := range allow {
			if have == want {
				return true
			}
		}
	}
	return false
}

func adminActor(r *http.Request) string {
	actor, _ := r.Context().Value(adminActorKey).(string)
	return actor
}

func adminUser(r *http.Request) string {
	id, _ := r.Context().Value(adminUserKey).(string)
	return id
}

// adminProvider is one row on the admin wire: the stored fields plus the
// per-row etag. It carries no model menu — menus are derived, not stored —
// and never a key value, only the environment variable's name.
type adminProvider struct {
	ID            string            `json:"id"`
	ProviderID    string            `json:"providerId"`
	Name          string            `json:"name"`
	BaseURL       string            `json:"baseURL"`
	APIKeyEnv     string            `json:"apiKeyEnv"`
	Groups        []string          `json:"groups,omitempty"`
	NameOverrides map[string]string `json:"nameOverrides,omitempty"`
	Etag          string            `json:"etag"`
}

func adminRowOf(p store.ProviderRow) adminProvider {
	return adminProvider{
		ID:            p.ID,
		ProviderID:    p.ProviderID,
		Name:          p.Name,
		BaseURL:       p.BaseURL,
		APIKeyEnv:     p.APIKeyEnv,
		Groups:        p.Groups,
		NameOverrides: p.NameOverrides,
		Etag:          quoteETag(p.UpdatedAt),
	}
}

// quoteETag is the row's etag: the quoted RFC3339Nano updated_at, matching
// the row's updatedAt field exactly so If-Match round-trips (SPEC-0004
// design: "ETag = quoted updated_at").
func quoteETag(t time.Time) string {
	return `"` + providers.FormatUpdatedAt(t) + `"`
}

// unquoteETag accepts a bare or quoted (and optionally weak-) tag.
func unquoteETag(tag string) string {
	tag = strings.TrimSpace(tag)
	tag = strings.TrimPrefix(tag, "W/")
	return strings.Trim(tag, `"`)
}

// decodeStrict decodes a JSON body, rejecting unknown fields — a typo in an
// automation's payload must fail loudly, not silently drop a field
// (SPEC-0004 REQ "Admin surface"). The body is one value: Decode stops at
// the end of the first, so anything after it (a second object, a stray
// token) would otherwise be dropped just as silently as an unknown field.
func decodeStrict(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

func (s *Server) handleAdminListProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListProviders(r.Context())
	if err != nil {
		s.adminStoreError(w, "list providers", err)
		return
	}
	out := make([]adminProvider, 0, len(rows))
	for _, p := range rows {
		out = append(out, adminRowOf(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"llmProviders": out})
}

func (s *Server) handleAdminGetProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetProvider(r.Context(), r.PathValue("id"))
	if err != nil {
		s.adminProviderError(w, err)
		return
	}
	row := adminRowOf(p)
	w.Header().Set("ETag", row.Etag)
	writeJSON(w, http.StatusOK, map[string]any{"llmProvider": row})
}

func (s *Server) handleAdminCreateProvider(w http.ResponseWriter, r *http.Request) {
	var body adminProvider
	if err := decodeStrict(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be one JSON provider object; unknown fields are rejected: "+err.Error())
		return
	}
	row := body.toStoreRow()
	if err := providers.ValidateStoreRow(row); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_row", err.Error())
		return
	}
	if err := s.store.CreateProvider(r.Context(), row, adminActor(r)); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			writeError(w, http.StatusConflict, "duplicate_id", "a provider with id "+row.ID+" already exists")
			return
		}
		s.adminStoreError(w, "create provider", err)
		return
	}
	created, err := s.store.GetProvider(r.Context(), row.ID)
	if err != nil {
		s.adminStoreError(w, "read created provider", err)
		return
	}
	out := adminRowOf(created)
	w.Header().Set("ETag", out.Etag)
	writeJSON(w, http.StatusCreated, map[string]any{"llmProvider": out})
}

// handleAdminUpdateProvider is a full replace behind an etag: a missing
// If-Match is 428 rather than a silent clobber — the whole point of the API
// is automation without lost updates — and a stale If-Match is 412 with the
// current row in the body so the caller can rebase in one round trip.
func (s *Server) handleAdminUpdateProvider(w http.ResponseWriter, r *http.Request) {
	current, err := s.store.GetProvider(r.Context(), r.PathValue("id"))
	if err != nil {
		s.adminProviderError(w, err)
		return
	}
	var body adminProvider
	if err := decodeStrict(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be one JSON provider object; unknown fields are rejected: "+err.Error())
		return
	}
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		writeError(w, http.StatusPreconditionRequired, "precondition_required", "PUT must carry If-Match with the row's current etag")
		return
	}
	expected, err := time.Parse(time.RFC3339Nano, unquoteETag(ifMatch))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "If-Match must be the row's etag: the quoted RFC3339Nano updated_at")
		return
	}
	if !expected.Equal(current.UpdatedAt) {
		writeETagStale(w, current)
		return
	}
	if body.ID != "" && body.ID != current.ID {
		writeError(w, http.StatusBadRequest, "invalid_request", "id in body must match the path")
		return
	}
	row := body.toStoreRow()
	row.ID = current.ID
	if err := providers.ValidateStoreRow(row); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_row", err.Error())
		return
	}
	// The store re-checks the precondition inside its row lock, so a write
	// that raced between this handler's read and its transaction still
	// answers 412 rather than clobbering the winner.
	if _, err := s.store.UpdateProvider(r.Context(), row, expected, adminActor(r)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such llm provider")
			return
		}
		if errors.Is(err, store.ErrStale) {
			current, err = s.store.GetProvider(r.Context(), current.ID)
			if err != nil {
				s.adminStoreError(w, "read current provider", err)
				return
			}
			writeETagStale(w, current)
			return
		}
		s.adminStoreError(w, "update provider", err)
		return
	}
	updated, err := s.store.GetProvider(r.Context(), current.ID)
	if err != nil {
		s.adminStoreError(w, "read updated provider", err)
		return
	}
	out := adminRowOf(updated)
	w.Header().Set("ETag", out.Etag)
	writeJSON(w, http.StatusOK, map[string]any{"llmProvider": out})
}

// writeETagStale answers 412 with the current row in the body so the caller
// can rebase in one round trip (SPEC-0004 design: "ETag = quoted updated_at").
func writeETagStale(w http.ResponseWriter, current store.ProviderRow) {
	writeJSON(w, http.StatusPreconditionFailed, map[string]any{
		"error":       "precondition_failed",
		"message":     "If-Match does not match the row's current etag; body carries the current row",
		"llmProvider": adminRowOf(current),
	})
}

func (s *Server) handleAdminDeleteProvider(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteProvider(r.Context(), r.PathValue("id"), adminActor(r)); err != nil {
		s.adminProviderError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// toStoreRow maps a wire row onto a store row. The path id is applied by
// the caller; timestamps belong to the store.
func (b adminProvider) toStoreRow() store.ProviderRow {
	return store.ProviderRow{
		ID:            b.ID,
		ProviderID:    b.ProviderID,
		Name:          b.Name,
		BaseURL:       b.BaseURL,
		APIKeyEnv:     b.APIKeyEnv,
		Groups:        b.Groups,
		NameOverrides: b.NameOverrides,
	}
}

func (s *Server) adminProviderError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such llm provider")
		return
	}
	s.adminStoreError(w, "provider", err)
}

func (s *Server) adminStoreError(w http.ResponseWriter, what string, err error) {
	slog.Error("admin "+what+" failed", "error", err)
	writeError(w, http.StatusInternalServerError, "internal", "store unavailable")
}

// handleAdminAudit answers GET /v1/admin/audit?limit= newest-first, for the
// console and for incident review (SPEC-0004 REQ "Audit").
func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	limit := adminAuditDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be a positive integer")
			return
		}
		if n > adminAuditMaxLimit {
			n = adminAuditMaxLimit
		}
		limit = n
	}
	entries, err := s.store.ListAudit(r.Context(), limit)
	if err != nil {
		s.adminStoreError(w, "list audit", err)
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"id":         e.ID,
			"createdAt":  e.CreatedAt.UTC().Format(time.RFC3339Nano),
			"actor":      e.Actor,
			"action":     e.Action,
			"providerId": e.ProviderID,
			"before":     e.Before,
			"after":      e.After,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": out})
}

// adminAPIToken is one token on the admin wire. The hash is listed so
// DELETE can name the token it revokes; the raw value exists only in the
// mint response, once.
type adminAPIToken struct {
	Hash       string   `json:"hash"`
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	CreatedAt  string   `json:"createdAt"`
	ExpiresAt  string   `json:"expiresAt"`
	LastUsedAt *string  `json:"lastUsedAt,omitempty"`
}

func adminTokenOf(t store.APIToken) adminAPIToken {
	out := adminAPIToken{
		Hash:      t.TokenHash,
		Name:      t.Name,
		Scopes:    t.Scopes,
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt: t.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
	if t.LastUsedAt != nil {
		stamp := t.LastUsedAt.UTC().Format(time.RFC3339Nano)
		out.LastUsedAt = &stamp
	}
	return out
}

func (s *Server) handleAdminListTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.ListAPITokens(r.Context())
	if err != nil {
		s.adminStoreError(w, "list api tokens", err)
		return
	}
	out := make([]adminAPIToken, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, adminTokenOf(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"apiTokens": out})
}

// handleAdminMintToken answers POST /v1/admin/api-tokens: the raw token is
// shown exactly here, exactly once; every later read shows the hash.
func (s *Server) handleAdminMintToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := decodeStrict(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be {name, scopes?}; unknown fields are rejected: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name is required so the audit names the machine behind each change")
		return
	}
	if body.Scopes == nil {
		body.Scopes = []string{ScopeLLMAdmin}
	}
	for _, scope := range body.Scopes {
		if strings.TrimSpace(scope) == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "scopes must not contain an empty entry")
			return
		}
	}
	raw, tok, err := s.store.MintAPIToken(r.Context(), adminUser(r), body.Name, body.Scopes, s.cfg.APITokenTTL)
	if err != nil {
		s.adminStoreError(w, "mint api token", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": raw, "apiToken": adminTokenOf(tok)})
}

// handleAdminRevokeToken deletes by hash or raw value — whichever the caller
// holds (DELETE /v1/admin/api-tokens/{hash-or-id}).
func (s *Server) handleAdminRevokeToken(w http.ResponseWriter, r *http.Request) {
	err := s.store.RevokeAPIToken(r.Context(), r.PathValue("hashOrToken"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such api token")
		return
	}
	if err != nil {
		s.adminStoreError(w, "revoke api token", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/config"
	"gitea.stump.rocks/stump.wtf/burrow/internal/oidc"
	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// OIDCClient is the slice of the OIDC flow the API drives; production binds
// oidc.Client, tests bind a fake issuer.
type OIDCClient interface {
	AuthCodeURL(state, codeVerifier string) string
	Exchange(ctx context.Context, code, codeVerifier string) (string, error)
	Verify(ctx context.Context, rawIDToken string) (oidc.Claims, error)
}

// Server carries burrow's handlers.
type Server struct {
	cfg       *config.Config
	store     *store.Store
	skills    *skills.Catalog
	providers *providers.Runtime // nil serves none
	oidc      OIDCClient
	adminAPI  *http.ServeMux // the /api/den/v1/admin subtree, driven in-process by the /admin console
}

// New builds the server.
func New(cfg *config.Config, st *store.Store, catalog *skills.Catalog, runtime *providers.Runtime, client OIDCClient) *Server {
	return &Server{cfg: cfg, store: st, skills: catalog, providers: runtime, oidc: client}
}

// Routes assembles the full router. Every /v1 route lives under /api/den (the
// client's default API base for a non-openworklabs origin); /api/runtime-config
// and the MCP endpoint live at the origin root, exactly where the client looks.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /auth/oidc/start", s.handleOIDCStart)
	mux.HandleFunc("GET /auth/oidc/callback", s.handleOIDCCallback)
	mux.HandleFunc("GET /api/runtime-config", s.handleRuntimeConfig)
	mux.HandleFunc("POST /mcp/agent", s.handleMCPAgent)

	// The /admin console (SPEC-0004 REQ "Admin console"): server-rendered
	// pages over the write API, gated by the same BURROW_ADMIN_GROUPS check.
	mux.HandleFunc("GET /admin/{$}", s.requireAdminPage(s.handleConsoleList))
	mux.HandleFunc("GET /admin", s.requireAdminPage(s.handleConsoleList))
	mux.HandleFunc("GET /admin/audit", s.requireAdminPage(s.handleConsoleAudit))
	mux.HandleFunc("GET /admin/preview", s.requireAdminPage(s.handleConsolePreview))
	mux.HandleFunc("GET /admin/providers/new", s.requireAdminPage(s.handleConsoleNew))
	mux.HandleFunc("POST /admin/providers", s.requireAdminPage(s.handleConsoleCreate))
	mux.HandleFunc("GET /admin/providers/{id}", s.requireAdminPage(s.handleConsoleEdit))
	mux.HandleFunc("POST /admin/providers/{id}", s.requireAdminPage(s.handleConsoleUpdate))
	mux.HandleFunc("POST /admin/providers/{id}/delete", s.requireAdminPage(s.handleConsoleDelete))

	api := http.NewServeMux()
	api.HandleFunc("POST /v1/auth/desktop-handoff", s.requireSession(s.handleMintHandoff))
	api.HandleFunc("POST /v1/auth/desktop-handoff/exchange", s.handleExchange)
	api.HandleFunc("GET /v1/me", s.requireSession(s.handleMe))
	api.HandleFunc("POST /v1/me/active-organization", s.requireSession(s.handleSetActiveOrg))
	api.HandleFunc("GET /v1/me/orgs", s.requireSession(s.handleMyOrgs))
	api.HandleFunc("GET /v1/me/desktop-config", s.requireSession(s.handleDesktopConfig))
	api.HandleFunc("GET /v1/me/library", s.requireSession(s.handleLibrary))
	api.HandleFunc("GET /v1/org", s.requireSession(s.handleOrg))
	api.HandleFunc("GET /v1/resources", s.requireSession(s.handleResources))
	api.HandleFunc("GET /v1/resources/marketplace-capabilities", s.requireSession(s.handleMarketplaceCapabilities))
	api.HandleFunc("GET /v1/marketplaces", s.requireSession(s.handleMarketplaces))
	api.HandleFunc("GET /v1/marketplaces/{id}/resolved", s.requireSession(s.handleMarketplaceResolved))
	api.HandleFunc("GET /v1/plugins/{id}/resolved", s.requireSession(s.handlePluginResolved))
	api.HandleFunc("GET /v1/plugins/{id}/config-objects", s.requireSession(s.handlePluginConfigObjects))
	api.HandleFunc("GET /v1/config-objects/{id}/versions/latest", s.requireSession(s.handleConfigObjectLatest))
	api.HandleFunc("GET /v1/llm-providers", s.requireSession(s.handleLLMProviders))
	api.HandleFunc("GET /v1/llm-providers/{id}/connect", s.requireSession(s.handleLLMProviderConnect))

	// Admin surface (SPEC-0004): burrow-designed routes, outside the client
	// contract. requireAdmin accepts an llm-admin API token or a session in
	// an BURROW_ADMIN_GROUPS group; with neither configured it is closed.
	api.HandleFunc("GET /v1/admin/llm-providers", s.requireAdmin(s.handleAdminListProviders))
	api.HandleFunc("POST /v1/admin/llm-providers", s.requireAdmin(s.handleAdminCreateProvider))
	api.HandleFunc("GET /v1/admin/llm-providers/{id}", s.requireAdmin(s.handleAdminGetProvider))
	api.HandleFunc("PUT /v1/admin/llm-providers/{id}", s.requireAdmin(s.handleAdminUpdateProvider))
	api.HandleFunc("DELETE /v1/admin/llm-providers/{id}", s.requireAdmin(s.handleAdminDeleteProvider))
	api.HandleFunc("GET /v1/admin/audit", s.requireAdmin(s.handleAdminAudit))
	api.HandleFunc("GET /v1/admin/api-tokens", s.requireAdmin(s.handleAdminListTokens))
	api.HandleFunc("POST /v1/admin/api-tokens", s.requireAdmin(s.handleAdminMintToken))
	api.HandleFunc("DELETE /v1/admin/api-tokens/{hashOrToken}", s.requireAdmin(s.handleAdminRevokeToken))

	// The console drives these exact handlers in-process (consoleCall), so
	// there is exactly one management path for provider rows.
	s.adminAPI = api

	api.HandleFunc("GET /v1/mcp-connections", s.requireSession(s.handleMCPConnections))
	api.HandleFunc("GET /v1/inference-providers", s.requireSession(s.handleInferenceProviders))
	api.HandleFunc("POST /v1/mcp/token", s.requireSession(s.handleMCPToken))
	api.HandleFunc("POST /api/auth/sign-out", s.requireSession(s.handleSignOut))
	mux.Handle("/api/den/", http.StripPrefix("/api/den", api))

	return logRequests(mux)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path == "/" || r.URL.Path == "/healthz" {
			return
		}
		slog.Debug("request", "method", r.Method, "path", r.URL.Path, "elapsed", time.Since(start))
	})
}

// origin returns the externally visible origin for this request: the
// configured BaseURL when set, else derived from X-Forwarded-Proto / TLS /
// Host so both loopback dev and proxied production produce https origins.
func (s *Server) origin(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return s.cfg.BaseURL
	}
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = strings.TrimSpace(strings.SplitN(proto, ",", 2)[0])
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// marketplaceID / pluginID / configObjectID: stable derived identifiers for
// the single-marketplace mapping (see the stage-a spec). The client treats
// them as opaque strings.
func marketplaceID(cfg *config.Config) string { return "mkt_" + cfg.OrgSlug }
func pluginID(name string) string             { return "plg_" + name }
func configObjectID(name string) string       { return "cfo_" + name }
func membershipID(name string) string         { return "pm_" + name }
func memberID(userID string) string           { return "mem_" + userID }

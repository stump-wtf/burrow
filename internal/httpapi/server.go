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
	cfg            *config.Config
	store          *store.Store
	skills         *skills.Catalog
	providers      *providers.Runtime // nil serves none
	providerPolicy providers.Policy   // what a write may name (ADR-0005)
	oidc           OIDCClient
	adminAPI       *http.ServeMux // the /api/den/v1/admin subtree, driven in-process by the /admin console
}

// New builds the server. providerPolicy is the pair of allowlists a provider
// write is held to, the same policy the runtime applies wherever a row is
// used; build it from the config with providers.NewPolicy.
func New(cfg *config.Config, st *store.Store, catalog *skills.Catalog, runtime *providers.Runtime, providerPolicy providers.Policy, client OIDCClient) *Server {
	return &Server{cfg: cfg, store: st, skills: catalog, providers: runtime, providerPolicy: providerPolicy, oidc: client}
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
	// Provider rows go through the write API itself; the skill-pack matrix
	// writes the Library's mapping directly (SPEC-0003 REQ "Library audience"),
	// which has no API surface of its own — the mapping is burrow's, not the
	// client contract's.
	console := http.NewServeMux()
	console.HandleFunc("GET /admin/{$}", s.requireAdminPage(s.handleConsoleList))
	console.HandleFunc("GET /admin", s.requireAdminPage(s.handleConsoleList))
	console.HandleFunc("GET /admin/audit", s.requireAdminPage(s.handleConsoleAudit))
	console.HandleFunc("GET /admin/skills", s.requireAdminPage(s.handleConsoleSkills))
	console.HandleFunc("POST /admin/skills", s.requireAdminPage(s.handleConsoleSkillsSave))
	console.HandleFunc("GET /admin/preview", s.requireAdminPage(s.handleConsolePreview))
	console.HandleFunc("GET /admin/providers/new", s.requireAdminPage(s.handleConsoleNew))
	console.HandleFunc("POST /admin/providers", s.requireAdminPage(s.handleConsoleCreate))
	console.HandleFunc("GET /admin/providers/{id}", s.requireAdminPage(s.handleConsoleEdit))
	console.HandleFunc("POST /admin/providers/{id}", s.requireAdminPage(s.handleConsoleUpdate))
	console.HandleFunc("GET /admin/providers/{id}/delete", s.requireAdminPage(s.handleConsoleConfirmDelete))
	console.HandleFunc("POST /admin/providers/{id}/delete", s.requireAdminPage(s.handleConsoleDelete))
	// Sign-out is the one console route outside requireAdminPage: a member
	// who is not an admin holds the session cookie too.
	console.HandleFunc("POST /admin/sign-out", s.handleConsoleSignOut)
	// Every console route, present and future, is mounted through the
	// cross-origin check: register console pages on the console mux above,
	// never on the outer mux, where a more specific pattern would bypass it.
	guarded := s.sameOriginConsole(console)
	mux.Handle("/admin", guarded)
	mux.Handle("/admin/", guarded)

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
	// Token management is a member's decision: a bat_ bearer is refused here,
	// so a leaked token cannot mint a successor (ADR-0005, #37).
	api.HandleFunc("GET /v1/admin/api-tokens", s.requireAdminSession(s.handleAdminListTokens))
	api.HandleFunc("POST /v1/admin/api-tokens", s.requireAdminSession(s.handleAdminMintToken))
	api.HandleFunc("DELETE /v1/admin/api-tokens/{hashOrToken}", s.requireAdminSession(s.handleAdminRevokeToken))
	// Member revocation is a member's decision for the same reason: a bat_
	// bearer is refused here too, so a leaked automation token cannot end
	// anyone's sessions (ADR-0008).
	api.HandleFunc("POST /v1/admin/members/{id}/revoke", s.requireAdminSession(s.handleAdminRevokeMember))

	// The console drives these exact handlers in-process (consoleCall), so
	// there is exactly one management path for provider rows.
	s.adminAPI = api

	api.HandleFunc("GET /v1/mcp-connections", s.requireSession(s.handleMCPConnections))
	api.HandleFunc("GET /v1/inference-providers", s.requireSession(s.handleInferenceProviders))
	api.HandleFunc("POST /v1/mcp/token", s.requireSession(s.handleMCPToken))
	api.HandleFunc("POST /api/auth/sign-out", s.requireSession(s.handleSignOut))
	mux.Handle("/api/den/", http.StripPrefix("/api/den", api))

	return logRequests(limitBody(mux))
}

// maxRequestBody caps every request body. No route has a legitimate body near
// it: the JSON routes are capped tighter where they decode (1 MiB in
// decodeBody and decodeStrict, 4 MiB on the MCP endpoint), and 10 MiB is the
// cap net/http itself puts on a form post, which is what the console's
// ParseForm calls ran under before the body was wrapped here — so no route's
// limit drops.
const maxRequestBody = 10 << 20

// limitBody bounds what any handler can be made to read. A handler that
// decodes r.Body directly buffers whatever it is sent, and the grant exchange
// did exactly that with no credential required: a 256 MiB body cost over a
// gigabyte before it was answered 401. A declared length over the cap is
// refused before a byte is read; an undeclared one fails the handler's read
// at the cap, and the handler answers with writeBodyTooLarge.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxRequestBody {
			writeBodyTooLarge(w)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		next.ServeHTTP(w, r)
	})
}

// sameOriginConsole refuses a console write that a browser says came from
// another origin. The console is the one surface a browser authenticates to
// with a cookie it attaches on its own, so it is the one surface a page
// elsewhere can write to on a signed-in admin's behalf — and SameSite=Lax
// does not stop that: SameSite is scoped to the site, and every sibling
// subdomain of burrow's registrable domain is same-site, so its form POSTs
// carry the cookie. A forged provider row is enough to exfiltrate a key
// (name an environment variable burrow holds, point baseURL elsewhere).
//
// The check is the standard library's: GET/HEAD/OPTIONS pass, so no console
// GET may change anything; any other method passes only when Sec-Fetch-Site
// is same-origin or none or, from a browser too old to send it, when
// Origin's host is the request's Host. A request carrying neither header
// passes: it is a non-browser client (curl, a script), which has no ambient
// cookie to ride, or a browser years out of date. The /api/den routes are
// bearer-only and deliberately stay outside the check.
//
// Governing: SPEC-0004 REQ "Admin console".
func (s *Server) sameOriginConsole(next http.Handler) http.Handler {
	guard := http.NewCrossOriginProtection()
	guard.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Warn("console: cross-origin request refused", "method", r.Method, "path", r.URL.Path,
			"origin", r.Header.Get("Origin"), "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
		s.renderConsoleError(w, http.StatusForbidden,
			"cross_origin: this request came from another site, so nothing was changed. Open the console and try again from there.")
	}))
	return guard.Handler(next)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path == "/" {
			return
		}
		slog.Debug("request", "method", r.Method, "path", r.URL.Path, "elapsed", time.Since(start))
	})
}

// origin returns the externally visible origin for this request: the
// configured BaseURL when set, else derived from X-Forwarded-Proto / TLS /
// Host so both loopback dev and proxied production produce https origins.
//
// The forwarded scheme is a request header, so it is only believed when it
// names a scheme burrow can be served on. Taken verbatim, "javascript" came
// back as javascript://<host> in denApiUrl, the MCP resource and the deep
// link's denBaseUrl. Anything but http or https falls back to the
// connection's own scheme.
func (s *Server) origin(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return s.cfg.BaseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		switch forwarded := strings.ToLower(strings.TrimSpace(strings.SplitN(proto, ",", 2)[0])); forwarded {
		case "http", "https":
			scheme = forwarded
		}
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

// burrow is a self-hosted, OpenWork-compatible control plane: OIDC sign-in,
// org, Library skills, and the MCP endpoint that delivers skills to the agent.
//
// Governing: ADR-0001 (clean-room implementation of the client contract).
// The contract itself is docs/client-contract.md.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/burrow/internal/config"
	"gitea.stump.rocks/stump.wtf/burrow/internal/httpapi"
	"gitea.stump.rocks/stump.wtf/burrow/internal/oidc"
	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// storeOpenTimeout bounds connecting to Postgres and converging the schema.
const storeOpenTimeout = 30 * time.Second

// How long a request may take to arrive, and how long a stop waits.
//
// The read timeouts bound the client, not the handler: net/http lifts the read
// deadline once the request body has been consumed, so a handler waiting on a
// git refresh or a gateway is not cut by them. Every body burrow accepts is
// small (httpapi caps them), so a minute to deliver one is generous on any
// link.
const (
	// readHeaderTimeout is how long a client has to finish its request
	// headers. Without it a connection that sends half a request line is
	// held open for as long as the client likes.
	readHeaderTimeout = 10 * time.Second
	// readTimeout covers the headers and the body together.
	readTimeout = 60 * time.Second
	// idleTimeout is how long a keep-alive connection may sit between
	// requests before burrow closes it.
	idleTimeout = 120 * time.Second
	// shutdownGrace is how long a stop waits for requests in flight. It sits
	// under Docker's default 10 s stop timeout — the deployment sets no
	// longer one — so burrow drains and exits by itself instead of being
	// killed mid-drain.
	shutdownGrace = 8 * time.Second
)

// newServer builds the HTTP server with its timeouts.
//
// There is deliberately no WriteTimeout. It runs from the end of the request
// headers, so it bounds the handler as well as the write, and two paths have
// no upper bound to set it above: a Library or MCP request can wait on a git
// clone that carries only the request's context, and a provider listing makes
// one gateway call of up to 10 s for every row with a stale menu.
func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// serve runs srv on ln until ctx ends, then stops accepting and gives
// requests in flight up to grace to finish. The image ships on every merge to
// main, so without this every deploy cut whatever was being served. A request
// still running when grace runs out is cut; the wait is bounded so a stuck
// handler cannot hold a deploy.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, grace time.Duration) error {
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	slog.Info("burrow shutting down", "grace", grace)
	drain, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(drain); err != nil {
		slog.Warn("shutdown grace ran out; closing the connections still open", "error", err)
		return srv.Close()
	}
	return nil
}

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("configuration", "error", err)
		os.Exit(1)
	}

	// SIGINT and SIGTERM end ctx instead of killing the process. Boot work
	// takes it too: once the signals are caught, a boot stuck on the database
	// or the issuer would otherwise ignore a stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Bounded: a boot that cannot reach the database, or cannot get a lock its
	// schema change needs, exits with an error instead of hanging unstarted.
	openCtx, cancelOpen := context.WithTimeout(ctx, storeOpenTimeout)
	st, err := store.Open(openCtx, cfg.DatabaseURL)
	cancelOpen()
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer st.Close()
	// Expired rows are refused on read but nothing else deletes them.
	go reapExpired(ctx, st, reapInterval)

	// Provider rows live in the store (SPEC-0004); the model menus are
	// derived from each row's own gateway key at request time (ADR-0004).
	// What a row may name is the pair of allowlists, applied here and again
	// wherever a row is used (ADR-0005); the server builds the same policy
	// from the same config for the write gate.
	llmPolicy := providers.NewPolicy(cfg.KeyEnvPrefixes, cfg.GatewayHosts)
	if len(cfg.GatewayHosts) == 0 {
		// An unset list allows no host, so the symptom is every row serving
		// no models. Say it once, at boot, where an operator is looking.
		slog.Warn("no provider gateway host is configured; provider rows will serve no models until BURROW_PROVIDER_BASE_URL_HOSTS names one (ADR-0005)")
	}
	llmRuntime := providers.NewRuntime(st, cfg.ModelsTTL, llmPolicy)
	if cfg.ProvidersFile != "" {
		seeded, warnings, skipped, err := providers.SeedIfEmpty(ctx, st, cfg.ProvidersFile, llmPolicy)
		if err != nil {
			slog.Error("seed llm providers", "error", err)
			os.Exit(1)
		}
		switch {
		case skipped:
			// The store is the source of truth; the file is history —
			// also when an admin has since deleted every row. SeedIfEmpty
			// has already warned about each row where the two differ.
			slog.Info("llm providers file ignored: the store was already seeded or written", "file", cfg.ProvidersFile)
		default:
			// First boot: the file seeds the table once, then the store is
			// the source of truth and the file is never consulted again.
			slog.Info("llm providers seeded from file", "count", seeded, "file", cfg.ProvidersFile)
			for _, id := range warnings {
				slog.Warn("dropped model-level groups while seeding; a derived model's audience is its row's — move the audience to the row", "provider", id)
			}
		}
	}

	catalog := skills.NewCatalog(skills.Source{
		Dir:      cfg.SkillsDir,
		GitURL:   cfg.SkillsGitURL,
		CacheDir: cfg.SkillsCacheDir,
		SubDir:   cfg.SkillsSubdir,
	})

	redirectURL := cfg.OIDCRedirectURL
	if redirectURL == "" {
		redirectURL = cfg.BaseURL + "/auth/oidc/callback"
	}
	if redirectURL == "/auth/oidc/callback" {
		slog.Error("configuration: set BURROW_BASE_URL or BURROW_OIDC_REDIRECT_URL so the OIDC redirect is absolute")
		os.Exit(1)
	}
	// Rows are mutable at runtime, so the need for the groups claim cannot
	// be decided at boot: request the scope unless the issuer rejects
	// unknown scopes (BURROW_OIDC_GROUPS_SCOPE=false, SPEC-0004).
	client, err := oidc.New(ctx, cfg.OIDCIssuerURL, cfg.OIDCClientID, cfg.OIDCClientSecret, redirectURL, cfg.OIDCGroupsScope)
	if err != nil {
		slog.Error("connect to OIDC issuer", "error", err)
		os.Exit(1)
	}

	srv := newServer(cfg.Addr, httpapi.New(cfg, st, catalog, llmRuntime, llmPolicy, client).Routes())
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		slog.Error("listen", "addr", cfg.Addr, "error", err)
		os.Exit(1)
	}
	slog.Info("burrow listening", "addr", cfg.Addr, "version", buildinfo.Version, "org", cfg.OrgSlug)
	if err := serve(ctx, srv, ln, shutdownGrace); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}

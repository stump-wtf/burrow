// burrow is a self-hosted, OpenWork-compatible control plane: OIDC sign-in,
// org, Library skills, and the MCP endpoint that delivers skills to the agent.
//
// Governing: ADR-0001 (clean-room implementation of the client contract).
// The contract itself is docs/client-contract.md.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"gitea.stump.rocks/stump.wtf/burrow/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/burrow/internal/config"
	"gitea.stump.rocks/stump.wtf/burrow/internal/httpapi"
	"gitea.stump.rocks/stump.wtf/burrow/internal/oidc"
	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("configuration", "error", err)
		os.Exit(1)
	}

	st, err := store.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer st.Close()
	// Expired rows are refused on read but nothing else deletes them.
	go reapExpired(context.Background(), st, reapInterval)

	// Provider rows live in the store (SPEC-0004); the model menus are
	// derived from each row's own gateway key at request time (ADR-0004).
	llmRuntime := providers.NewRuntime(st, cfg.ModelsTTL)
	if cfg.ProvidersFile != "" {
		seeded, warnings, skipped, err := providers.SeedIfEmpty(context.Background(), st, cfg.ProvidersFile)
		if err != nil {
			slog.Error("seed llm providers", "error", err)
			os.Exit(1)
		}
		switch {
		case skipped:
			// The store is the source of truth; the file is history.
			slog.Info("llm providers file ignored: store already has rows", "file", cfg.ProvidersFile)
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
	client, err := oidc.New(context.Background(), cfg.OIDCIssuerURL, cfg.OIDCClientID, cfg.OIDCClientSecret, redirectURL, cfg.OIDCGroupsScope)
	if err != nil {
		slog.Error("connect to OIDC issuer", "error", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: httpapi.New(cfg, st, catalog, llmRuntime, client).Routes(),
	}
	slog.Info("burrow listening", "addr", cfg.Addr, "version", buildinfo.Version, "org", cfg.OrgSlug)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}

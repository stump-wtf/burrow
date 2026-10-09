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

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	var llmRuntime *providers.Runtime
	if cfg.ProvidersFile != "" {
		providerList, err := providers.Load(cfg.ProvidersFile)
		if err != nil {
			slog.Error("load llm providers", "error", err)
			os.Exit(1)
		}
		info, err := os.Stat(cfg.ProvidersFile)
		if err != nil {
			slog.Error("stat llm providers file", "error", err)
			os.Exit(1)
		}
		llmRuntime = providers.NewRuntime(providerList, info.ModTime())
		slog.Info("llm providers loaded", "count", len(providerList))
	}

	catalog := skills.NewCatalog(skills.Source{
		Dir:      cfg.SkillsDir,
		GitURL:   cfg.SkillsGitURL,
		CacheDir: cfg.SkillsCacheDir,
	})

	redirectURL := cfg.OIDCRedirectURL
	if redirectURL == "" {
		redirectURL = cfg.BaseURL + "/auth/oidc/callback"
	}
	if redirectURL == "/auth/oidc/callback" {
		slog.Error("configuration: set BURROW_BASE_URL or BURROW_OIDC_REDIRECT_URL so the OIDC redirect is absolute")
		os.Exit(1)
	}
	client, err := oidc.New(context.Background(), cfg.OIDCIssuerURL, cfg.OIDCClientID, cfg.OIDCClientSecret, redirectURL)
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

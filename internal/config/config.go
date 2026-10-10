// Package config loads burrow's runtime configuration from the environment.
//
// Governing: ADR-0001 (OIDC sign-in, Pocket ID first, any issuer by config).
// Secrets (the OIDC client credential above all) arrive via the environment —
// OpenBao in deployment — and never live in the repo or the database.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the full runtime configuration for one burrow process.
type Config struct {
	// Addr is the listen address (BURROW_ADDR, default ":8080").
	Addr string
	// BaseURL overrides the externally visible origin (BURROW_BASE_URL). When
	// empty, the origin is derived per request (X-Forwarded-Proto, then TLS,
	// then http) so loopback dev and proxied production both work.
	BaseURL string

	DBPath string

	// OIDC sign-in: any compliant issuer, Pocket ID first.
	OIDCIssuerURL    string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCRedirectURL  string // optional; default <origin>/auth/oidc/callback

	// Single-org model (ADR-0001): one org from config, every OIDC user a
	// member. Group mapping is a later seam, not a stage-A feature.
	OrgName string
	OrgSlug string

	// Library skills source: a local directory of <name>/SKILL.md, or a git
	// URL cloned/refreshed into SkillsCacheDir. SkillsSubdir names a
	// directory inside the source (the org's skills repo keeps them under
	// skills/); empty loads the source root.
	SkillsDir      string
	SkillsGitURL   string
	SkillsCacheDir string
	SkillsSubdir   string

	// ProvidersFile is the optional JSON file of org LLM provider
	// definitions (Stage B); empty serves none.
	ProvidersFile string

	GrantTTL     time.Duration
	SessionTTL   time.Duration
	MCPTokenTTL  time.Duration
	OIDCStateTTL time.Duration
}

// FromEnv builds a Config from BURROW_* environment variables.
func FromEnv() (*Config, error) {
	c := &Config{
		Addr:             env("BURROW_ADDR", ":8080"),
		BaseURL:          strings.TrimRight(os.Getenv("BURROW_BASE_URL"), "/"),
		DBPath:           env("BURROW_DB_PATH", "burrow.db"),
		OIDCIssuerURL:    strings.TrimRight(os.Getenv("BURROW_OIDC_ISSUER_URL"), "/"),
		OIDCClientID:     os.Getenv("BURROW_OIDC_CLIENT_ID"),
		OIDCClientSecret: os.Getenv("BURROW_OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:  os.Getenv("BURROW_OIDC_REDIRECT_URL"),
		OrgName:          env("BURROW_ORG_NAME", "burrow"),
		OrgSlug:          env("BURROW_ORG_SLUG", "burrow"),
		SkillsDir:        os.Getenv("BURROW_SKILLS_DIR"),
		SkillsGitURL:     os.Getenv("BURROW_SKILLS_GIT_URL"),
		SkillsCacheDir:   env("BURROW_SKILLS_CACHE_DIR", ""),
		SkillsSubdir:     os.Getenv("BURROW_SKILLS_SUBDIR"),
		ProvidersFile:    os.Getenv("BURROW_PROVIDERS_FILE"),
		GrantTTL:         envDuration("BURROW_GRANT_TTL", 5*time.Minute),
		SessionTTL:       envDuration("BURROW_SESSION_TTL", 30*24*time.Hour),
		MCPTokenTTL:      envDuration("BURROW_MCP_TOKEN_TTL", 24*time.Hour),
		OIDCStateTTL:     envDuration("BURROW_OIDC_STATE_TTL", 10*time.Minute),
	}
	// OIDC issuer and client id are required; the client secret is not —
	// the deployment uses a public PKCE client (Pocket ID, like cairn's),
	// which carries no secret at all. The OAuth2 layer pins the
	// public-client auth style when the secret is empty (see
	// internal/oidc), so no client credential is ever sent: an empty
	// value is a supported configuration, not a missing one.
	if c.OIDCIssuerURL == "" || c.OIDCClientID == "" {
		return nil, fmt.Errorf("config: BURROW_OIDC_ISSUER_URL and BURROW_OIDC_CLIENT_ID are required (BURROW_OIDC_CLIENT_SECRET stays empty for a public PKCE client)")
	}
	if c.SkillsDir == "" && c.SkillsGitURL == "" {
		return nil, fmt.Errorf("config: set BURROW_SKILLS_DIR (a directory of <name>/SKILL.md) or BURROW_SKILLS_GIT_URL")
	}
	if c.SkillsCacheDir == "" {
		c.SkillsCacheDir = filepath.Join(cacheDefault(), "skills")
	}
	return c, nil
}

// OrgID returns the stable identifier for the single configured org. The
// client treats org ids as opaque strings.
func (c *Config) OrgID() string { return "org_" + c.OrgSlug }

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func cacheDefault() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "burrow")
	}
	return ".burrow-cache"
}

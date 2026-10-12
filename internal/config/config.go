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
	"strconv"
	"strings"
	"time"
)

// Config is the full runtime configuration for one burrow process.
type Config struct {
	// Addr is the listen address (BURROW_ADDR, default ":8080").
	Addr string
	// BaseURL overrides the externally visible origin (BURROW_BASE_URL). When
	// empty, the origin is derived per request (an http or https
	// X-Forwarded-Proto, then TLS, then http) so loopback dev and proxied
	// production both work.
	BaseURL string

	// DatabaseURL is the Postgres connection string (BURROW_DATABASE_URL),
	// required. The deployment renders it from OpenBao at the edge; it never
	// lives in the repo or the container image (ADR-0002).
	DatabaseURL string

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

	// ProvidersFile is the first-boot seed of org LLM provider definitions
	// (SPEC-0004): consulted only while the provider store has never been
	// written; afterwards the store is the source of truth and the file is
	// ignored, even if every row is later deleted.
	ProvidersFile string

	// ModelsTTL bounds how long a row's gateway-derived model menu is
	// trusted before the gateway is asked again (BURROW_MODELS_TTL, 60s).
	ModelsTTL time.Duration

	// AdminGroups names the OIDC groups whose members may reach the admin
	// surface (BURROW_ADMIN_GROUPS, comma list, default empty — which closes
	// group-based administration; API tokens still work, SPEC-0004).
	AdminGroups []string

	// APITokenTTL is the lifetime of a minted admin API token
	// (BURROW_API_TOKEN_TTL, default 90 days).
	APITokenTTL time.Duration

	// OIDCGroupsScope requests the "groups" scope at sign-in
	// (BURROW_OIDC_GROUPS_SCOPE, default true). Rows are mutable at runtime,
	// so the need can no longer be decided at boot (ADR-0004); an issuer
	// that rejects unknown scopes opts out by setting this false.
	OIDCGroupsScope bool

	GrantTTL     time.Duration
	SessionTTL   time.Duration
	MCPTokenTTL  time.Duration
	OIDCStateTTL time.Duration
}

// FromEnv builds a Config from BURROW_* environment variables.
func FromEnv() (*Config, error) {
	// A duration or boolean that is set but unreadable fails the load (see
	// envDuration); the first one found is reported once the struct is built.
	var invalid error
	duration := func(key string, def time.Duration) time.Duration {
		d, err := envDuration(key, def)
		if err != nil && invalid == nil {
			invalid = err
		}
		return d
	}
	boolean := func(key string, def bool) bool {
		b, err := envBool(key, def)
		if err != nil && invalid == nil {
			invalid = err
		}
		return b
	}
	c := &Config{
		Addr:             env("BURROW_ADDR", ":8080"),
		BaseURL:          strings.TrimRight(os.Getenv("BURROW_BASE_URL"), "/"),
		DatabaseURL:      strings.TrimSpace(os.Getenv("BURROW_DATABASE_URL")),
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
		ModelsTTL:        duration("BURROW_MODELS_TTL", 60*time.Second),
		AdminGroups:      envList("BURROW_ADMIN_GROUPS"),
		APITokenTTL:      duration("BURROW_API_TOKEN_TTL", 90*24*time.Hour),
		OIDCGroupsScope:  boolean("BURROW_OIDC_GROUPS_SCOPE", true),
		GrantTTL:         duration("BURROW_GRANT_TTL", 5*time.Minute),
		SessionTTL:       duration("BURROW_SESSION_TTL", 30*24*time.Hour),
		MCPTokenTTL:      duration("BURROW_MCP_TOKEN_TTL", 24*time.Hour),
		OIDCStateTTL:     duration("BURROW_OIDC_STATE_TTL", 10*time.Minute),
	}
	if invalid != nil {
		return nil, invalid
	}
	// OIDC issuer and client id are required; the client secret is not —
	// the deployment uses a public PKCE client (Pocket ID, like cairn's),
	// which carries no secret at all. The OAuth2 layer pins the
	// public-client auth style when the secret is empty (see
	// internal/oidc), so no client credential is ever sent: an empty
	// value is a supported configuration, not a missing one.
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("config: BURROW_DATABASE_URL is required (a postgres:// URL — burrow keeps its state in the shared Postgres cluster, ADR-0002)")
	}
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

// envDuration reads a lifetime. Unset or blank means def. Anything else must
// be a positive Go duration, and a value that is not is an error rather than
// def: "7d" or "300" would otherwise leave an operator who meant to shorten a
// window running the default one, with nothing said. Every caller is a token
// or cache lifetime, so zero and negative values are mistakes as well. The
// error names the variable, not its value.
func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("config: %s must be a positive duration with a unit, such as 90s, 30m or 720h (there is no day unit: 7 days is 168h)", key)
	}
	return d, nil
}

// envList parses a comma-separated list, trimming entries and dropping
// empty ones; unset means the empty list, not a missing configuration.
func envList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// envBool reads a switch. Unset or blank means def; a value that is set but
// is not a boolean is an error for the same reason as in envDuration — "off"
// read as the default true is the opposite of what was asked for.
func envBool(key string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("config: %s must be true or false", key)
	}
	return b, nil
}

func cacheDefault() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "burrow")
	}
	return ".burrow-cache"
}

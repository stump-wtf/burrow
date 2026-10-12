package config_test

import (
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/config"
)

// The store moved to Postgres (ADR-0002), so the connection string is now part
// of what makes a configuration valid: starting without it must say so rather
// than fall back to a database file nobody is watching.
func TestFromEnvRequiresTheDatabaseURL(t *testing.T) {
	t.Setenv("BURROW_OIDC_ISSUER_URL", "https://identity.example")
	t.Setenv("BURROW_OIDC_CLIENT_ID", "burrow")
	t.Setenv("BURROW_SKILLS_DIR", t.TempDir())
	t.Setenv("BURROW_DATABASE_URL", "")

	_, err := config.FromEnv()
	if err == nil || !strings.Contains(err.Error(), "BURROW_DATABASE_URL") {
		t.Fatalf("a missing BURROW_DATABASE_URL must be a named configuration error, got %v", err)
	}
}

func TestFromEnvReadsTheDatabaseURL(t *testing.T) {
	t.Setenv("BURROW_OIDC_ISSUER_URL", "https://identity.example")
	t.Setenv("BURROW_OIDC_CLIENT_ID", "burrow")
	t.Setenv("BURROW_SKILLS_DIR", t.TempDir())
	t.Setenv("BURROW_DATABASE_URL", " postgres://burrow:burrow@postgres:5432/burrow?sslmode=disable ")

	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.DatabaseURL != "postgres://burrow:burrow@postgres:5432/burrow?sslmode=disable" {
		t.Fatalf("the URL must be trimmed of surrounding whitespace, got %q", cfg.DatabaseURL)
	}
}

// The provider-admin knobs (SPEC-0004, ADR-0004) must have working defaults
// and be overridable, because deployment sets them in burrow.env rather than
// in code.
func TestFromEnvProviderAdminKnobs(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		t.Setenv("BURROW_OIDC_ISSUER_URL", "https://identity.example")
		t.Setenv("BURROW_OIDC_CLIENT_ID", "burrow")
		t.Setenv("BURROW_SKILLS_DIR", t.TempDir())
		t.Setenv("BURROW_DATABASE_URL", "postgres://burrow:burrow@postgres:5432/burrow?sslmode=disable")
	}

	t.Run("defaults", func(t *testing.T) {
		base(t)
		cfg, err := config.FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.ModelsTTL != 60*time.Second {
			t.Fatalf("menu cache TTL default: %v", cfg.ModelsTTL)
		}
		if cfg.APITokenTTL != 90*24*time.Hour {
			t.Fatalf("admin token TTL default: %v", cfg.APITokenTTL)
		}
		if len(cfg.AdminGroups) != 0 {
			t.Fatalf("no admin groups by default (the surface starts closed): %v", cfg.AdminGroups)
		}
		if !cfg.OIDCGroupsScope {
			t.Fatalf("groups must be requested at sign-in by default")
		}
	})

	t.Run("overrides", func(t *testing.T) {
		base(t)
		t.Setenv("BURROW_MODELS_TTL", "5m")
		t.Setenv("BURROW_API_TOKEN_TTL", "24h")
		t.Setenv("BURROW_ADMIN_GROUPS", "openwork-owner, stumpcloud-admins ,")
		t.Setenv("BURROW_OIDC_GROUPS_SCOPE", "false")
		cfg, err := config.FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.ModelsTTL != 5*time.Minute || cfg.APITokenTTL != 24*time.Hour {
			t.Fatalf("durations: %v %v", cfg.ModelsTTL, cfg.APITokenTTL)
		}
		if strings.Join(cfg.AdminGroups, ",") != "openwork-owner,stumpcloud-admins" {
			t.Fatalf("admin groups must be trimmed and emptied entries dropped: %v", cfg.AdminGroups)
		}
		if cfg.OIDCGroupsScope {
			t.Fatalf("BURROW_OIDC_GROUPS_SCOPE=false must disable the scope request")
		}
	})
}

// validEnv sets the smallest environment FromEnv accepts.
func validEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BURROW_OIDC_ISSUER_URL", "https://identity.example")
	t.Setenv("BURROW_OIDC_CLIENT_ID", "burrow")
	t.Setenv("BURROW_SKILLS_DIR", t.TempDir())
	t.Setenv("BURROW_DATABASE_URL", "postgres://burrow:burrow@postgres:5432/burrow?sslmode=disable")
}

// A variable that is set must mean what it says. Falling back to the default
// on a value burrow cannot read is silent in exactly the wrong direction: an
// operator who shortens the session window with BURROW_SESSION_TTL=7d (days
// are not a Go duration unit) keeps 30-day sessions and is never told.
func TestFromEnvRejectsUnreadableValues(t *testing.T) {
	cases := []struct {
		name, key, value string
		got              func(*config.Config) any
	}{
		{"days are not a unit", "BURROW_SESSION_TTL", "7d", func(c *config.Config) any { return c.SessionTTL }},
		{"days are not a unit", "BURROW_API_TOKEN_TTL", "7d", func(c *config.Config) any { return c.APITokenTTL }},
		{"a number without a unit", "BURROW_MODELS_TTL", "300", func(c *config.Config) any { return c.ModelsTTL }},
		{"words", "BURROW_GRANT_TTL", "five minutes", func(c *config.Config) any { return c.GrantTTL }},
		{"a unit that does not exist", "BURROW_MCP_TOKEN_TTL", "1day", func(c *config.Config) any { return c.MCPTokenTTL }},
		{"a space inside", "BURROW_OIDC_STATE_TTL", "10 m", func(c *config.Config) any { return c.OIDCStateTTL }},
		// Every one of these is a lifetime: zero or less would mint tokens
		// that are already expired, so it is a mistake, not a setting.
		{"zero", "BURROW_SESSION_TTL", "0", func(c *config.Config) any { return c.SessionTTL }},
		{"zero with a unit", "BURROW_MODELS_TTL", "0s", func(c *config.Config) any { return c.ModelsTTL }},
		{"negative", "BURROW_GRANT_TTL", "-5m", func(c *config.Config) any { return c.GrantTTL }},
		{"negative", "BURROW_API_TOKEN_TTL", "-1h", func(c *config.Config) any { return c.APITokenTTL }},
		{"negative", "BURROW_MCP_TOKEN_TTL", "-1h", func(c *config.Config) any { return c.MCPTokenTTL }},
		{"zero", "BURROW_OIDC_STATE_TTL", "0m", func(c *config.Config) any { return c.OIDCStateTTL }},
		{"not a boolean", "BURROW_OIDC_GROUPS_SCOPE", "off", func(c *config.Config) any { return c.OIDCGroupsScope }},
		{"not a boolean", "BURROW_OIDC_GROUPS_SCOPE", "no", func(c *config.Config) any { return c.OIDCGroupsScope }},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			validEnv(t)
			t.Setenv(tc.key, tc.value)
			cfg, err := config.FromEnv()
			if err == nil {
				t.Fatalf("%s (%s): accepted, and silently configured as %v", tc.name, tc.value, tc.got(cfg))
			}
			if !strings.Contains(err.Error(), tc.key) || !strings.HasPrefix(err.Error(), "config: ") {
				t.Fatalf("the error must name the variable in the file's usual shape, got %v", err)
			}
		})
	}
}

// Unset — and set-but-empty, which is how an env template renders an absent
// value — still means the default, and surrounding whitespace is not part of
// a value.
func TestFromEnvUnsetValuesKeepTheirDefaults(t *testing.T) {
	keys := []string{
		"BURROW_MODELS_TTL", "BURROW_API_TOKEN_TTL", "BURROW_GRANT_TTL", "BURROW_SESSION_TTL",
		"BURROW_MCP_TOKEN_TTL", "BURROW_OIDC_STATE_TTL", "BURROW_OIDC_GROUPS_SCOPE",
	}
	for _, blank := range []string{"", "  "} {
		validEnv(t)
		for _, key := range keys {
			t.Setenv(key, blank)
		}
		cfg, err := config.FromEnv()
		if err != nil {
			t.Fatalf("blank %q values must mean the defaults: %v", blank, err)
		}
		if cfg.ModelsTTL != time.Minute || cfg.APITokenTTL != 90*24*time.Hour || cfg.GrantTTL != 5*time.Minute ||
			cfg.SessionTTL != 30*24*time.Hour || cfg.MCPTokenTTL != 24*time.Hour || cfg.OIDCStateTTL != 10*time.Minute ||
			!cfg.OIDCGroupsScope {
			t.Fatalf("blank %q values must mean the defaults, got %+v", blank, cfg)
		}
	}

	validEnv(t)
	t.Setenv("BURROW_SESSION_TTL", " 168h ")
	t.Setenv("BURROW_OIDC_GROUPS_SCOPE", " 0 ")
	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.SessionTTL != 7*24*time.Hour || cfg.OIDCGroupsScope {
		t.Fatalf("padded values must be read as written: %v %v", cfg.SessionTTL, cfg.OIDCGroupsScope)
	}
}

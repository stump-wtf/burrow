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

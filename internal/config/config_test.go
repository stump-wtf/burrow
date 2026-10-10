package config_test

import (
	"strings"
	"testing"

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

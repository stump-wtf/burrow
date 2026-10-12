package providers_test

// First-boot seeding (SPEC-0004 REQ "Rows in the store", scenario
// "First-boot seed"): an empty table takes the file once; a non-empty table
// ignores it; model names become display-name overrides; model-level groups
// are dropped with a warning naming the row.

import (
	"context"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store/storetest"
)

const seedFile = `{
  "providers": [
    {"id": "lpr_local", "providerId": "litellm", "name": "Local",
     "baseURL": "https://gw.example/v1", "apiKeyEnv": "KEY_LOCAL", "groups": ["family"],
     "models": [{"id": "qwen-27b", "name": "Qwen 27B"}, {"id": "glm", "name": "GLM"}]},
    {"id": "lpr_owner", "providerId": "litellm", "name": "Owner",
     "baseURL": "https://gw.example/v1", "apiKeyEnv": "KEY_OWNER",
     "models": [{"id": "gpt-5", "name": "GPT-5", "groups": ["owner"]}]}
  ]
}`

func TestSeedFromFile(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()

	seeded, warnings, err := providers.SeedFromFile(ctx, st, writeProvidersFile(t, seedFile))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if seeded != 2 {
		t.Fatalf("two rows seeded, got %d", seeded)
	}
	if len(warnings) != 1 || warnings[0] != "lpr_owner" {
		t.Fatalf("the row with model-level groups must be warned about, got %v", warnings)
	}

	row, err := st.GetProvider(ctx, "lpr_local")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.NameOverrides["qwen-27b"] != "Qwen 27B" || row.NameOverrides["glm"] != "GLM" {
		t.Fatalf("file model names must become name overrides: %#v", row.NameOverrides)
	}
	if len(row.Groups) != 1 || row.Groups[0] != "family" {
		t.Fatalf("row-level groups carry over: %#v", row.Groups)
	}
	gated, err := st.GetProvider(ctx, "lpr_owner")
	if err != nil {
		t.Fatalf("get owner row: %v", err)
	}
	if gated.Groups != nil {
		t.Fatalf("dropped model groups must not become a row audience: %#v", gated.Groups)
	}

	// The seed is auditable: the audit trail names "seed" as its actor.
	entries, err := st.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	seededCount := 0
	for _, e := range entries {
		if e.Actor == providers.SeedActor && e.Action == "create" {
			seededCount++
		}
	}
	if seededCount != 2 {
		t.Fatalf("both seeded rows must be audited under the seed actor, got %d", seededCount)
	}
}

func TestSeedIfEmptySkipsNonEmptyTable(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	if err := st.CreateProvider(ctx, store.ProviderRow{
		ID: "lpr_existing", ProviderID: "litellm", Name: "Existing",
		BaseURL: "https://elsewhere.example/v1", APIKeyEnv: "KEY",
	}, "admin"); err != nil {
		t.Fatalf("create: %v", err)
	}

	seeded, warnings, skipped, err := providers.SeedIfEmpty(ctx, st, writeProvidersFile(t, seedFile))
	if err != nil {
		t.Fatalf("seed if empty: %v", err)
	}
	if !skipped || seeded != 0 || len(warnings) != 0 {
		t.Fatalf("a non-empty table must skip the file entirely: %v %v %v", seeded, warnings, skipped)
	}
	if _, err := st.GetProvider(ctx, "lpr_local"); err != store.ErrNotFound {
		t.Fatalf("the file must not have been consulted, got %v", err)
	}
}

// The seeded row serves its overrides until the first derived menu arrives:
// a row with no provisioned key shows no models even when seeded with names
// (ADR-0004: the menu is the key's scope, not the file's list).
func TestSeededRowServesNoModelsWithoutKey(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	if _, _, err := providers.SeedFromFile(ctx, st, writeProvidersFile(t, seedFile)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("KEY_LOCAL", "")
	t.Setenv("KEY_OWNER", "")
	runtime := providers.NewRuntime(st, time.Minute)
	rows, err := runtime.RowsFor(ctx, []string{"family"})
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	for _, r := range rows {
		if len(r.Models) != 0 {
			t.Fatalf("a keyless row serves no models, %s has %+v", r.ID, r.Models)
		}
	}
}

// A value the store cannot hold must fail the file at validation, before the
// first insert: failing in Postgres on the second row leaves the first one
// committed, and a non-empty table is never seeded again.
func TestSeedRejectsControlCharactersBeforeInserting(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	const file = `{"providers": [
    {"id": "lpr_local", "providerId": "litellm", "name": "Local",
     "baseURL": "https://gw.example/v1", "apiKeyEnv": "KEY_LOCAL"},
    {"id": "lpr_owner", "providerId": "litellm", "name": "Own\u0000er",
     "baseURL": "https://gw.example/v1", "apiKeyEnv": "KEY_OWNER"}
  ]}`

	seeded, _, err := providers.SeedFromFile(ctx, st, writeProvidersFile(t, file))
	if err == nil {
		t.Fatal("a NUL in a name must fail the seed")
	}
	n, countErr := st.CountProviders(ctx)
	if countErr != nil {
		t.Fatal(countErr)
	}
	if seeded != 0 || n != 0 {
		t.Fatalf("a refused file must insert nothing, seeded=%d stored=%d (error: %v)", seeded, n, err)
	}
}

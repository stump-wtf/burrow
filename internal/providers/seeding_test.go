package providers_test

// First-boot seeding (SPEC-0004 REQ "Rows in the store", scenario
// "First-boot seed"): a store nothing has written takes the file once; a
// store with rows, or one an admin has since emptied, ignores it; model
// names become display-name overrides; model-level groups are dropped with a
// warning naming the row.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
		t.Fatalf("the file's rows must not have been inserted, got %v", err)
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

// An admin who deletes every row has pulled the org's models on purpose; the
// next restart (the image ships on every merge) must not hand them back with
// the file's original audiences. "Already seeded" is therefore read from the
// audit trail, which survives deletes, not from the table being empty
// (ADR-0004: after first boot the store, not the file, is the source of
// truth).
func TestSeedIfEmptyDoesNotReseedAfterEveryRowIsDeleted(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	path := writeProvidersFile(t, seedFile)

	// A brand-new database still takes the file.
	seeded, _, skipped, err := providers.SeedIfEmpty(ctx, st, path)
	if err != nil || skipped || seeded != 2 {
		t.Fatalf("first boot must seed both rows: seeded=%d skipped=%v err=%v", seeded, skipped, err)
	}
	for _, id := range []string{"lpr_local", "lpr_owner"} {
		if err := st.DeleteProvider(ctx, id, time.Time{}, "admin"); err != nil {
			t.Fatalf("delete %s: %v", id, err)
		}
	}

	seeded, _, skipped, err = providers.SeedIfEmpty(ctx, st, path)
	if err != nil {
		t.Fatalf("restart after the deletes: %v", err)
	}
	if !skipped || seeded != 0 {
		t.Errorf("a store an admin emptied must stay empty across a restart: seeded=%d skipped=%v", seeded, skipped)
	}
	rows, err := st.ListProviders(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("the deleted rows came back from the file: %d rows served", len(rows))
	}
	entries, err := st.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) != 4 || entries[0].Action != "delete" {
		var trail []string
		for i := len(entries) - 1; i >= 0; i-- {
			trail = append(trail, entries[i].Action+" by "+entries[i].Actor)
		}
		t.Errorf("the audit trail must end at the admin's deletes, got %v", trail)
	}
}

// The same holds for a store an admin wrote before any file was configured:
// rows created and deleted through the admin API leave the store "written",
// so a file configured later is not a first boot.
func TestSeedIfEmptySkipsAStoreAnAdminAlreadyWrote(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	if err := st.CreateProvider(ctx, store.ProviderRow{
		ID: "lpr_existing", ProviderID: "litellm", Name: "Existing",
		BaseURL: "https://elsewhere.example/v1", APIKeyEnv: "KEY",
	}, "admin"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.DeleteProvider(ctx, "lpr_existing", time.Time{}, "admin"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	seeded, _, skipped, err := providers.SeedIfEmpty(ctx, st, writeProvidersFile(t, seedFile))
	if err != nil {
		t.Fatalf("seed if empty: %v", err)
	}
	if !skipped || seeded != 0 {
		t.Fatalf("an admin-written store must not be seeded: seeded=%d skipped=%v", seeded, skipped)
	}
}

// partialSeedFile passes Load but its second row cannot be stored: Postgres
// refuses a NUL byte in TEXT. It stands in for any failure after the first
// insert — a dropped connection, a SIGTERM mid-boot.
const partialSeedFile = `{
  "providers": [
    {"id": "lpr_one", "providerId": "litellm", "name": "One",
     "baseURL": "https://gw.example/v1", "apiKeyEnv": "KEY_ONE", "models": []},
    {"id": "lpr_two", "providerId": "litellm", "name": "Two\u0000",
     "baseURL": "https://gw.example/v1", "apiKeyEnv": "KEY_TWO", "models": []},
    {"id": "lpr_three", "providerId": "litellm", "name": "Three",
     "baseURL": "https://gw.example/v1", "apiKeyEnv": "KEY_THREE", "models": []}
  ]
}`

// A seed that fails partway must leave nothing behind. Otherwise the rows
// that landed make the table look seeded: main exits 1, the container
// restarts, the file is ignored for good, and burrow serves a partial
// provider set with nothing louder than the "file ignored" Info line.
func TestSeedIfEmptyLandsWholeOrNotAtAll(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	path := writeProvidersFile(t, partialSeedFile)

	count := func() (rows, audit int) {
		t.Helper()
		list, err := st.ListProviders(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		entries, err := st.ListAudit(ctx, 10)
		if err != nil {
			t.Fatalf("list audit: %v", err)
		}
		return len(list), len(entries)
	}

	// Boot 1: the seed fails.
	seeded, _, skipped, err := providers.SeedIfEmpty(ctx, st, path)
	if err == nil {
		t.Fatalf("a seed with an unstorable row must fail: seeded=%d skipped=%v", seeded, skipped)
	}
	if rows, audit := count(); rows != 0 || audit != 0 || seeded != 0 {
		t.Errorf("boot 1: a failed seed must leave nothing behind: seeded=%d, %d rows and %d audit entries stored (err: %v)", seeded, rows, audit, err)
	}

	// Boot 2, same file: the failure must repeat, not turn into a skip.
	seeded, _, skipped, err = providers.SeedIfEmpty(ctx, st, path)
	if rows, _ := count(); skipped || err == nil || rows != 0 {
		t.Errorf("boot 2: a failed seed must not read as already seeded: seeded=%d skipped=%v err=%v, %d of 3 rows served", seeded, skipped, err, rows)
	}

	// Boot 3, with the file repaired: the whole seed lands.
	seeded, _, skipped, err = providers.SeedIfEmpty(ctx, st, writeProvidersFile(t, strings.ReplaceAll(partialSeedFile, `\u0000`, "")))
	if rows, audit := count(); err != nil || skipped || seeded != 3 || rows != 3 || audit != 3 {
		t.Errorf("boot 3: the repaired file must seed whole: seeded=%d skipped=%v err=%v, %d rows and %d audit entries", seeded, skipped, err, rows, audit)
	}
}

// Two processes booting against one empty database (a rolling restart, a
// second replica) must not both seed: one lands the file, the rest see a
// store that is already seeded — never a "row already exists" boot failure.
func TestSeedIfEmptyConcurrentSeeders(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	path := writeProvidersFile(t, seedFile)

	const seeders = 8
	type result struct {
		seeded  int
		skipped bool
		err     error
	}
	results := make([]result, seeders)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := &results[i]
			r.seeded, _, r.skipped, r.err = providers.SeedIfEmpty(ctx, st, path)
		}()
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, r := range results {
		switch {
		case r.err != nil:
			t.Errorf("seeder %d failed instead of seeing an already-seeded store: seeded=%d err=%v", i, r.seeded, r.err)
		case r.skipped && r.seeded == 0:
		case !r.skipped && r.seeded == 2:
			winners++
		default:
			t.Errorf("seeder %d landed part of the file: seeded=%d skipped=%v", i, r.seeded, r.skipped)
		}
	}
	if winners != 1 {
		t.Errorf("exactly one seeder must land the file, got %d", winners)
	}
	rows, err := st.ListProviders(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	entries, err := st.ListAudit(ctx, 50)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(rows) != 2 || len(entries) != 2 {
		t.Errorf("the file must land exactly once: %d rows, %d audit entries", len(rows), len(entries))
	}
}

// captureLogs points the default slog logger at a buffer for one test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })
	return &logs
}

// driftedFile is seedFile after an operator edited it as if it were still
// the control: lpr_local's audience changed, lpr_owner moved gateways and
// keys, and lpr_new was added. None of it takes effect after first boot.
const driftedFile = `{
  "providers": [
    {"id": "lpr_local", "providerId": "litellm", "name": "Local",
     "baseURL": "https://gw.example/v1", "apiKeyEnv": "KEY_LOCAL", "groups": ["owner"],
     "models": [{"id": "qwen-27b", "name": "Qwen 27B"}]},
    {"id": "lpr_owner", "providerId": "litellm", "name": "Owner",
     "baseURL": "https://other.example/v1", "apiKeyEnv": "KEY_ROTATED",
     "models": [{"id": "gpt-5", "name": "GPT-5"}]},
    {"id": "lpr_new", "providerId": "litellm", "name": "New",
     "baseURL": "https://gw.example/v1", "apiKeyEnv": "KEY_NEW", "models": []}
  ]
}`

// After first boot the file is only history (ADR-0004), but a deployment's
// inventory may still present it as the control. An edit there — a removed
// row, a narrowed audience — silently does nothing, so each drifted row is
// named at Warn on every boot that skips the seed.
func TestSeedIfEmptyWarnsOncePerDriftedRow(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	if _, _, _, err := providers.SeedIfEmpty(ctx, st, writeProvidersFile(t, seedFile)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.CreateProvider(ctx, store.ProviderRow{
		ID: "lpr_console", ProviderID: "litellm", Name: "Console",
		BaseURL: "https://gw.example/v1", APIKeyEnv: "KEY_CONSOLE",
	}, "admin"); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Setenv("KEY_LOCAL", "sk-local-key-value")
	t.Setenv("KEY_ROTATED", "sk-rotated-key-value")

	logs := captureLogs(t)
	_, _, skipped, err := providers.SeedIfEmpty(ctx, st, writeProvidersFile(t, driftedFile))
	if err != nil || !skipped {
		t.Fatalf("a seeded store skips the file: skipped=%v err=%v", skipped, err)
	}
	written := logs.String()
	for _, want := range []string{
		"provider=lpr_local drift=groups ",
		"provider=lpr_owner drift=baseURL,apiKeyEnv ",
		"provider=lpr_new drift=file-only ",
		"provider=lpr_console drift=store-only ",
	} {
		if !strings.Contains(written, want) {
			t.Errorf("log must contain %q, got:\n%s", want, written)
		}
	}
	if n := strings.Count(written, "level=WARN"); n != 4 {
		t.Errorf("one warning per drifted row, got %d:\n%s", n, written)
	}
	if strings.Contains(written, "sk-") {
		t.Errorf("a drift warning names the variable, never a key value:\n%s", written)
	}
	// The warning changes nothing: the store still serves what it held.
	row, err := st.GetProvider(ctx, "lpr_local")
	if err != nil || len(row.Groups) != 1 || row.Groups[0] != "family" {
		t.Errorf("the store row must be untouched: %#v %v", row.Groups, err)
	}
	if _, err := st.GetProvider(ctx, "lpr_new"); err != store.ErrNotFound {
		t.Errorf("a row added to the file after first boot must not be inserted, got %v", err)
	}
}

// A file that still matches the store warns about nothing; groups are an
// any-of set, so their order in the file is not drift.
func TestSeedIfEmptyQuietWhenFileMatchesStore(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	const file = `{"providers": [{"id": "lpr_a", "providerId": "litellm", "name": "A",
		"baseURL": "https://gw.example/v1", "apiKeyEnv": "KEY_A", "groups": %s, "models": []}]}`
	if _, _, _, err := providers.SeedIfEmpty(ctx, st, writeProvidersFile(t, fmt.Sprintf(file, `["family", "owner"]`))); err != nil {
		t.Fatalf("seed: %v", err)
	}

	logs := captureLogs(t)
	_, _, skipped, err := providers.SeedIfEmpty(ctx, st, writeProvidersFile(t, fmt.Sprintf(file, `["owner", "family"]`)))
	if err != nil || !skipped {
		t.Fatalf("a seeded store skips the file: skipped=%v err=%v", skipped, err)
	}
	if written := logs.String(); strings.Contains(written, "level=WARN") {
		t.Fatalf("a file that matches the store must not warn, got:\n%s", written)
	}
}

// The store is authoritative once seeded, so a file that no longer parses
// is a warning, never a reason to stop boot.
func TestSeedIfEmptyUnreadableFileAfterSeedOnlyWarns(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	if _, _, _, err := providers.SeedIfEmpty(ctx, st, writeProvidersFile(t, seedFile)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	logs := captureLogs(t)
	seeded, _, skipped, err := providers.SeedIfEmpty(ctx, st, writeProvidersFile(t, `{"providers": [`))
	if err != nil || !skipped || seeded != 0 {
		t.Fatalf("a broken file must not stop a seeded deployment: seeded=%d skipped=%v err=%v", seeded, skipped, err)
	}
	written := logs.String()
	if n := strings.Count(written, "level=WARN"); n != 1 {
		t.Errorf("the unreadable file must be warned about once, got %d:\n%s", n, written)
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

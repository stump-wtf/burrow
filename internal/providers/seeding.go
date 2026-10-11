// First-boot seeding: BURROW_PROVIDERS_FILE is consulted only while the
// llm_providers table is empty, so an existing deployment adopts the store
// with no behavior change and no data migration (ADR-0004, SPEC-0004 REQ
// "Rows in the store", scenario "First-boot seed").
package providers

import (
	"context"
	"fmt"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// SeedActor is the audit identity recorded for rows inserted by seeding.
const SeedActor = "seed"

// SeedIfEmpty runs the first-boot seed: with an empty llm_providers table
// the file is loaded and inserted (model names becoming display-name
// overrides); with a non-empty table the file is ignored entirely and
// skipped is true (SPEC-0004 REQ "Rows in the store", scenario "First-boot
// seed"). warnings names the rows whose model-level groups were dropped.
func SeedIfEmpty(ctx context.Context, st *store.Store, path string) (seeded int, warnings []string, skipped bool, err error) {
	n, err := st.CountProviders(ctx)
	if err != nil {
		return 0, nil, false, err
	}
	if n > 0 {
		return 0, nil, true, nil
	}
	seeded, warnings, err = SeedFromFile(ctx, st, path)
	return seeded, warnings, false, err
}

// SeedFromFile validates the seed file and inserts each row into the store.
// File model names become display-name overrides; model-level groups are
// dropped (ADR-0004 supersedes them for derived menus) and their row ids are
// returned as warnings for main.go to log. Insertion is all-or-log: a row
// that fails to insert (duplicate id, lost race) stops the seed with an
// error, leaving what landed auditable.
func SeedFromFile(ctx context.Context, st *store.Store, path string) (seeded int, warnings []string, err error) {
	list, err := Load(path)
	if err != nil {
		return 0, nil, err
	}
	mixed, err := modelGroupRows(path)
	if err != nil {
		return 0, nil, fmt.Errorf("providers: re-read %s: %w", path, err)
	}
	dropped := make(map[string]bool, len(mixed))
	for _, id := range mixed {
		dropped[id] = true
	}
	for _, p := range list {
		overrides := map[string]string{}
		for _, m := range p.Models {
			if m.Name != "" {
				overrides[m.ID] = m.Name
			}
		}
		row := store.ProviderRow{
			ID:            p.ID,
			ProviderID:    p.ProviderID,
			Name:          p.Name,
			BaseURL:       p.BaseURL,
			APIKeyEnv:     p.APIKeyEnv,
			Groups:        p.Groups,
			NameOverrides: overrides,
		}
		if err := validateStoreRow(row); err != nil {
			return seeded, warnings, fmt.Errorf("providers: seed %s: %w", p.ID, err)
		}
		if err := st.CreateProvider(ctx, row, SeedActor); err != nil {
			return seeded, warnings, fmt.Errorf("providers: seed %s: %w", p.ID, err)
		}
		seeded++
		if dropped[p.ID] {
			warnings = append(warnings, p.ID)
		}
	}
	return seeded, warnings, nil
}

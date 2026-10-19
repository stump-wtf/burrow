// First-boot seeding: BURROW_PROVIDERS_FILE is consulted only while the
// provider store has never been written, so an existing deployment adopts
// the store with no behavior change and no data migration (ADR-0004,
// SPEC-0004 REQ "Rows in the store", scenario "First-boot seed").
package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// SeedActor is the audit identity recorded for rows inserted by seeding.
const SeedActor = "seed"

// SeedIfEmpty runs the first-boot seed: with a provider store nothing has
// ever written, the file is loaded and inserted (model names becoming
// display-name overrides); otherwise nothing in the file takes effect and
// skipped is true (SPEC-0004 REQ "Rows in the store", scenario "First-boot
// seed"). "Never written" is the store's answer, not "the table is empty":
// an admin who deleted every row pulled the org's models on purpose, and the
// next restart must not bring them back with the file's audiences. warnings
// names the rows whose model-level groups were dropped. A skipped file is
// still read once, only to warn about rows where it has drifted from the
// store (warnSeedDrift).
func SeedIfEmpty(ctx context.Context, st *store.Store, path string, pol Policy) (seeded int, warnings []string, skipped bool, err error) {
	done, err := st.ProvidersSeeded(ctx)
	if err != nil {
		return 0, nil, false, err
	}
	if done {
		warnSeedDrift(ctx, st, path)
		return 0, nil, true, nil
	}
	seeded, warnings, err = SeedFromFile(ctx, st, path, pol)
	if errors.Is(err, store.ErrAlreadySeeded) {
		// Another process booting on the same database seeded it between
		// the check above and the insert: that is a skip, not a failure.
		warnSeedDrift(ctx, st, path)
		return 0, nil, true, nil
	}
	return seeded, warnings, false, err
}

// warnSeedDrift logs one Warn per row on which the ignored file and the
// store disagree: a row only in the file, a row only in the store, or
// groups, baseURL or apiKeyEnv differing. After first boot an edit to the
// file does nothing by design (ADR-0004), but a deployment's inventory may
// still document the file as the control — removing a row there, or
// narrowing its groups, would otherwise change nothing and say nothing. The
// warning names the row and the field, never a value. A file that no longer
// reads or validates is a warning too, not a boot failure: the store, not
// the file, is authoritative here.
func warnSeedDrift(ctx context.Context, st *store.Store, path string) {
	list, err := Load(path)
	if err != nil {
		slog.Warn("llm providers file is ignored and could not be loaded; the store is authoritative, so it was not compared", "file", path, "error", err)
		return
	}
	rows, err := st.ListProviders(ctx)
	if err != nil {
		slog.Warn("llm providers file was not compared with the store", "file", path, "error", err)
		return
	}
	stored := make(map[string]store.ProviderRow, len(rows))
	for _, row := range rows {
		stored[row.ID] = row
	}
	const msg = "llm providers file differs from the store; the file is only a first-boot seed, so its version has no effect — change the row through the admin API or console"
	inFile := make(map[string]bool, len(list))
	for _, p := range list {
		inFile[p.ID] = true
		row, ok := stored[p.ID]
		if !ok {
			slog.Warn(msg, "provider", p.ID, "drift", "file-only", "file", path)
			continue
		}
		var fields []string
		if !sameGroups(p.Groups, row.Groups) {
			fields = append(fields, "groups")
		}
		if p.BaseURL != row.BaseURL {
			fields = append(fields, "baseURL")
		}
		if p.APIKeyEnv != row.APIKeyEnv {
			fields = append(fields, "apiKeyEnv")
		}
		if len(fields) > 0 {
			slog.Warn(msg, "provider", p.ID, "drift", strings.Join(fields, ","), "file", path)
		}
	}
	for _, row := range rows {
		if !inFile[row.ID] {
			slog.Warn(msg, "provider", row.ID, "drift", "store-only", "file", path)
		}
	}
}

// sameGroups compares two audiences as the any-of sets they are (SPEC-0003
// REQ "Visibility"), so reordering a file's groups is not drift.
func sameGroups(a, b []string) bool {
	return slices.Equal(slices.Compact(slices.Sorted(slices.Values(a))), slices.Compact(slices.Sorted(slices.Values(b))))
}

// SeedFromFile validates the seed file and inserts its rows into the store.
// File model names become display-name overrides; model-level groups are
// dropped (ADR-0004 supersedes them for derived menus) and their row ids are
// returned as warnings for main.go to log. The seed is all-or-nothing: every
// row and its audit entry land in one transaction, so a row that fails to
// insert leaves the store as unseeded as it was and the next boot tries the
// whole file again. A store that has already been written is refused with
// store.ErrAlreadySeeded.
//
// A row that is malformed stops the boot, as it always did. A row that only
// trips the allowlist is logged and seeded anyway (ADR-0005: "Seeding"): the
// file is the deployment's own rendering, so a row outside the lists is a
// list that has not converged, and refusing to boot would take the whole
// control plane down over it. Such a row serves nothing until either side is
// corrected.
func SeedFromFile(ctx context.Context, st *store.Store, path string, pol Policy) (seeded int, warnings []string, err error) {
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
	rows := make([]store.ProviderRow, 0, len(list))
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
		if err := validateStoreRow(row, pol); err != nil {
			if errors.Is(err, ErrNotAllowed) {
				slog.Warn("seeded llm provider row is outside the allowlist; it serves nothing until the row or the configuration is corrected",
					"provider", p.ID, "file", path, "reason", err)
			} else {
				return 0, nil, fmt.Errorf("providers: seed %s: %w", p.ID, err)
			}
		}
		rows = append(rows, row)
		if dropped[p.ID] {
			warnings = append(warnings, p.ID)
		}
	}
	if err := st.SeedProviders(ctx, rows, SeedActor); err != nil {
		return 0, nil, fmt.Errorf("providers: seed: %w", err)
	}
	return len(rows), warnings, nil
}

package store_test

// Governing: SPEC-0004 REQ "Rows in the store", "Audit", "Admin
// authorization" — provider CRUD with in-transaction audit, and the API
// token lifecycle.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

func testProviderRow(id string) store.ProviderRow {
	return store.ProviderRow{
		ID:            id,
		ProviderID:    "litellm",
		Name:          "Gateway",
		BaseURL:       "https://gw.example/v1",
		APIKeyEnv:     "GW_KEY",
		NameOverrides: map[string]string{"gpt-5": "GPT-5"},
	}
}

// auditFor returns the audit entries for one provider, newest first.
func auditFor(t *testing.T, st *store.Store, id string) []store.AuditEntry {
	t.Helper()
	entries, err := st.ListAudit(context.Background(), 100)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	var out []store.AuditEntry
	for _, e := range entries {
		if e.ProviderID == id {
			out = append(out, e)
		}
	}
	return out
}

func TestCreateProviderDuplicateRejected(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.CreateProvider(ctx, testProviderRow("lpr_a"), "tester"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.CreateProvider(ctx, testProviderRow("lpr_a"), "tester"); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate id must fail with ErrDuplicate, got %v", err)
	}
}

func TestProviderRoundTripGroupsAndOverrides(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	row := testProviderRow("lpr_rt")
	row.Groups = []string{"family", "friends"}
	if err := st.CreateProvider(ctx, row, "tester"); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.GetProvider(ctx, "lpr_rt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if strings.Join(got.Groups, ",") != "family,friends" {
		t.Fatalf("groups round trip: %#v", got.Groups)
	}
	if got.NameOverrides["gpt-5"] != "GPT-5" {
		t.Fatalf("name overrides round trip: %#v", got.NameOverrides)
	}
	if !got.CreatedAt.Equal(got.UpdatedAt) {
		t.Fatalf("a fresh row's updated_at is its created_at: %v vs %v", got.CreatedAt, got.UpdatedAt)
	}

	// NULL groups means every member and must read back as nil, not [""].
	open := testProviderRow("lpr_open")
	if err := st.CreateProvider(ctx, open, "tester"); err != nil {
		t.Fatalf("create open: %v", err)
	}
	got, err = st.GetProvider(ctx, "lpr_open")
	if err != nil {
		t.Fatalf("get open: %v", err)
	}
	if got.Groups != nil {
		t.Fatalf("absent audience must read back nil, got %#v", got.Groups)
	}
}

func TestUpdateProviderIsFullReplace(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	row := testProviderRow("lpr_u")
	row.Groups = []string{"g"}
	if err := st.CreateProvider(ctx, row, "tester"); err != nil {
		t.Fatalf("create: %v", err)
	}
	created, err := st.GetProvider(ctx, "lpr_u")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	replacement := store.ProviderRow{
		ID: "lpr_u", ProviderID: "other", Name: "Renamed",
		BaseURL: "https://gw2.example/v1", APIKeyEnv: "GW2_KEY",
	}
	updated, err := st.UpdateProvider(ctx, replacement, time.Time{}, "tester")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !updated.After(created.UpdatedAt) || updated.Equal(created.UpdatedAt) {
		t.Fatalf("update must advance updated_at: %v → %v", created.UpdatedAt, updated)
	}
	got, err := st.GetProvider(ctx, "lpr_u")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Name != "Renamed" || got.BaseURL != "https://gw2.example/v1" || got.APIKeyEnv != "GW2_KEY" || got.ProviderID != "other" {
		t.Fatalf("full replace must overwrite every field: %#v", got)
	}
	if got.Groups != nil {
		t.Fatalf("omitted groups in the replacement must clear the audience, got %#v", got.Groups)
	}
	if len(got.NameOverrides) != 0 {
		t.Fatalf("omitted nameOverrides in the replacement must clear them, got %#v", got.NameOverrides)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("created_at must survive a replace: %v vs %v", got.CreatedAt, created.CreatedAt)
	}

	if _, err := st.UpdateProvider(ctx, testProviderRow("lpr_missing"), time.Time{}, "tester"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update of an unknown row must be ErrNotFound, got %v", err)
	}
}

func TestUpdateProviderStalePrecondition(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.CreateProvider(ctx, testProviderRow("lpr_s"), "tester"); err != nil {
		t.Fatalf("create: %v", err)
	}
	first, err := st.GetProvider(ctx, "lpr_s")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// A racing writer lands first; the second writer's expectation — read
	// before the race — must fail inside the row lock, not clobber it.
	winner := testProviderRow("lpr_s")
	winner.Name = "Winner"
	if _, err := st.UpdateProvider(ctx, winner, first.UpdatedAt, "winner"); err != nil {
		t.Fatalf("first update: %v", err)
	}
	loser := testProviderRow("lpr_s")
	loser.Name = "Loser"
	if _, err := st.UpdateProvider(ctx, loser, first.UpdatedAt, "loser"); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale expectation must be ErrStale, got %v", err)
	}
	got, err := st.GetProvider(ctx, "lpr_s")
	if err != nil {
		t.Fatalf("get after race: %v", err)
	}
	if got.Name != "Winner" {
		t.Fatalf("the stale writer must not clobber the winner, got %q", got.Name)
	}
}

// A delete that names the version it read removes only that version: after
// another writer lands, the same expectation is ErrStale, the row and the
// winner's change survive, and nothing is audited for the refused delete.
func TestDeleteProviderStalePrecondition(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.CreateProvider(ctx, testProviderRow("lpr_a"), "alice"); err != nil {
		t.Fatalf("create: %v", err)
	}
	read, err := st.GetProvider(ctx, "lpr_a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	winner := testProviderRow("lpr_a")
	winner.Name = "Winner"
	current, err := st.UpdateProvider(ctx, winner, read.UpdatedAt, "winner")
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	if err := st.DeleteProvider(ctx, "lpr_a", read.UpdatedAt, "loser"); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale expectation must be ErrStale, got %v", err)
	}
	got, err := st.GetProvider(ctx, "lpr_a")
	if err != nil || got.Name != "Winner" {
		t.Fatalf("a stale delete must leave the winner's row, got %q, %v", got.Name, err)
	}
	if actions := actionsOf(auditFor(t, st, "lpr_a")); strings.Join(actions, ",") != "update,create" {
		t.Fatalf("a refused delete must not be audited, got %v", actions)
	}

	if err := st.DeleteProvider(ctx, "lpr_a", current, "alice"); err != nil {
		t.Fatalf("delete with the current expectation: %v", err)
	}
	if _, err := st.GetProvider(ctx, "lpr_a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted row must be gone, got %v", err)
	}
	if err := st.DeleteProvider(ctx, "lpr_a", current, "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a missing row is ErrNotFound whatever the expectation, got %v", err)
	}
}

func TestDeleteAndAuditTrail(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.CreateProvider(ctx, testProviderRow("lpr_a"), "alice"); err != nil {
		t.Fatalf("create: %v", err)
	}
	row := testProviderRow("lpr_a")
	row.Name = "Renamed"
	row.Groups = []string{"g"}
	if _, err := st.UpdateProvider(ctx, row, time.Time{}, "ci-token"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := st.DeleteProvider(ctx, "lpr_a", time.Time{}, "alice"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeleteProvider(ctx, "lpr_a", time.Time{}, "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete must be ErrNotFound, got %v", err)
	}

	entries := auditFor(t, st, "lpr_a")
	if len(entries) != 3 {
		t.Fatalf("create+update+delete must leave three audit rows, got %d", len(entries))
	}
	// Newest first.
	if entries[0].Action != "delete" || entries[1].Action != "update" || entries[2].Action != "create" {
		t.Fatalf("audit must be newest-first, got %v %v %v", entries[0].Action, entries[1].Action, entries[2].Action)
	}
	if entries[0].Actor != "alice" || entries[1].Actor != "ci-token" {
		t.Fatalf("audit must record the acting identity: %v / %v", entries[0].Actor, entries[1].Actor)
	}
	// Delete: full before-image, no after.
	var before map[string]any
	if err := json.Unmarshal(entries[0].Before, &before); err != nil {
		t.Fatalf("delete before-image must be the row JSON: %v", err)
	}
	if before["name"] != "Renamed" || before["id"] != "lpr_a" {
		t.Fatalf("delete before-image wrong: %#v", before)
	}
	if len(entries[0].After) != 0 {
		t.Fatalf("delete must have no after-image, got %s", entries[0].After)
	}
	// Create: after-image carries the name overrides.
	var after map[string]any
	if err := json.Unmarshal(entries[2].After, &after); err != nil {
		t.Fatalf("create after-image must be the row JSON: %v", err)
	}
	overrides, ok := after["nameOverrides"].(map[string]any)
	if !ok || overrides["gpt-5"] != "GPT-5" {
		t.Fatalf("create after-image must carry nameOverrides: %#v", after)
	}
	if entries[1].Before == nil || entries[1].After == nil {
		t.Fatalf("update must carry both images: %s → %s", entries[1].Before, entries[1].After)
	}
	// The audit names environment variables, never key values: nothing in
	// the bodies may look like a provisioned secret.
	for _, e := range entries {
		for _, raw := range [][]byte{e.Before, e.After} {
			if len(raw) == 0 {
				continue
			}
			if !strings.Contains(string(raw), "GW_KEY") {
				t.Fatalf("audit row must name the key env var: %s", raw)
			}
		}
	}
}

func TestListAuditLimit(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"lpr_1", "lpr_2", "lpr_3"} {
		if err := st.CreateProvider(ctx, testProviderRow(id), "tester"); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	entries, err := st.ListAudit(ctx, 2)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("limit must cap the page, got %d", len(entries))
	}
	if entries[0].ProviderID != "lpr_3" || entries[1].ProviderID != "lpr_2" {
		t.Fatalf("newest first expected: %v %v", entries[0].ProviderID, entries[1].ProviderID)
	}
}

func TestBumpProviderUpdatedAt(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.CreateProvider(ctx, testProviderRow("lpr_b"), "seed"); err != nil {
		t.Fatalf("create: %v", err)
	}
	before, err := st.GetProvider(ctx, "lpr_b")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	bumped, err := st.BumpProviderUpdatedAt(ctx, "lpr_b")
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	if !bumped.After(before.UpdatedAt) {
		t.Fatalf("bump must advance updated_at: %v → %v", before.UpdatedAt, bumped)
	}
	// A menu change is a gateway event, not an admin edit: no audit row.
	if entries := auditFor(t, st, "lpr_b"); len(entries) != 1 || entries[0].Action != "create" {
		t.Fatalf("bump must not write audit, got %v", actionsOf(entries))
	}
}

func actionsOf(entries []store.AuditEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Action)
	}
	return out
}

func TestListAndCountProviders(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if n, err := st.CountProviders(ctx); err != nil || n != 0 {
		t.Fatalf("a fresh store counts zero: %d %v", n, err)
	}
	for _, id := range []string{"lpr_1", "lpr_2"} {
		if err := st.CreateProvider(ctx, testProviderRow(id), "tester"); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	n, err := st.CountProviders(ctx)
	if err != nil || n != 2 {
		t.Fatalf("count: %d %v", n, err)
	}
	rows, err := st.ListProviders(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "lpr_1" || rows[1].ID != "lpr_2" {
		t.Fatalf("list must be stable order: %+v", rows)
	}
}

func TestAPITokenLifecycle(t *testing.T) {
	st := openTestStore(t)
	user := seedUser(t, st)
	ctx := context.Background()

	raw, tok, err := st.MintAPIToken(ctx, user.ID, "ci", []string{"llm-admin"}, time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(raw, store.AdminTokenPrefix) {
		t.Fatalf("the raw token must carry the distinguishing prefix: %q", raw)
	}
	if strings.Contains(strings.Join(listHashes(t, st), " "), raw) {
		t.Fatal("listing must never echo the raw token")
	}

	got, err := st.LookupAPIToken(ctx, raw)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.Name != "ci" || len(got.Scopes) != 1 || got.Scopes[0] != "llm-admin" {
		t.Fatalf("token record wrong: %+v", got)
	}
	if got.LastUsedAt == nil {
		t.Fatal("every use must stamp last_used_at")
	}

	if err := st.RevokeAPIToken(ctx, tok.TokenHash); err != nil {
		t.Fatalf("revoke by hash: %v", err)
	}
	if _, err := st.LookupAPIToken(ctx, raw); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a revoked token must be ErrNotFound, got %v", err)
	}
	if err := st.RevokeAPIToken(ctx, tok.TokenHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoking an unknown token must be ErrNotFound, got %v", err)
	}
}

func TestAPITokenRevokeByRawValue(t *testing.T) {
	st := openTestStore(t)
	user := seedUser(t, st)
	ctx := context.Background()
	raw, _, err := st.MintAPIToken(ctx, user.ID, "ci", []string{"llm-admin"}, time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := st.RevokeAPIToken(ctx, raw); err != nil {
		t.Fatalf("revoke by raw value: %v", err)
	}
	if _, err := st.LookupAPIToken(ctx, raw); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("token must be gone, got %v", err)
	}
}

func TestAPITokenExpiry(t *testing.T) {
	st := openTestStore(t)
	user := seedUser(t, st)
	ctx := context.Background()
	raw, _, err := st.MintAPIToken(ctx, user.ID, "stale", []string{"llm-admin"}, -time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := st.LookupAPIToken(ctx, raw); !errors.Is(err, store.ErrExpired) {
		t.Fatalf("an expired token must be ErrExpired (invalid_token, never a session code), got %v", err)
	}
}

func listHashes(t *testing.T, st *store.Store) []string {
	t.Helper()
	tokens, err := st.ListAPITokens(context.Background())
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	var out []string
	for _, tok := range tokens {
		out = append(out, tok.TokenHash)
	}
	return out
}

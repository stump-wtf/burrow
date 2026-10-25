package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// Governing: SPEC-0003 REQ "Model audience" — a model that carries no rule
// follows its row, a rule is replaced whole by the next save, and the rules of a
// row that is gone are gone with it.

func seedModelProviders(t *testing.T, st *store.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := st.CreateProvider(context.Background(), testProviderRow(id), "seed"); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
}

func modelRows(t *testing.T, st *store.Store) string {
	t.Helper()
	rules, err := st.ListModelAudiences(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	parts := make([]string, 0, len(rules))
	for _, rule := range rules {
		parts = append(parts, rule.ProviderID+"/"+rule.ModelID+"="+strings.Join(rule.Groups, "+"))
	}
	return strings.Join(parts, ",")
}

func saveModels(t *testing.T, st *store.Store, rules ...store.ModelAudience) string {
	t.Helper()
	current, err := st.ListModelAudiences(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if err := st.SaveModelAudiences(context.Background(), store.ModelAudiencesRevision(current), rules, "test"); err != nil {
		t.Fatalf("save: %v", err)
	}
	return modelRows(t, st)
}

func TestModelAudiencesDefaultToNone(t *testing.T) {
	st := openTestStore(t)
	if got := modelRows(t, st); got != "" {
		t.Fatalf("a fresh store declares nothing, got %q", got)
	}
}

func TestSaveModelAudiencesReplacesTheWholeSet(t *testing.T) {
	st := openTestStore(t)
	seedModelProviders(t, st, "lpr_local")

	if got := saveModels(t, st,
		store.ModelAudience{ProviderID: "lpr_local", ModelID: "qwen3.8-flash-next", Groups: []string{"family", "jon", "family"}},
		store.ModelAudience{ProviderID: "lpr_local", ModelID: "glm-5.3-balanced", Groups: []string{"family"}}); got !=
		"lpr_local/glm-5.3-balanced=family,lpr_local/qwen3.8-flash-next=family+jon" {
		t.Fatalf("saved rules = %q; duplicates must collapse and groups must be ordered", got)
	}

	// A second save replaces: a model it does not name carries no rule again,
	// which returns it to following its row.
	if got := saveModels(t, st,
		store.ModelAudience{ProviderID: "lpr_local", ModelID: "glm-5.3-balanced", Groups: []string{"family"}}); got !=
		"lpr_local/glm-5.3-balanced=family" {
		t.Fatalf("after replace = %q", got)
	}
}

func TestSaveModelAudiencesRefusesAnEmptyAudience(t *testing.T) {
	st := openTestStore(t)
	seedModelProviders(t, st, "lpr_local")
	current, err := st.ListModelAudiences(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	err = st.SaveModelAudiences(context.Background(), store.ModelAudiencesRevision(current),
		[]store.ModelAudience{{ProviderID: "lpr_local", ModelID: "gpt-5", Groups: nil}}, "test")
	if err == nil || !strings.Contains(err.Error(), "at least one group") {
		t.Fatalf("an audience of nobody must be refused as the mistake it is, got %v", err)
	}
	if got := modelRows(t, st); got != "" {
		t.Fatalf("the refused save stored %q", got)
	}
}

func TestSaveModelAudiencesRefusesAStalePage(t *testing.T) {
	st := openTestStore(t)
	seedModelProviders(t, st, "lpr_local")
	ctx := context.Background()
	current, err := st.ListModelAudiences(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	revision := store.ModelAudiencesRevision(current)
	if err := st.SaveModelAudiences(ctx, revision,
		[]store.ModelAudience{{ProviderID: "lpr_local", ModelID: "gpt-5", Groups: []string{"family"}}}, "test"); err != nil {
		t.Fatalf("first save: %v", err)
	}
	err = st.SaveModelAudiences(ctx, revision,
		[]store.ModelAudience{{ProviderID: "lpr_local", ModelID: "gpt-5", Groups: []string{"jon"}}}, "test")
	if !errors.Is(err, store.ErrModelAudiencesChanged) {
		t.Fatalf("a second save from the same page must be refused, got %v", err)
	}
	if got := modelRows(t, st); got != "lpr_local/gpt-5=family" {
		t.Fatalf("the refused save changed %q", got)
	}
}

// A save moves the timestamp of the rows whose rules actually changed: the
// snapshot a member's client polls is id→updated_at, so a menu that narrowed
// under an unchanged timestamp would stay cached on the desktop.
func TestSaveModelAudiencesBumpsOnlyChangedRows(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	seedModelProviders(t, st, "lpr_local", "lpr_gateway")

	before, err := st.GetProvider(ctx, "lpr_local")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	untouched, err := st.GetProvider(ctx, "lpr_gateway")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	saveModels(t, st, store.ModelAudience{ProviderID: "lpr_local", ModelID: "gpt-5", Groups: []string{"family"}})

	after, err := st.GetProvider(ctx, "lpr_local")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("a row whose rules moved must advance: %v then %v", before.UpdatedAt, after.UpdatedAt)
	}
	if again, err := st.GetProvider(ctx, "lpr_gateway"); err != nil {
		t.Fatalf("get: %v", err)
	} else if !again.UpdatedAt.Equal(untouched.UpdatedAt) {
		t.Fatalf("a row nobody changed must not move: %v", again.UpdatedAt)
	}

	entries, err := st.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	var audited []string
	for _, entry := range entries {
		if entry.Action == "model-audiences" {
			audited = append(audited, entry.ProviderID+" "+string(entry.After))
		}
	}
	if len(audited) != 1 || !strings.Contains(audited[0], "lpr_local") || !strings.Contains(audited[0], "family") {
		t.Fatalf("exactly the changed row's rules must be audited, got %v", audited)
	}

	// Saving the same rules again changes nothing, so it moves no timestamp and
	// appends no audit entry: an admin reopening a page and saving it is not an
	// edit.
	before, err = st.GetProvider(ctx, "lpr_local")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	saveModels(t, st, store.ModelAudience{ProviderID: "lpr_local", ModelID: "gpt-5", Groups: []string{"family"}})
	after, err = st.GetProvider(ctx, "lpr_local")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("an unchanged save must not move updated_at: %v then %v", before.UpdatedAt, after.UpdatedAt)
	}
	entries, err = st.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	count := 0
	for _, entry := range entries {
		if entry.Action == "model-audiences" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("an unchanged save must add no audit entry, got %d", count)
	}
}

// Deleting the row takes its rules with it, so a row later created with the same
// id cannot inherit an audience nobody wrote for it.
func TestModelAudiencesGoWithTheRow(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	seedModelProviders(t, st, "lpr_local")
	saveModels(t, st, store.ModelAudience{ProviderID: "lpr_local", ModelID: "gpt-5", Groups: []string{"family"}})

	row, err := st.GetProvider(ctx, "lpr_local")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := st.DeleteProvider(ctx, "lpr_local", row.UpdatedAt, "test"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := modelRows(t, st); got != "" {
		t.Fatalf("a deleted row's rules must go with it, got %q", got)
	}
}

func TestModelAudiencesRevisionIgnoresOrder(t *testing.T) {
	a := store.ModelAudiencesRevision([]store.ModelAudience{
		{ProviderID: "lpr_a", ModelID: "m1", Groups: []string{"family", "jon"}},
		{ProviderID: "lpr_b", ModelID: "m2", Groups: []string{"joe"}},
	})
	b := store.ModelAudiencesRevision([]store.ModelAudience{
		{ProviderID: "lpr_b", ModelID: "m2", Groups: []string{"joe"}},
		{ProviderID: "lpr_a", ModelID: "m1", Groups: []string{"jon", "family"}},
	})
	if a != b {
		t.Fatalf("the same rules in another order must fingerprint the same: %s vs %s", a, b)
	}
	c := store.ModelAudiencesRevision([]store.ModelAudience{
		{ProviderID: "lpr_a", ModelID: "m1", Groups: []string{"family", "jon", "extra"}},
		{ProviderID: "lpr_b", ModelID: "m2", Groups: []string{"joe"}},
	})
	if a == c {
		t.Fatal("a wider audience must not fingerprint the same")
	}
}

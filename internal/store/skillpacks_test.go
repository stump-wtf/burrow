package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// Governing: SPEC-0003 REQ "Library audience" — the mapping an admin edits is
// group to skills, and "no rule written" must stay distinguishable from a rule
// that names no skills, because the first is the org-wide default.

func TestSkillPackDefaultsToNoRules(t *testing.T) {
	st := openTestStore(t)
	packs, err := st.ListSkillPacks(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(packs) != 0 {
		t.Fatalf("a fresh store has no rules, got %v", packs)
	}
}

func TestSetSkillPackReplacesOneGroup(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.SetSkillPack(ctx, "family", []string{"code-review", "deploy-helper", "code-review"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := st.SetSkillPack(ctx, "friends", []string{"deploy-helper"}); err != nil {
		t.Fatalf("set friends: %v", err)
	}
	packs, err := st.ListSkillPacks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := packs["family"]; strings.Join(got, ",") != "code-review,deploy-helper" {
		t.Fatalf("duplicate names must collapse: %v", got)
	}
	if got := packs["friends"]; strings.Join(got, ",") != "deploy-helper" {
		t.Fatalf("friends: %v", got)
	}

	// Replacing is a replace, not an append: the second save's list is the
	// group's whole allowance.
	if err := st.SetSkillPack(ctx, "family", []string{"code-review"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	packs, err = st.ListSkillPacks(ctx)
	if err != nil {
		t.Fatalf("list after replace: %v", err)
	}
	if got := packs["family"]; strings.Join(got, ",") != "code-review" {
		t.Fatalf("family: %v", got)
	}
	if _, mapped := packs["friends"]; !mapped {
		t.Fatalf("one group's save must not touch another's: %v", packs)
	}
}

func TestClearSkillPackDeclaresAnEmptyAllowance(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.SetSkillPack(ctx, "family", []string{"code-review"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := st.SetSkillPack(ctx, "family", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	packs, err := st.ListSkillPacks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	allowance, declared := packs["family"]
	if !declared {
		t.Fatalf("an empty list is a rule that allows nothing, so the group stays declared: %v", packs)
	}
	if len(allowance) != 0 {
		t.Fatalf("the old allowance must be gone: %v", allowance)
	}
	if _, declared := packs["nobody"]; declared {
		t.Fatalf("a group never written must stay undeclared: %v", packs)
	}
}

func TestSetSkillPackNeedsAGroup(t *testing.T) {
	st := openTestStore(t)
	if err := st.SetSkillPack(context.Background(), "  ", []string{"code-review"}); err == nil {
		t.Fatalf("a blank group name would be an unaddressable rule")
	}
}

// Withdrawing a rule is not the same move as declaring an empty one, so the
// console needs both.
func TestForgetSkillPackWithdrawsTheRule(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.SetSkillPack(ctx, "family", []string{"code-review", "deploy-helper"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := st.ForgetSkillPack(ctx, "family"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	packs, err := st.ListSkillPacks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, declared := packs["family"]; declared {
		t.Fatalf("the rule must be gone, not emptied: %v", packs)
	}
	// Forgetting twice, and forgetting a group never declared, are no-ops.
	if err := st.ForgetSkillPack(ctx, "family"); err != nil {
		t.Fatalf("forget again: %v", err)
	}
	if err := st.ForgetSkillPack(ctx, "nobody"); err != nil {
		t.Fatalf("forget an undeclared group: %v", err)
	}
}

func TestMemberGroupsAreDeduplicatedAndSorted(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	members := []struct {
		id     string
		groups []string
	}{
		{"sub-a", []string{"everyone", "family"}},
		{"sub-b", []string{"family"}},
		{"sub-c", nil},
	}
	for i, m := range members {
		if err := st.UpsertUser(ctx, store.User{
			ID: m.id, Email: "member" + string(rune('0'+i)) + "@example.com", Groups: m.groups,
		}); err != nil {
			t.Fatalf("upsert %s: %v", m.id, err)
		}
	}
	got, err := st.MemberGroups(ctx)
	if err != nil {
		t.Fatalf("member groups: %v", err)
	}
	if strings.Join(got, ",") != "everyone,family" {
		t.Fatalf("want everyone,family got %v", got)
	}
}

// Two saves of one group must leave exactly one of the two allowances. Before
// the writers were serialized, the second DELETE could not see the first
// save's fresh rows, so the group ended up allowed the union — wider than
// either admin wrote — or the save died on the primary key.
func TestConcurrentSetSkillPackKeepsOneWritersAllowance(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.SetSkillPack(ctx, "family", []string{"seed"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for round := 0; round < 25; round++ {
		start := make(chan struct{})
		errs := make(chan error, 2)
		for _, skills := range [][]string{{"alpha", "shared"}, {"beta", "shared"}} {
			go func(skills []string) {
				<-start
				errs <- st.SetSkillPack(ctx, "family", skills)
			}(skills)
		}
		close(start)
		for i := 0; i < 2; i++ {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: a concurrent save failed: %v", round, err)
			}
		}
		packs, err := st.ListSkillPacks(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		got := strings.Join(packs["family"], ",")
		if got != "alpha,shared" && got != "beta,shared" {
			t.Fatalf("round %d: allowance %q is neither writer's", round, got)
		}
	}
}

func TestSaveSkillPacksAppliesOnlyAtTheRevisionItWasBuiltFrom(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.SetSkillPack(ctx, "family", []string{"code-review"}); err != nil {
		t.Fatalf("declare family: %v", err)
	}
	if err := st.SetSkillPack(ctx, "ops", []string{"deploy-helper"}); err != nil {
		t.Fatalf("declare ops: %v", err)
	}
	read, err := st.ListSkillPacks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	revision := store.SkillPacksRevision(read)

	// One save declares a column and withdraws another, together.
	if err := st.SaveSkillPacks(ctx, revision, map[string][]string{"family": {"deploy-helper"}}, []string{"ops"}); err != nil {
		t.Fatalf("save at the current revision: %v", err)
	}
	packs, err := st.ListSkillPacks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, declared := packs["ops"]; declared || strings.Join(packs["family"], ",") != "deploy-helper" {
		t.Fatalf("after save: %v", packs)
	}

	// The same revision is now out of date, and a refused save writes nothing.
	err = st.SaveSkillPacks(ctx, revision, map[string][]string{"family": {}}, nil)
	if !errors.Is(err, store.ErrSkillPacksChanged) {
		t.Fatalf("a save from an old revision: %v, want ErrSkillPacksChanged", err)
	}
	packs, err = st.ListSkillPacks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Join(packs["family"], ",") != "deploy-helper" {
		t.Fatalf("a refused save must not write: %v", packs)
	}
}

// The fingerprint tracks the rules, not the order they were read in, and tells
// "no rule" apart from "a rule that allows nothing".
func TestSkillPacksRevision(t *testing.T) {
	a := store.SkillPacksRevision(map[string][]string{"family": {"b", "a"}, "ops": {}})
	b := store.SkillPacksRevision(map[string][]string{"ops": nil, "family": {"a", "b"}})
	if a != b {
		t.Fatalf("order must not matter: %s vs %s", a, b)
	}
	for name, other := range map[string]map[string][]string{
		"an empty rule withdrawn": {"family": {"a", "b"}},
		"a skill added":           {"family": {"a", "b", "c"}, "ops": {}},
		"a skill moved":           {"family": {"a"}, "ops": {"b"}},
		"names that concatenate":  {"family": {"ab"}, "ops": {}},
	} {
		if store.SkillPacksRevision(other) == a {
			t.Fatalf("%s must change the revision", name)
		}
	}
}

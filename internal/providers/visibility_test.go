package providers

import (
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// The predicates here decide who is served a model, who is served a row, and who
// is handed the key that reaches every model in it, so each of the four states
// a model can be in is named: no rule and an open row, no rule and a restricted
// row, a rule the member holds, and a rule they do not.

func rowOf(id string, groups []string) store.ProviderRow {
	return store.ProviderRow{ID: id, ProviderID: "litellm", Name: id,
		BaseURL: "https://gateway.invalid/v1", APIKeyEnv: "LITELLM_API_KEY",
		Groups: groups, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0)}
}

func TestUndeclaredModelFollowsItsRow(t *testing.T) {
	aud := Audiences{}
	for _, tc := range []struct {
		name   string
		rowAud []string
		member []string
		want   bool
	}{
		{"open row, any member", nil, []string{"everyone"}, true},
		{"restricted row, member inside", []string{"family"}, []string{"family"}, true},
		{"restricted row, member outside", []string{"family"}, []string{"contractors"}, false},
	} {
		if got := aud.modelVisible("lpr_x", "m1", tc.rowAud, tc.member); got != tc.want {
			t.Errorf("%s: visible = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDeclaredModelAudienceReplacesTheRows(t *testing.T) {
	aud := Audiences{"lpr_x": {"m1": {"family"}}}
	// The row is Joe's alone; the rule opens m1 to the household. A rule is the
	// model's whole audience, so it widens one model without widening the row.
	if !aud.modelVisible("lpr_x", "m1", []string{"joe"}, []string{"family"}) {
		t.Error("a rule naming family must show the model to a member holding family")
	}
	if aud.modelVisible("lpr_x", "m2", []string{"joe"}, []string{"family"}) {
		t.Error("a model no rule names must keep following the row, which excludes family")
	}
	if aud.modelVisible("lpr_x", "m1", []string{"joe"}, []string{"contractors"}) {
		t.Error("a member holding none of a rule's groups must not see the model")
	}
}

func TestEntitlementNeedsNoGatewayAnswer(t *testing.T) {
	// The key gate must not depend on the menu: a gateway that is down, cooling
	// down, or answering empty has no say about who is entitled to its key.
	aud := Audiences{"lpr_x": {"glm-5.3": {"family"}}}
	if !aud.entitled(rowOf("lpr_x", []string{"joe"}), []string{"family"}) {
		t.Error("a member holding one of the row's rules must be entitled to its key")
	}
	if !aud.entitled(rowOf("lpr_x", nil), []string{"everyone"}) {
		t.Error("an open row still entitles every member: models nobody ruled are theirs")
	}
	if aud.entitled(rowOf("lpr_x", []string{"joe"}), []string{"contractors"}) {
		t.Error("a member named by no rule and excluded by the row must not be entitled")
	}
	if !aud.entitled(rowOf("lpr_y", []string{"joe"}), []string{"joe"}) {
		t.Error("a row with no rules is entitled exactly as its own audience says")
	}
	if aud.entitled(rowOf("lpr_y", []string{"joe"}), []string{"family"}) {
		t.Error("a row with no rules must not widen past its audience")
	}
}

func TestRowWithNothingVisibleIsNotServed(t *testing.T) {
	aud := Audiences{"lpr_x": {"glm-5.3": {"family"}}}
	row := rowOf("lpr_x", []string{"joe"})
	// family is entitled through the rule, and the menu confirms a model they
	// can see: the row is served.
	if !aud.servesRow(row, []string{"family"}, []string{"glm-5.3"}, true) {
		t.Error("a member who sees one model must be served the row")
	}
	// Entitled through the row's audience, but every model the gateway named is
	// ruled to someone else: no row at all, which is the whole of the issue's
	// "a member who sees none of a provider's models gets no row".
	open := rowOf("lpr_x", nil)
	if aud.servesRow(open, []string{"contractors"}, []string{"glm-5.3"}, true) {
		t.Error("a member who sees no model of a row must not be served it")
	}
	// With no answer from the gateway the rules alone decide, so an outage
	// neither withdraws a row from its own audience nor hands one to nobody.
	if !aud.servesRow(open, []string{"contractors"}, nil, false) {
		t.Error("an unanswered menu must not withdraw a row its audience allows")
	}
	if aud.servesRow(rowOf("lpr_x", []string{"joe"}), []string{"contractors"}, nil, false) {
		t.Error("a member named by no rule and excluded by the row must be answered no row, menu or no menu")
	}
}

func TestNoRulesAnywhereServesEveryModel(t *testing.T) {
	// SPEC-0003 REQ "Compatibility": with nothing declared, every model of an
	// open row is served whatever the gateway says, including nothing.
	aud := Audiences{}
	row := rowOf("lpr_x", nil)
	if got := aud.visibleModels(row, []string{"m2", "m1"}, []string{"anyone"}); len(got) != 2 || got[0] != "m2" {
		t.Fatalf("visibleModels must keep the menu's order and change nothing, got %v", got)
	}
	for _, tc := range []struct {
		ids       []string
		menuKnown bool
	}{
		{[]string{"m1"}, true},
		{nil, true},  // a gateway that answered empty
		{nil, false}, // a gateway that has not answered
	} {
		if !aud.servesRow(row, []string{"anyone"}, tc.ids, tc.menuKnown) {
			t.Errorf("menu %v known=%v: an unrestricted row must still be served", tc.ids, tc.menuKnown)
		}
	}
}

func TestAudiencesOfKeepsRowsApart(t *testing.T) {
	// The same model id behind two gateways is two models: a rule for one must
	// not open the other.
	aud := AudiencesOf([]store.ModelAudience{
		{ProviderID: "lpr_a", ModelID: "gpt-5", Groups: []string{"family"}},
		{ProviderID: "lpr_b", ModelID: "gpt-5", Groups: []string{"joe"}},
	})
	if !aud.modelVisible("lpr_a", "gpt-5", nil, []string{"family"}) {
		t.Error("lpr_a/gpt-5 is the household's")
	}
	if aud.modelVisible("lpr_b", "gpt-5", nil, []string{"family"}) {
		t.Error("lpr_b/gpt-5 is not")
	}
	if !aud.hasRules("lpr_a") || aud.hasRules("lpr_c") {
		t.Error("hasRules must report the rows that carry rules")
	}
}

func TestKeyScopeWarningNamesOnlyMixedRows(t *testing.T) {
	// One row carries one key, so the warning is for the rows where hiding a
	// model is not the same as withholding it (SPEC-0003 REQ "Key-scope
	// warning"): a rule that says something other than the row already says.
	aud := AudiencesOf([]store.ModelAudience{
		{ProviderID: "lpr_mixed", ModelID: "private", Groups: []string{"joe"}},
		{ProviderID: "lpr_uniform", ModelID: "a", Groups: []string{"family"}},
		{ProviderID: "lpr_uniform", ModelID: "b", Groups: []string{"family"}},
	})
	if !mixedAudience(rowOf("lpr_mixed", []string{"family"}), aud) {
		t.Error("a model narrower than its row must be named")
	}
	if mixedAudience(rowOf("lpr_uniform", []string{"family"}), aud) {
		t.Error("rules that repeat the row's own audience say nothing new")
	}
	if mixedAudience(rowOf("lpr_plain", []string{"family"}), aud) {
		t.Error("a row with no rules has one audience: its own")
	}
	// An open row with one restricted model is mixed too: everyone else can
	// still reach that model with the row's key.
	if !mixedAudience(rowOf("lpr_mixed", nil), aud) {
		t.Error("an open row with a restricted model must be named")
	}
	// Order and repeats are how the columns were rendered, not a difference.
	restated := AudiencesOf([]store.ModelAudience{
		{ProviderID: "lpr_row", ModelID: "m", Groups: []string{"family", "family", "jon"}},
	})
	if mixedAudience(rowOf("lpr_row", []string{"jon", "family"}), restated) {
		t.Error("the same audience in another order is not a mixed row")
	}
}

package providers

// Model audiences: which models of one row one member sees
// (SPEC-0003 REQ "Model audience", stump.wtf/burrow#13).
//
// A provider row carries one audience and one key, and its menu is derived from
// what that key reaches, so the row alone cannot express "the local models are
// the household's and the rest are mine". A declared model audience says just
// that: the model is visible to members holding one of its groups, and a model
// nobody declared follows its row, exactly as SPEC-0003 always worded it. This
// is the rule in one place, so the listing, the resource snapshot and
// /connect — which hands over the key that reaches every model in the row —
// answer from the same slice and cannot disagree with one another.
//
// The row-level decision is deliberately made without consulting the gateway.
// Whether a member is entitled to a row's key is a question about the rules and
// their groups, and a gateway that is down or cooling down must not change it
// (ADR-0004 keeps /connect from ever waiting on one). Where the menu IS known,
// it decides whether the row is served at all: a member who sees no model of a
// row gets no row, and /connect answers for them exactly as for an unknown id.

import (
	"context"
	"log/slog"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// Audiences is the declared per-model audience table as the filter wants it:
// provider row id → model id → the groups that may see it. A row with no entry
// carries no rule, and neither does a model with none.
type Audiences map[string]map[string][]string

// AudiencesOf lines the store's rules up for lookup. The result is the
// Runtime's own copy: callers own what ListModelAudiences returned.
func AudiencesOf(rules []store.ModelAudience) Audiences {
	aud := Audiences{}
	for _, rule := range rules {
		models, ok := aud[rule.ProviderID]
		if !ok {
			models = map[string][]string{}
			aud[rule.ProviderID] = models
		}
		models[rule.ModelID] = rule.Groups
	}
	return aud
}

// hasRules reports whether anything is declared for one provider row. With
// nothing declared the row behaves exactly as it did before this feature
// existed, which is what makes shipping the table a no-op.
func (a Audiences) hasRules(providerID string) bool {
	return len(a[providerID]) > 0
}

// modelVisible reports whether a member holding memberGroups sees one model of
// one row: its own audience where one is declared, the row's audience where it
// is not (SPEC-0003 REQ "Visibility").
func (a Audiences) modelVisible(providerID, modelID string, rowGroups, memberGroups []string) bool {
	groups, declared := a[providerID][modelID]
	if !declared {
		return allowed(rowGroups, memberGroups)
	}
	return allowed(groups, memberGroups)
}

// entitled reports whether a member holding memberGroups may reach one row's
// key: whether at least one of its models is visible to them, judged from the
// rules and the row's audience alone. A row with no rule follows its row's
// audience; a row with rules is reachable either through that audience (which
// still covers every model no rule names) or through any rule the member holds.
func (a Audiences) entitled(p store.ProviderRow, memberGroups []string) bool {
	if !a.hasRules(p.ID) {
		return allowed(p.Groups, memberGroups)
	}
	if allowed(p.Groups, memberGroups) {
		return true
	}
	for _, groups := range a[p.ID] {
		if allowed(groups, memberGroups) {
			return true
		}
	}
	return false
}

// visibleModels keeps the ids a member sees, in the menu's order.
func (a Audiences) visibleModels(p store.ProviderRow, ids []string, memberGroups []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if a.modelVisible(p.ID, id, p.Groups, memberGroups) {
			out = append(out, id)
		}
	}
	return out
}

// servesRow decides whether a member holding memberGroups is served one row at
// all. Entitlement comes first; then, for a row whose models are governed by
// rules, an answer from the gateway that leaves nothing visible takes the row
// away too — that is the whole of "a member who sees none of a provider's
// models gets no row". A row with no rules is never hidden by an empty or
// unanswered menu, so a gateway outage cannot withdraw a row from an org that
// never restricted anything (SPEC-0003 REQ "Compatibility").
func (a Audiences) servesRow(p store.ProviderRow, memberGroups []string, ids []string, menuKnown bool) bool {
	if !a.entitled(p, memberGroups) {
		return false
	}
	if !a.hasRules(p.ID) || !menuKnown {
		return true
	}
	for _, id := range ids {
		if a.modelVisible(p.ID, id, p.Groups, memberGroups) {
			return true
		}
	}
	return false
}

// visibleToAudiences keeps the rows a member holding memberGroups may see,
// ordered as ListProviders returned them. It is the row-level half of the same
// rule and needs no menu, so the snapshot and the listing agree on which ids a
// member has even before either derives a menu.
func visibleToAudiences(rows []store.ProviderRow, aud Audiences, memberGroups []string) []store.ProviderRow {
	out := make([]store.ProviderRow, 0, len(rows))
	for _, p := range rows {
		if aud.entitled(p, memberGroups) {
			out = append(out, p)
		}
	}
	return out
}

// mixedAudience reports whether one row's models do not all share one audience
// (SPEC-0003 REQ "Key-scope warning"). A model no rule names follows its row,
// so the row is uniform only where every declared rule names exactly the row's
// own groups.
func mixedAudience(p store.ProviderRow, aud Audiences) bool {
	for _, groups := range aud[p.ID] {
		if !sameGroups(groups, p.Groups) {
			return true
		}
	}
	return false
}

// WarnKeyScope logs SPEC-0003's key-scope warning for every row whose models do
// not all share one audience. A row carries one key, and that key reaches every
// model in the row, so a narrower model is hidden from the listing but stays
// reachable through it; a key per group is what would enforce the difference
// (ADR-0009 carries that open decision). Called at boot and whenever an admin
// saves the mapping — the two moments a model's audience changes.
func (r *Runtime) WarnKeyScope(ctx context.Context) {
	rows, err := r.st.ListProviders(ctx)
	if err != nil {
		slog.Error("models: load rows for the key-scope warning", "error", err)
		return
	}
	aud, err := r.audiences(ctx)
	if err != nil {
		slog.Error("models: load model audiences for the key-scope warning", "error", err)
		return
	}
	for _, p := range rows {
		if mixedAudience(p, aud) {
			slog.Warn("model audiences differ inside one row; its single key reaches every model, so a narrower model is hidden but still reachable (SPEC-0003, ADR-0009)",
				"provider", p.ID)
		}
	}
}

package store

// Model audiences: the per-model visibility rules inside one provider row
// (SPEC-0003 REQ "Model audience", stump.wtf/burrow#13). A provider row has one
// audience and one gateway key, and its menu is derived from that key, so the
// row alone cannot say "this model is the household's and that one is mine" —
// which is what an org with a personal model on a shared gateway needs.
//
// A row in model_audiences is one model whose audience differs from its row's.
// A model with no row inherits its row's audience, which is precisely the
// behavior before this feature, so shipping the table changes nothing and an
// org that writes no rule answers exactly as SPEC-0004 describes. The rule
// replaces the row's audience for that model rather than narrowing it: a model
// named in a rule is visible to the members holding one of its groups, and to
// nobody else, so restricting one model never quietly widens another.
//
// Membership itself stays in the identity provider, as it does for skill packs:
// burrow stores which groups a model is visible to and never who belongs to a
// group, so adding someone to a model's audience is adding them to the group
// where they already belong.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrModelAudiencesChanged is returned by SaveModelAudiences when the rules in
// the store are no longer the ones the caller's revision was computed from, so
// a console page rendered before another admin's save changes nothing.
var ErrModelAudiencesChanged = errors.New("store: model audiences changed since they were read")

// ModelAudience is one model's declared audience inside one provider row.
// Groups is the audience itself: members holding any of these OIDC groups. It
// is never empty — an empty audience is a mistake, not a rule, and the write
// refuses it (SPEC-0003 REQ "Group declarations").
type ModelAudience struct {
	ProviderID string
	ModelID    string
	Groups     []string
}

// modelAudiencesKey separates a provider id from a model id inside one
// fingerprint entry, so a pair cannot collide with a longer name.
const modelAudiencesKey = "\x00"

// lockModelAudiences serializes every writer of the rule table and of the rows
// whose timestamps it moves. A save replaces the rules of one provider by
// delete-then-insert and bumps that row's updated_at so clients refetch it;
// taking the provider lock first, in the same order in both places, keeps a
// concurrent provider write from waiting on our table while we wait on its row.
// Readers take ACCESS SHARE and are not blocked.
func lockModelAudiences(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx,
		`LOCK TABLE llm_providers, model_audiences IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("store: lock model audiences: %w", err)
	}
	return nil
}

// ListModelAudiences returns every declared rule, ordered by provider row and
// model id. A model nobody has declared is absent, which is the same statement
// as "this model follows its row".
func (s *Store) ListModelAudiences(ctx context.Context) ([]ModelAudience, error) {
	return listModelAudiences(ctx, s.db)
}

// listModelAudiences reads the rules through the pool or through a transaction.
func listModelAudiences(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]ModelAudience, error) {
	rows, err := q.QueryContext(ctx, `
SELECT provider_id, model_id, model_groups
FROM model_audiences ORDER BY provider_id, model_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list model audiences: %w", err)
	}
	defer rows.Close()
	var out []ModelAudience
	for rows.Next() {
		var (
			providerID, modelID, raw string
		)
		if err := rows.Scan(&providerID, &modelID, &raw); err != nil {
			return nil, fmt.Errorf("store: scan model audiences: %w", err)
		}
		groups, err := decodeGroups(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, ModelAudience{ProviderID: providerID, ModelID: modelID, Groups: groups})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list model audiences: %w", err)
	}
	return out, nil
}

// ModelAudiencesRevision fingerprints a set of rules, independent of slice and
// group order. The console renders it into the matrix form and
// SaveModelAudiences checks it, which is how a save proves it was made from the
// rules still in force.
func ModelAudiencesRevision(rules []ModelAudience) string {
	entries := make([][]string, 0, len(rules))
	for _, rule := range rules {
		groups := append([]string{}, rule.Groups...)
		sort.Strings(groups)
		entries = append(entries, append([]string{rule.ProviderID + modelAudiencesKey + rule.ModelID}, groups...))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i][0] < entries[j][0] })
	// Marshalling [][]string cannot fail, and JSON quoting keeps a name that
	// contains the separator from colliding with two shorter names.
	encoded, _ := json.Marshal(entries)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// SaveModelAudiences applies one console save — the whole rule table — in a
// single transaction, and only if the rules in the store still fingerprint to
// revision. Providers whose rules actually changed have their updated_at moved
// and an audit entry appended, so a member's client refetches a row whose menu
// just narrowed and an incident review can still read who widened what.
//
// A rule whose Groups is empty is refused rather than stored: "visible to
// nobody" written as an empty list is indistinguishable from a mistake, and a
// model nobody may see belongs on the gateway side, not in burrow's rules.
func (s *Store) SaveModelAudiences(ctx context.Context, revision string, rules []ModelAudience, actor string) error {
	for _, rule := range rules {
		if strings.TrimSpace(rule.ProviderID) == "" {
			return fmt.Errorf("store: model audience needs a provider id")
		}
		if strings.TrimSpace(rule.ModelID) == "" {
			return fmt.Errorf("store: model audience needs a model id")
		}
		if hasControlRune(rule.ModelID) {
			return fmt.Errorf("store: model id %q contains a control character", rule.ModelID)
		}
		if len(rule.Groups) == 0 {
			return fmt.Errorf("store: model audience for %s/%s needs at least one group", rule.ProviderID, rule.ModelID)
		}
		for _, group := range rule.Groups {
			if strings.TrimSpace(group) == "" {
				return fmt.Errorf("store: model audience for %s/%s contains an empty group name", rule.ProviderID, rule.ModelID)
			}
			if hasControlRune(group) {
				return fmt.Errorf("store: model audience for %s/%s contains a control character", rule.ProviderID, rule.ModelID)
			}
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin save model audiences: %w", err)
	}
	defer tx.Rollback()
	if err := lockModelAudiences(ctx, tx); err != nil {
		return err
	}
	before, err := listModelAudiences(ctx, tx)
	if err != nil {
		return err
	}
	if ModelAudiencesRevision(before) != revision {
		return ErrModelAudiencesChanged
	}
	after := normalizeModelAudiences(rules)
	for _, providerID := range changedModelProviders(before, after) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM model_audiences WHERE provider_id = $1`, providerID); err != nil {
			return fmt.Errorf("store: clear model audiences for %s: %w", providerID, err)
		}
		created := now()
		for _, rule := range after {
			if rule.ProviderID != providerID {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO model_audiences (provider_id, model_id, model_groups, created_at, updated_at)
VALUES ($1, $2, $3, $4, $4)`,
				rule.ProviderID, rule.ModelID, encodeModelGroups(rule.Groups), created); err != nil {
				return fmt.Errorf("store: save model audience %s/%s: %w", rule.ProviderID, rule.ModelID, err)
			}
		}
		// The timestamp is what makes a member's client refetch the row: the
		// snapshot it polls carries id→updated_at, so a menu that narrowed
		// under an unchanged timestamp would stay cached on the desktop. A row
		// deleted between the read and here is gone; its rules went with it by
		// cascade, and there is nothing left to bump or audit.
		var bumped time.Time
		err := tx.QueryRowContext(ctx,
			`UPDATE llm_providers SET updated_at = $2 WHERE id = $1 RETURNING updated_at`,
			providerID, now()).Scan(&bumped)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("store: bump provider %s for model rules: %w", providerID, err)
		}
		if err := insertAudit(ctx, tx, actor, "model-audiences", providerID,
			modelAudiencesJSON(before, providerID), modelAudiencesJSON(after, providerID)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// normalizeModelAudiences drops duplicates and empty names, orders groups
// inside a rule and rules by row and model, so a save that means the same thing
// as an earlier one stores the same rows and reads back the same fingerprint.
func normalizeModelAudiences(rules []ModelAudience) []ModelAudience {
	byKey := map[string]ModelAudience{}
	for _, rule := range rules {
		providerID := strings.TrimSpace(rule.ProviderID)
		modelID := strings.TrimSpace(rule.ModelID)
		if providerID == "" || modelID == "" {
			continue
		}
		groups := dedupeGroups(rule.Groups)
		if len(groups) == 0 {
			continue
		}
		key := providerID + modelAudiencesKey + modelID
		merged := byKey[key]
		merged.ProviderID, merged.ModelID = providerID, modelID
		merged.Groups = append(merged.Groups, groups...)
		byKey[key] = merged
	}
	out := make([]ModelAudience, 0, len(byKey))
	for _, rule := range byKey {
		rule.Groups = dedupeGroups(rule.Groups)
		out = append(out, rule)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProviderID != out[j].ProviderID {
			return out[i].ProviderID < out[j].ProviderID
		}
		return out[i].ModelID < out[j].ModelID
	})
	return out
}

// dedupeGroups trims and dedupes group names, dropping an empty one, which is
// what the sign-in side of the same comparison does (SPEC-0003 REQ "Groups
// claim"): a rule's name and the claim's name have to be the same spelling.
func dedupeGroups(groups []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range groups {
		if group = strings.TrimSpace(group); group != "" && !seen[group] {
			seen[group] = true
			out = append(out, group)
		}
	}
	sort.Strings(out)
	return out
}

// changedModelProviders lists the rows whose rules differ between two rule
// sets, sorted, so a save touches and audits exactly the rows it changed.
func changedModelProviders(before, after []ModelAudience) []string {
	render := func(rules []ModelAudience) map[string]string {
		byProvider := map[string][]string{}
		for _, rule := range rules {
			byProvider[rule.ProviderID] = append(byProvider[rule.ProviderID], rule.ModelID+modelAudiencesKey+strings.Join(rule.Groups, ","))
		}
		out := map[string]string{}
		for providerID, entries := range byProvider {
			sort.Strings(entries)
			out[providerID] = strings.Join(entries, "|")
		}
		return out
	}
	b, a := render(before), render(after)
	providers := map[string]bool{}
	for providerID, text := range b {
		if a[providerID] != text {
			providers[providerID] = true
		}
	}
	for providerID, text := range a {
		if b[providerID] != text {
			providers[providerID] = true
		}
	}
	out := make([]string, 0, len(providers))
	for providerID := range providers {
		out = append(out, providerID)
	}
	sort.Strings(out)
	return out
}

// modelAudiencesJSON is the audit body for one provider's rules: the models and
// the groups they name, never a key or a member list.
func modelAudiencesJSON(rules []ModelAudience, providerID string) json.RawMessage {
	var out []map[string]any
	for _, rule := range rules {
		if rule.ProviderID != providerID {
			continue
		}
		out = append(out, map[string]any{"modelId": rule.ModelID, "groups": rule.Groups})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(`[]`)
	}
	return raw
}

// encodeModelGroups spells a rule's audience as the JSON array the column
// holds. Unlike a provider row's audience there is no NULL meaning: a rule
// always names groups, and an empty audience is refused before the write.
func encodeModelGroups(groups []string) string {
	raw, err := json.Marshal(groups)
	if err != nil {
		// []string never fails to marshal.
		return "[]"
	}
	return string(raw)
}

// hasControlRune reports whether s carries a character Postgres TEXT cannot
// hold or that no group claim can match.
func hasControlRune(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

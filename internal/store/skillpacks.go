package store

// Skill packs: the group-to-skills mapping behind the Library's audience
// filter (SPEC-0003 REQ "Library audience", stump.wtf/burrow#12). A group that
// has declared a rule gets a row in skill_packs; its allowance is the set of
// rows in skill_pack_skills.
//
// The two tables keep two different states apart. No row at all for a group
// means no rule written: while no group has a row the whole org sees the whole
// catalog (which is what makes shipping this change nothing), but once any
// rule exists a member holding only undeclared groups falls to the fail-closed
// default and sees nothing (SPEC-0003 as amended by ADR-0006,
// stump.wtf/burrow#39). A group with a row and no skills has declared an empty
// allowance: its members see no Library entries. Collapsing the two would
// remove the admin's ability to say "nobody in this group", which is a thing
// an admin means.
//
// Membership itself stays in the identity provider — burrow stores only which
// skills each group may see, never who is in a group.

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
)

// ErrSkillPacksChanged is returned by SaveSkillPacks when the rules in the
// store are no longer the ones the caller's revision was computed from.
var ErrSkillPacksChanged = errors.New("store: skill packs changed since they were read")

// lockSkillPacks serializes every writer of the two pack tables for the rest
// of the transaction. Row locks are not enough here: a group's allowance is
// replaced by delete-then-insert, and at READ COMMITTED a second writer's
// DELETE cannot see the rows the first one has just inserted, so two saves of
// one group either merged into an allowance wider than either wrote or died on
// the primary key. Readers take ACCESS SHARE and are not blocked.
func lockSkillPacks(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `LOCK TABLE skill_packs, skill_pack_skills IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("store: lock skill packs: %w", err)
	}
	return nil
}

// SetSkillPack declares one group's whole allowance in a single transaction,
// so a failed insert cannot leave the group half-mapped. An empty list is a
// rule that allows nothing; to return a group to "no rule written", map it to
// the skills its members should see instead.
func (s *Store) SetSkillPack(ctx context.Context, group string, skills []string) error {
	group = strings.TrimSpace(group)
	if group == "" {
		return fmt.Errorf("store: skill pack needs a group name")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin set skill pack: %w", err)
	}
	defer tx.Rollback()
	if err := lockSkillPacks(ctx, tx); err != nil {
		return err
	}
	if err := setSkillPack(ctx, tx, group, skills); err != nil {
		return err
	}
	return tx.Commit()
}

// setSkillPack replaces one group's allowance inside a transaction that
// already holds lockSkillPacks.
func setSkillPack(ctx context.Context, tx *sql.Tx, group string, skills []string) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO skill_packs (pack_group, created_at) VALUES ($1, $2)
ON CONFLICT (pack_group) DO NOTHING`, group, now()); err != nil {
		return fmt.Errorf("store: declare skill pack %q: %w", group, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM skill_pack_skills WHERE pack_group = $1`, group); err != nil {
		return fmt.Errorf("store: clear skill pack %q: %w", group, err)
	}
	for _, skill := range dedupe(skills) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO skill_pack_skills (pack_group, skill, created_at) VALUES ($1, $2, $3)`,
			group, skill, now()); err != nil {
			return fmt.Errorf("store: map skill %s to group %q: %w", skill, group, err)
		}
	}
	return nil
}

// SaveSkillPacks applies one console save — every declared column and every
// withdrawn one — in a single transaction, and only if the rules in the store
// still fingerprint to revision. A page rendered before another admin's save
// therefore changes nothing and gets ErrSkillPacksChanged, instead of writing
// its older picture of the matrix over the newer one.
func (s *Store) SaveSkillPacks(ctx context.Context, revision string, declare map[string][]string, forget []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin save skill packs: %w", err)
	}
	defer tx.Rollback()
	if err := lockSkillPacks(ctx, tx); err != nil {
		return err
	}
	current, err := listSkillPacks(ctx, tx)
	if err != nil {
		return err
	}
	if SkillPacksRevision(current) != revision {
		return ErrSkillPacksChanged
	}
	for _, group := range forget {
		if group = strings.TrimSpace(group); group == "" {
			return fmt.Errorf("store: skill pack needs a group name")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM skill_packs WHERE pack_group = $1`, group); err != nil {
			return fmt.Errorf("store: forget skill pack %q: %w", group, err)
		}
	}
	groups := make([]string, 0, len(declare))
	for group := range declare {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	for _, group := range groups {
		name := strings.TrimSpace(group)
		if name == "" {
			return fmt.Errorf("store: skill pack needs a group name")
		}
		if err := setSkillPack(ctx, tx, name, declare[group]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SkillPacksRevision fingerprints a set of rules, independent of map and slice
// order. The console renders it into the matrix form and SaveSkillPacks checks
// it, which is how a save proves it was made from the rules still in force.
func SkillPacksRevision(packs map[string][]string) string {
	groups := make([]string, 0, len(packs))
	for group := range packs {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	canonical := make([][]string, 0, len(groups))
	for _, group := range groups {
		skills := append([]string{}, packs[group]...)
		sort.Strings(skills)
		canonical = append(canonical, append([]string{group}, skills...))
	}
	// Marshalling [][]string cannot fail, and JSON quoting keeps a name that
	// contains a separator from colliding with two shorter names.
	encoded, _ := json.Marshal(canonical)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// ForgetSkillPack withdraws one group's rule entirely, returning it to "no rule
// written" — which is not the same as SetSkillPack with an empty list, because
// that declares an allowance of nothing. The cascade takes the skill rows with
// it. Forgetting a group nobody declared is not an error.
func (s *Store) ForgetSkillPack(ctx context.Context, group string) error {
	group = strings.TrimSpace(group)
	if group == "" {
		return fmt.Errorf("store: skill pack needs a group name")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM skill_packs WHERE pack_group = $1`, group); err != nil {
		return fmt.Errorf("store: forget skill pack %q: %w", group, err)
	}
	return nil
}

// ListSkillPacks returns every group that has declared a rule, mapped to its
// allowance, ready for skills.Visible. A declared group is a key even when its
// allowance is empty — that emptiness is the rule — while a group nobody has
// declared is absent.
func (s *Store) ListSkillPacks(ctx context.Context) (map[string][]string, error) {
	return listSkillPacks(ctx, s.db)
}

// listSkillPacks reads the rules through the pool or through a transaction.
func listSkillPacks(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (map[string][]string, error) {
	rows, err := q.QueryContext(ctx, `
SELECT p.pack_group, k.skill
FROM skill_packs p
LEFT JOIN skill_pack_skills k ON k.pack_group = p.pack_group
ORDER BY p.pack_group, k.skill`)
	if err != nil {
		return nil, fmt.Errorf("store: list skill packs: %w", err)
	}
	defer rows.Close()
	packs := map[string][]string{}
	for rows.Next() {
		var group string
		var skill *string
		if err := rows.Scan(&group, &skill); err != nil {
			return nil, fmt.Errorf("store: scan skill pack: %w", err)
		}
		if _, seen := packs[group]; !seen {
			packs[group] = []string{}
		}
		if skill != nil && *skill != "" {
			packs[group] = append(packs[group], *skill)
		}
	}
	return packs, rows.Err()
}

// MemberGroups returns every OIDC group recorded at any member's sign-in,
// sorted and deduplicated. The console lists these as its columns: the
// audience an admin can assign to is the set of groups burrow has actually
// seen, and a group nobody belongs to yet appears once a rule names it.
func (s *Store) MemberGroups(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT oidc_groups FROM users`)
	if err != nil {
		return nil, fmt.Errorf("store: member groups: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("store: scan member groups: %w", err)
		}
		groups, err := decodeGroups(raw)
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			if g = strings.TrimSpace(g); g != "" {
				seen[g] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out, nil
}

// dedupe trims each name, drops empties, and keeps first-seen order so a
// console save reads back in the order the admin checked them.
func dedupe(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

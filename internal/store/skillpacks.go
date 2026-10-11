package store

// Skill packs: the group-to-skills mapping behind the Library's audience
// filter (SPEC-0003 REQ "Library audience", stump.wtf/burrow#12). A group that
// has declared a rule gets a row in skill_packs; its allowance is the set of
// rows in skill_pack_skills.
//
// The two tables keep two different states apart. No row at all for a group
// means no rule written, so a member holding only such groups keeps the whole
// catalog — that is the org-wide default, and it is what makes shipping this
// change nothing. A group with a row and no skills has declared an empty
// allowance: its members see no Library entries. Collapsing the two would
// remove the admin's ability to say "nobody in this group", which is a thing
// an admin means.
//
// Membership itself stays in the identity provider — burrow stores only which
// skills each group may see, never who is in a group.

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

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
	return tx.Commit()
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
	rows, err := s.db.QueryContext(ctx, `
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

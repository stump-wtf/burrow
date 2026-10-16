package skills

// The Library's audience filter: which of the catalog's entries one member may
// see, given their OIDC groups and the group-to-skill mapping an admin edits
// in the console. This is the whole rule, in one place, so every surface that
// serves skills (the control-plane JSON, the MCP tools, the discovery index)
// answers from the same slice and cannot disagree with the others.
//
// Membership is not stored here — it belongs to the identity provider. burrow
// records each member's groups at sign-in and holds only the mapping from a
// group to the skills that group may see.
//
// Governing: SPEC-0003 REQ "Library audience", REQ "Compatibility"
// (stump.wtf/burrow#12, amended by ADR-0006 for the fail-closed default,
// stump.wtf/burrow#39), ADR-0003 (option 2: a console-edited mapping, where
// there is no key whose scope could drift from the menu).

import "strings"

// Packs maps an OIDC group to the skill names that group may see. A group that
// has declared a rule is a key; its list may be empty, which means the rule
// allows nothing. A group that is absent has no rule at all.
type Packs map[string][]string

// Visible returns the entries one member sees, keeping the catalog's order.
//
// The union of the member's declared groups is their allowance. Once any
// rule exists, a member holding no declared group sees nothing: writing a
// rule for any group is a statement that the Library is restricted, and an
// undeclared group — one not yet seen, one the identity provider renamed or
// recased, one a sign-in stopped carrying — must not be the widest door in
// the system (ADR-0006, stump.wtf/burrow#39). While no rule exists anywhere
// the org behaves exactly as it did before this existed, which is what makes
// shipping the change a no-op. A declared group contributes its own list, so
// a member whose only group declares an empty allowance sees nothing —
// "these members get nothing" is a thing an admin means, and it is not the
// same statement as "nobody has written a rule about this group".
func Visible(list []Skill, groups []string, packs Packs) []Skill {
	if len(packs) == 0 {
		return list
	}
	var allowed map[string]bool
	for _, group := range groups {
		names, declared := packs[group]
		if !declared {
			continue
		}
		if allowed == nil {
			allowed = make(map[string]bool, len(names))
		}
		for _, name := range names {
			allowed[name] = true
		}
	}
	if allowed == nil {
		return nil
	}
	out := make([]Skill, 0, len(list))
	for _, skill := range list {
		if allowed[skill.Name] {
			out = append(out, skill)
		}
	}
	return out
}

// HoldsDeclared reports whether any group the member holds carries a rule.
// It is false wherever the fail-closed default applies to them: a group
// nobody has declared, a sign-in that carried no groups at all, or a rename
// or recase that left the declared name behind. Callers log or surface that
// state so an admin sees the drift instead of learning about it from a member
// whose Library went quiet (stump.wtf/burrow#39).
func HoldsDeclared(groups []string, packs Packs) bool {
	for _, group := range groups {
		if _, declared := packs[group]; declared {
			return true
		}
	}
	return false
}

// Search returns the entries whose name, title, or description contain every
// whitespace-separated term of query, case-insensitively. An empty query
// returns the list as given. Callers pass a list they have already filtered by
// audience, so a search can only ever answer from what the member may see.
func Search(list []Skill, query string) []Skill {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return list
	}
	var out []Skill
	for _, s := range list {
		haystack := strings.ToLower(s.Name + " " + s.Title + " " + s.Description)
		matched := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matched = false
				break
			}
		}
		if matched {
			out = append(out, s)
		}
	}
	return out
}

// Find returns the entry with exactly name.
func Find(list []Skill, name string) (Skill, bool) {
	for _, s := range list {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

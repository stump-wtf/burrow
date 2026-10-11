package httpapi

// The skill-pack matrix: the console's editor for the Library's audience
// rules (SPEC-0003 REQ "Library audience", stump.wtf/burrow#12). Skills down
// the rows, groups across the columns, one checkbox per cell.
//
// Membership is deliberately absent. The columns are group NAMES burrow has
// seen at sign-in; who belongs to each is the identity provider's business, so
// adding someone to a pack is adding them to the group in Pocket ID. That
// boundary is what keeps this page small: burrow stores one thing, a group's
// skill list, and never a member list.
//
// Saving declares a rule for every column whose switch is on, so the state has
// two distinct shapes: a group nobody has switched on has no rule and its
// members keep the whole catalog (the org-wide default), while a group switched
// on with nothing checked has a rule that allows nothing. Those are different
// statements and the store keeps them apart, which is why the switch is on the
// page at all — without it, saving the matrix would write a rule for every
// group burrow has ever seen, and a group nobody had configured yet would
// suddenly see nothing.
//
// Unlike a provider row, saving here changes no key's scope: the Library is
// served by burrow itself, so the filter takes effect on the next request
// rather than at the member's next sign-in. Only a change of GROUP membership
// waits for a sign-in, because that is what the ID token carries.
//
// Governing: SPEC-0003 REQ "Library audience", SPEC-0004 REQ "Admin console"
// (the same gate), ADR-0003 (option 2 for skills: no key to keep in step).

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// skillMatrix is the matrix page's data: one column per group, one row per
// catalog entry. Declared parallels Groups and marks the columns that carry a
// rule today; Cells parallels both.
type skillMatrix struct {
	Groups   []string
	Declared []bool
	Rows     []skillMatrixRow
	// NoRules renders the "nothing configured yet" note, so an admin can tell
	// an unconfigured Library apart from one whose groups are all switched on
	// and empty.
	NoRules bool
	// UnnamedGroups lists groups whose rule no member has signed in under yet
	// — a misspelled group name is otherwise invisible on this page.
	UntaggedGroups []string
}

type skillMatrixRow struct {
	Name  string
	Title string
	Cells []bool
}

// skillPackFormCount names the hidden field carrying how many group columns the
// rendered form covers. The save rewrites exactly those, so an older page left
// open cannot silently overwrite a group added since it was rendered.
const skillPackFormCount = "groupCount"

// handleConsoleSkills answers GET /admin/skills.
func (s *Server) handleConsoleSkills(w http.ResponseWriter, r *http.Request) {
	page := consolePage{Title: "Skill packs"}
	matrix, errMsg := s.skillMatrix(r)
	if errMsg != "" {
		s.renderConsoleError(w, http.StatusInternalServerError, errMsg)
		return
	}
	page.Matrix = matrix
	switch r.URL.Query().Get("saved") {
	case "1":
		page.Flash = "Saved. It takes effect on the next request — the Library is served here, so no key is involved. A member's GROUPS still come from their last sign-in, so moving someone between packs takes a sign-out and back in."
	}
	renderConsole(w, http.StatusOK, page)
}

// handleConsoleSkillsSave answers POST /admin/skills: for each rendered column,
// declare the group's whole allowance, or withdraw its rule if the column's
// switch was turned off. A column left switched off that never had a rule is
// not written at all.
func (s *Server) handleConsoleSkillsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderConsoleError(w, http.StatusBadRequest, "unreadable form")
		return
	}
	form := r.PostForm
	count, err := strconv.Atoi(form.Get(skillPackFormCount))
	if err != nil || count < 0 {
		s.renderConsoleError(w, http.StatusBadRequest, "this form is out of date — reload the page")
		return
	}
	var rowSkills []string
	for i := 0; ; i++ {
		name := form.Get(fmt.Sprintf("skill.%d", i))
		if name == "" {
			break
		}
		rowSkills = append(rowSkills, name)
	}
	packs, err := s.store.ListSkillPacks(r.Context())
	if err != nil {
		s.renderConsoleError(w, http.StatusInternalServerError, "could not load the skill packs")
		return
	}
	for column := 0; column < count; column++ {
		group := strings.TrimSpace(form.Get(fmt.Sprintf("group.%d", column)))
		if group == "" {
			s.renderConsoleError(w, http.StatusBadRequest, "a group column arrived without a name — reload the page")
			return
		}
		if !form.Has(fmt.Sprintf("declare.%d", column)) {
			if _, wasDeclared := packs[group]; wasDeclared {
				if err := s.store.ForgetSkillPack(r.Context(), group); err != nil {
					s.renderConsoleError(w, http.StatusInternalServerError, "could not clear the rule for "+group+": "+err.Error())
					return
				}
			}
			continue
		}
		var allowed []string
		for row, skill := range rowSkills {
			if form.Has(fmt.Sprintf("cell.%d.%d", column, row)) {
				allowed = append(allowed, skill)
			}
		}
		if err := s.store.SetSkillPack(r.Context(), group, allowed); err != nil {
			s.renderConsoleError(w, http.StatusInternalServerError, "could not save the rule for "+group+": "+err.Error())
			return
		}
	}
	http.Redirect(w, r, "/admin/skills?saved=1", http.StatusSeeOther)
}

// skillMatrix reads the catalog, the declared rules, and the group names burrow
// has seen, and lines them up. The second return is a user-facing error.
func (s *Server) skillMatrix(r *http.Request) (*skillMatrix, string) {
	catalog, err := s.skills.All(r.Context())
	if err != nil {
		return nil, "could not load the Library"
	}
	packs, err := s.store.ListSkillPacks(r.Context())
	if err != nil {
		return nil, "could not load the skill packs"
	}
	memberGroups, err := s.store.MemberGroups(r.Context())
	if err != nil {
		return nil, "could not load the member groups"
	}
	groups := union(memberGroups, mapKeys(packs))

	var untagged []string
	var declared []bool
	for _, group := range groups {
		_, isDeclared := packs[group]
		declared = append(declared, isDeclared)
		if isDeclared && !contains(memberGroups, group) {
			untagged = append(untagged, group)
		}
	}
	matrix := &skillMatrix{
		Groups:         groups,
		Declared:       declared,
		NoRules:        len(packs) == 0,
		UntaggedGroups: untagged,
	}
	for _, skill := range catalog {
		row := skillMatrixRow{Name: skill.Name, Title: skill.Title, Cells: make([]bool, len(groups))}
		for column, group := range groups {
			row.Cells[column] = contains(packs[group], skill.Name)
		}
		matrix.Rows = append(matrix.Rows, row)
	}
	return matrix, ""
}

// union merges two name lists, sorted and deduplicated.
func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range append(append([]string{}, a...), b...) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

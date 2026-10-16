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
// two distinct shapes: a group switched on with nothing checked has a rule that
// allows nothing, while a column switched off carries no rule — which, since
// ADR-0006, means its members see no skills as soon as any rule exists, the
// fail-closed default that replaced the old "keeps the whole catalog" (the
// switch still matters: with no rules written anywhere the Library stays
// entirely open, and switching a group off after declaring an empty allowance
// returns it to "no rule written", which is not the same statement). Without
// the switch, saving the matrix would write a rule for every group burrow has
// ever seen, and a group nobody had configured yet would suddenly see nothing.
//
// Unlike a provider row, saving here changes no key's scope: the Library is
// served by burrow itself, so the filter takes effect on the next request
// rather than at the member's next sign-in. Only a change of GROUP membership
// waits for a sign-in, because that is what the ID token carries.
//
// Governing: SPEC-0003 REQ "Library audience", SPEC-0004 REQ "Admin console"
// (the same gate), ADR-0003 (option 2 for skills: no key to keep in step).

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
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
	// UndeclaredGroups lists groups members have signed in under that no rule
	// names. While any rule exists these members sit on the fail-closed
	// default and see no skills (ADR-0006), so the page says so instead of
	// leaving the drift invisible (stump.wtf/burrow#39).
	UndeclaredGroups []string
	// Revision fingerprints the rules this page was rendered from; the form
	// posts it back so a save can be refused once they have moved on.
	Revision string
}

type skillMatrixRow struct {
	Name  string
	Title string
	Cells []bool
}

// skillPackFormCount names the hidden field carrying how many group columns the
// rendered form covers. The save rewrites exactly those, so an older page left
// open cannot silently overwrite a group added since it was rendered.
//
// skillPackFormRevision names the hidden field carrying the fingerprint of the
// rules the page was rendered from. A browser posts an untouched column exactly
// as it was drawn, so without it a page left open would put back whatever
// another admin had changed in the meantime — and read a rule declared since as
// a column "switched off", withdrawing it.
//
// skillPackFormNewGroup carries an optional group name typed into the matrix.
// Under the fail-closed default a group nobody has declared is a group whose
// members see nothing, so declaring one by name — before any of its members has
// ever signed in — is how an admin brings a new IdP group into the matrix
// (stump.wtf/burrow#39).
const (
	skillPackFormCount    = "groupCount"
	skillPackFormRevision = "revision"
	skillPackFormNewGroup = "newGroup"
)

// skillPacksStale is what an admin reads when the save is refused.
const skillPacksStale = "the skill packs changed after this page was loaded, so nothing was saved — reload the page and make the change again"

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
	var columnGroups []string
	rendered := map[string]bool{}
	for i := 0; ; i++ {
		name := form.Get(fmt.Sprintf("skill.%d", i))
		if name == "" {
			break
		}
		rowSkills = append(rowSkills, name)
		rendered[name] = true
	}
	packs, err := s.store.ListSkillPacks(r.Context())
	if err != nil {
		slog.Error("skills: load packs for save", "error", err)
		s.renderConsoleError(w, http.StatusInternalServerError, "could not load the skill packs")
		return
	}
	revision := form.Get(skillPackFormRevision)
	if store.SkillPacksRevision(packs) != revision {
		slog.Warn("skills: stale pack save refused", "actor", adminActor(r))
		s.renderConsoleError(w, http.StatusConflict, skillPacksStale)
		return
	}
	declare := map[string][]string{}
	var forget []string
	for column := 0; column < count; column++ {
		group := strings.TrimSpace(form.Get(fmt.Sprintf("group.%d", column)))
		if group == "" {
			s.renderConsoleError(w, http.StatusBadRequest, "a group column arrived without a name — reload the page")
			return
		}
		columnGroups = append(columnGroups, group)
		if !form.Has(fmt.Sprintf("declare.%d", column)) {
			if _, wasDeclared := packs[group]; wasDeclared {
				forget = append(forget, group)
			}
			continue
		}
		var allowed []string
		for row, skill := range rowSkills {
			if form.Has(fmt.Sprintf("cell.%d.%d", column, row)) {
				allowed = append(allowed, skill)
			}
		}
		// A skill the page had no row for was not unchecked by anyone: it
		// dropped out of the catalog (a broken SKILL.md is skipped by the
		// loader) and the admin could not see it. Keep it in the rule, so
		// saving an unrelated change does not quietly strip it from every
		// group — it would stay stripped after the file was fixed.
		for _, skill := range packs[group] {
			if !rendered[skill] {
				allowed = append(allowed, skill)
			}
		}
		declare[group] = allowed
	}
	// A name typed into the page declares that group with an empty allowance,
	// so a new IdP group can be brought under the matrix before any of its
	// members ever signs in — the fail-closed default would otherwise leave
	// them on an invisible rule of nothing (stump.wtf/burrow#39). A name the
	// page already renders as a column is that column's business: its cells
	// and switch say what was meant, and re-declaring it empty from the text
	// box would erase the rule on the page.
	if group := strings.TrimSpace(form.Get(skillPackFormNewGroup)); group != "" && !contains(columnGroups, group) {
		if hasControlRune(group) {
			s.renderConsoleError(w, http.StatusBadRequest, "a group name cannot contain control characters")
			return
		}
		declare[group] = nil
		slog.Info("skills: group declared by name", "actor", adminActor(r), "group", group)
	}
	if err := s.store.SaveSkillPacks(r.Context(), revision, declare, forget); err != nil {
		if errors.Is(err, store.ErrSkillPacksChanged) {
			slog.Warn("skills: stale pack save refused", "actor", adminActor(r))
			s.renderConsoleError(w, http.StatusConflict, skillPacksStale)
			return
		}
		slog.Error("skills: save packs", "actor", adminActor(r), "error", err)
		s.renderConsoleError(w, http.StatusInternalServerError, "could not save the skill packs")
		return
	}
	for _, group := range forget {
		slog.Info("skills: pack withdrawn", "actor", adminActor(r), "group", group)
	}
	for _, group := range sortedKeys(declare) {
		slog.Info("skills: pack saved", "actor", adminActor(r), "group", group, "skills", len(declare[group]))
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
	var undeclared []string
	var declared []bool
	for _, group := range groups {
		_, isDeclared := packs[group]
		declared = append(declared, isDeclared)
		if isDeclared && !contains(memberGroups, group) {
			untagged = append(untagged, group)
		}
		// The other half of the drift picture: a group members sign in under
		// that no rule names sits on the fail-closed default, so the page
		// names it instead of leaving its members quietly empty
		// (stump.wtf/burrow#39, ADR-0006).
		if !isDeclared && len(packs) > 0 && contains(memberGroups, group) {
			undeclared = append(undeclared, group)
		}
	}
	matrix := &skillMatrix{
		Groups:           groups,
		Declared:         declared,
		NoRules:          len(packs) == 0,
		UntaggedGroups:   untagged,
		UndeclaredGroups: undeclared,
		Revision:         store.SkillPacksRevision(packs),
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

func sortedKeys[V any](m map[string]V) []string {
	out := mapKeys(m)
	sort.Strings(out)
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

// hasControlRune reports whether s carries a control character. Postgres TEXT
// cannot hold one, and the group is compared exactly against the claim, so a
// name carrying one could only ever name an audience no member holds — the
// same check the provider surface makes (SPEC-0004).
func hasControlRune(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

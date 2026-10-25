package httpapi

// The models matrix: the console's editor for per-model audiences inside a
// provider row (SPEC-0003 REQ "Model audience", stump.wtf/burrow#13). Models
// down the rows, grouped by the provider row they were derived from, groups
// across the columns, one checkbox per cell.
//
// A cell checked is a rule written; a model with no cell checked carries no
// rule and follows its row's audience, which is what an org that never
// restricted anything looks like. Checking a cell REPLACES the row's audience
// for that model rather than narrowing it, so a personal row can open one
// model to the household without opening the rest — and opening a model to a
// group the row itself excludes is deliberate, not an oversight.
//
// What a checkbox cannot do is hand out a wider key than the row's: the menu is
// filtered here, but one row has one gateway key, so a member who is served the
// row can reach every model it lists with that key (SPEC-0003 REQ "Key-scope
// warning"). The page says so, and ADR-0009 says what closing it takes.
//
// Governing: SPEC-0003 REQ "Model audience", SPEC-0004 REQ "Admin console" (the
// same gate), ADR-0009.

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// modelMatrix is the matrix page's data: one column per group, one row per
// model of every provider row. Cells parallels Groups.
type modelMatrix struct {
	Groups []string
	Rows   []modelMatrixRow
	// NoRules renders the "nothing configured yet" note, so an admin can tell
	// an unconfigured org apart from one whose every model follows its row.
	NoRules bool
	// NamedGroups lists groups a rule names that no member has signed in
	// under — a misspelled group is otherwise invisible on this page, and its
	// members would see only what their row's audience already allows.
	NamedGroups []string
	// MenusPending names the provider rows whose menu burrow has not derived
	// yet, so the page can say a model list is incomplete instead of passing a
	// short one off as the whole truth.
	MenusPending []string
	// Revision fingerprints the rules this page was rendered from; the form
	// posts it back so a save can be refused once they have moved on.
	Revision string
}

type modelMatrixRow struct {
	ProviderID   string
	ProviderName string
	ModelID      string
	Cells        []bool
	// Declared marks a model that carries a rule today, as opposed to one that
	// follows its row.
	Declared bool
}

// The matrix's hidden fields: how many group columns the rendered form covers,
// which model each row of it is, and the fingerprint of the rules the page was
// drawn from. A save rewrites exactly the cells the page displayed, so a page
// left open across another admin's save either changes nothing or says so.
const (
	modelFormGroupCount = "groupCount"
	modelFormRevision   = "revision"
	modelFormRowPrefix  = "model"
)

// modelsStale is what an admin reads when the save is refused.
const modelsStale = "the model audiences changed after this page was loaded, so nothing was saved — reload the page and make the change again"

// handleConsoleModels answers GET /admin/models.
func (s *Server) handleConsoleModels(w http.ResponseWriter, r *http.Request) {
	page := consolePage{Title: "Model access"}
	matrix, errMsg := s.modelMatrix(r)
	if errMsg != "" {
		s.renderConsoleError(w, http.StatusInternalServerError, errMsg)
		return
	}
	page.Models = matrix
	switch r.URL.Query().Get("saved") {
	case "1":
		page.Flash = "Saved. It takes effect on the next request, and every row whose rules moved had its timestamp advanced so a member's client refetches it. A member's GROUPS still come from their last sign-in, so moving someone between audiences takes a sign-out and back in."
	}
	renderConsole(w, http.StatusOK, page)
}

// handleConsoleModelsSave answers POST /admin/models: every model the page
// rendered, with the groups checked for it. A model with nothing checked is left
// with no rule, which means it follows its row.
func (s *Server) handleConsoleModelsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderConsoleError(w, http.StatusBadRequest, "unreadable form")
		return
	}
	known, err := s.store.ListProviders(r.Context())
	if err != nil {
		slog.Error("models: load providers for save", "error", err)
		s.renderConsoleError(w, http.StatusInternalServerError, "could not load the providers")
		return
	}
	ids := map[string]bool{}
	for _, p := range known {
		ids[p.ID] = true
	}
	rules, errMsg := modelRulesFromForm(r.PostForm, ids)
	if errMsg != "" {
		s.renderConsoleError(w, http.StatusBadRequest, errMsg)
		return
	}
	current, err := s.store.ListModelAudiences(r.Context())
	if err != nil {
		slog.Error("models: load audiences for save", "error", err)
		s.renderConsoleError(w, http.StatusInternalServerError, "could not load the model audiences")
		return
	}
	revision := r.PostForm.Get(modelFormRevision)
	if store.ModelAudiencesRevision(current) != revision {
		slog.Warn("models: stale audience save refused", "actor", adminActor(r))
		s.renderConsoleError(w, http.StatusConflict, modelsStale)
		return
	}
	if err := s.store.SaveModelAudiences(r.Context(), revision, rules, adminActor(r)); err != nil {
		if errors.Is(err, store.ErrModelAudiencesChanged) {
			slog.Warn("models: stale audience save refused", "actor", adminActor(r))
			s.renderConsoleError(w, http.StatusConflict, modelsStale)
			return
		}
		slog.Error("models: save audiences", "actor", adminActor(r), "error", err)
		s.renderConsoleError(w, http.StatusInternalServerError, "could not save the model audiences")
		return
	}
	slog.Info("models: audiences saved", "actor", adminActor(r), "rules", len(rules))
	// The admin who just saved is the one who should hear that a row's key now
	// reaches models its listing hides (SPEC-0003 REQ "Key-scope warning").
	if s.providers != nil {
		s.providers.WarnKeyScope(r.Context())
	}
	http.Redirect(w, r, "/admin/models?saved=1", http.StatusSeeOther)
}

// modelRulesFromForm reads the checked cells into rules, one per model that any
// column checked. The provider row and model of each rendered line arrive as
// hidden fields, so a save names exactly what the admin saw. A provider id the
// store does not have is refused rather than stored: the row was deleted while
// the page was open, and rules for a row that is gone would only resurface if
// someone later reused the id.
func modelRulesFromForm(form url.Values, known map[string]bool) ([]store.ModelAudience, string) {
	count, err := strconv.Atoi(form.Get(modelFormGroupCount))
	if err != nil || count < 0 {
		return nil, "this form is out of date — reload the page"
	}
	var groups []string
	for column := 0; column < count; column++ {
		group := strings.TrimSpace(form.Get(fmt.Sprintf("group.%d", column)))
		if group == "" {
			return nil, "a group column arrived without a name — reload the page"
		}
		if hasControlRune(group) {
			return nil, "a group name cannot contain control characters"
		}
		groups = append(groups, group)
	}
	var rules []store.ModelAudience
	for row := 0; ; row++ {
		providerID := form.Get(fmt.Sprintf("%s.%d.provider", modelFormRowPrefix, row))
		modelID := form.Get(fmt.Sprintf("%s.%d.id", modelFormRowPrefix, row))
		if providerID == "" || modelID == "" {
			break
		}
		if !known[providerID] {
			return nil, "a provider row on this page no longer exists — reload the page"
		}
		if strings.TrimSpace(modelID) == "" || hasControlRune(modelID) {
			return nil, "a model on this page has a name burrow never derives — reload the page"
		}
		var allowed []string
		for column, group := range groups {
			if form.Has(fmt.Sprintf("cell.%d.%d", column, row)) {
				allowed = append(allowed, group)
			}
		}
		if len(allowed) == 0 {
			continue // nothing checked: the model follows its row
		}
		rules = append(rules, store.ModelAudience{ProviderID: providerID, ModelID: modelID, Groups: allowed})
	}
	return rules, ""
}

// modelMatrix reads the provider rows, their derived menus, the declared rules
// and the group names burrow has seen, and lines them up. The second return is
// a user-facing error.
func (s *Server) modelMatrix(r *http.Request) (*modelMatrix, string) {
	providerRows, err := s.store.ListProviders(r.Context())
	if err != nil {
		return nil, "could not load the providers"
	}
	rules, err := s.store.ListModelAudiences(r.Context())
	if err != nil {
		return nil, "could not load the model audiences"
	}
	memberGroups, err := s.store.MemberGroups(r.Context())
	if err != nil {
		return nil, "could not load the member groups"
	}
	var named []string
	byModel := map[string][]string{}
	for _, rule := range rules {
		named = append(named, rule.Groups...)
		byModel[audienceKey(rule.ProviderID, rule.ModelID)] = rule.Groups
	}
	matrix := &modelMatrix{
		Groups:   union(memberGroups, named),
		NoRules:  len(rules) == 0,
		Revision: store.ModelAudiencesRevision(rules),
	}
	for _, group := range union(nil, named) {
		if !contains(memberGroups, group) {
			matrix.NamedGroups = append(matrix.NamedGroups, group)
		}
	}
	for _, p := range providerRows {
		ids, _ := s.providers.MenuFor(r.Context(), p)
		if !s.providers.MenuKnown(p.ID) {
			matrix.MenusPending = append(matrix.MenusPending, p.Name)
		}
		// A model that carries a rule is listed even when the gateway no longer
		// answers for it: its rule is real, and a page that could not see it
		// would drop that rule on save.
		models := append([]string{}, ids...)
		for _, rule := range rules {
			if rule.ProviderID == p.ID && !contains(models, rule.ModelID) {
				models = append(models, rule.ModelID)
			}
		}
		sort.Strings(models)
		for _, modelID := range models {
			cells := make([]bool, len(matrix.Groups))
			key := audienceKey(p.ID, modelID)
			for column, group := range matrix.Groups {
				cells[column] = contains(byModel[key], group)
			}
			matrix.Rows = append(matrix.Rows, modelMatrixRow{
				ProviderID:   p.ID,
				ProviderName: p.Name,
				ModelID:      modelID,
				Cells:        cells,
				Declared:     byModel[key] != nil,
			})
		}
	}
	return matrix, ""
}

// audienceKey is one model of one row, as the matrix indexes its rules. The
// separator keeps a provider id ending in a model's first characters from
// reading as another model of another row.
func audienceKey(providerID, modelID string) string {
	return providerID + "\x00" + modelID
}

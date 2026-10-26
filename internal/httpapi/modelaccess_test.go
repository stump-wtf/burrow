package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/httpapi"
	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// Governing: SPEC-0003 REQ "Model audience" (stump.wtf/burrow#13) — inside one
// provider row a model's audience is its own where one is declared and the
// row's where it is not; a member who sees none of a row's models is served no
// row, and /connect answers for them as for an unknown id, because the row's
// one key reaches every model behind it.
//
// The fixture is the one SPEC-0003 was written against: lpr_local is the
// household's row, lpr_owner is Joe's alone, and lpr_mixed is open to every
// member. A rule on lpr_mixed/narrow is the motivating case — a personal model
// on a shared gateway — and rules over both of lpr_local's models are the case
// where a whole row moves out from under a member who could see it before.

func saveModelRules(t *testing.T, e *testEnv, rules ...store.ModelAudience) {
	t.Helper()
	ctx := context.Background()
	current, err := e.st.ListModelAudiences(ctx)
	if err != nil {
		t.Fatalf("list model audiences: %v", err)
	}
	if err := e.st.SaveModelAudiences(ctx, store.ModelAudiencesRevision(current), rules, "test"); err != nil {
		t.Fatalf("save model audiences: %v", err)
	}
}

func TestModelRuleNarrowsOneModelOfAnOpenRow(t *testing.T) {
	e := newAccessTestEnv(t)
	saveModelRules(t, e, store.ModelAudience{ProviderID: "lpr_mixed", ModelID: "narrow", Groups: []string{"owner"}})

	// A member of the open row's other audiences keeps the row and loses only
	// the model the rule moved.
	signInAs(t, e, "family", "friends")
	want := []string{"lpr_family:glm", "lpr_family:gpt-oss", "lpr_local:qwen-27b", "lpr_local:qwen-flash", "lpr_mixed:open"}
	if got := visibleModels(t, e); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("family's menu = %v, want %v", got, want)
	}

	// The member the rule names sees it, and everything else the row opens.
	signInAs(t, e, "owner")
	want = []string{"lpr_mixed:narrow", "lpr_mixed:open", "lpr_owner:gpt-5"}
	if got := visibleModels(t, e); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("owner's menu = %v, want %v", got, want)
	}
}

// The rule must reach the key too: OpenWork calls the gateway directly with
// whatever /connect hands over, so a row whose models are all someone else's
// must answer like a row that does not exist.
func TestConnectWithholdsTheKeyWhenNoModelIsVisible(t *testing.T) {
	e := newAccessTestEnv(t)
	saveModelRules(t, e,
		store.ModelAudience{ProviderID: "lpr_mixed", ModelID: "open", Groups: []string{"owner"}},
		store.ModelAudience{ProviderID: "lpr_mixed", ModelID: "narrow", Groups: []string{"owner"}})

	// lpr_mixed names no groups of its own, so family reaches the row through
	// its audience — and the rules leave them nothing to see.
	signInAs(t, e, "family", "friends")
	if got := visibleModels(t, e); strings.Contains(strings.Join(got, ","), "lpr_mixed") {
		t.Fatalf("a row with nothing visible must not be served at all: %v", got)
	}
	resp := e.getJSON(t, "/api/den/v1/llm-providers/lpr_mixed/connect", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("connect status %d, want 404 like an unknown id", resp.StatusCode)
	}

	var snapshot any
	if resp := e.getJSON(t, "/api/den/v1/resources", &snapshot); resp.StatusCode != http.StatusOK {
		t.Fatalf("resources status %d", resp.StatusCode)
	}
	rows := mapOf(t, mapOf(t, mapOf(t, snapshot)["resources"])["llmProviders"])
	if _, present := rows["lpr_mixed"]; present {
		t.Fatalf("the snapshot must carry only rows the member is served: %v", rows)
	}

	// The member the rules name gets the row, its two models and its key.
	signInAs(t, e, "owner")
	if got := visibleModels(t, e); !strings.Contains(strings.Join(got, ","), "lpr_mixed:narrow") {
		t.Fatalf("owner must be served the row they hold every rule for: %v", got)
	}
	var payload any
	if resp := e.getJSON(t, "/api/den/v1/llm-providers/lpr_mixed/connect", &payload); resp.StatusCode != http.StatusOK {
		t.Fatalf("connect status %d", resp.StatusCode)
	} else if got := nonEmptyString(t, mapOf(t, mapOf(t, payload)["llmProvider"]), "apiKey"); got != "sk-key_mixed" {
		t.Fatalf("apiKey = %q", got)
	}
}

// A row whose audience excludes a member, opened to them by a rule on one
// model: the rule is that model's whole audience, so a personal row can offer
// the household one model without offering the rest.
func TestRuleOpensOneModelOfAnExcludedRow(t *testing.T) {
	e := newAccessTestEnv(t)
	saveModelRules(t, e, store.ModelAudience{ProviderID: "lpr_owner", ModelID: "gpt-5", Groups: []string{"owner", "family"}})

	signInAs(t, e, "family")
	want := []string{"lpr_family:glm", "lpr_family:gpt-oss", "lpr_local:qwen-27b", "lpr_local:qwen-flash", "lpr_mixed:narrow", "lpr_mixed:open", "lpr_owner:gpt-5"}
	if got := visibleModels(t, e); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("family's menu = %v, want %v", got, want)
	}
	var payload any
	if resp := e.getJSON(t, "/api/den/v1/llm-providers/lpr_owner/connect", &payload); resp.StatusCode != http.StatusOK {
		t.Fatalf("a member who sees one model of a row must be able to connect: status %d", resp.StatusCode)
	}
	models := listOf(t, mapOf(t, mapOf(t, payload)["llmProvider"])["models"])
	if len(models) != 1 {
		t.Fatalf("connect lists only the visible models, got %v", models)
	}

	// A member holding neither the row's audience nor the rule's group still
	// sees nothing of it.
	signInAs(t, e, "friends")
	if got := visibleModels(t, e); strings.Contains(strings.Join(got, ","), "lpr_owner") {
		t.Fatalf("friends must not be served lpr_owner: %v", got)
	}
	if resp := e.getJSON(t, "/api/den/v1/llm-providers/lpr_owner/connect", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("connect status %d, want 404", resp.StatusCode)
	}
}

// With nothing declared anywhere the answers are the ones SPEC-0004 describes:
// every model of every row the member's groups allow. This is the compatibility
// case the whole feature rests on, and it is the same request pair as above so
// a rule cannot be what makes a menu appear.
func TestNothingDeclaredLeavesEveryMenuAsItWas(t *testing.T) {
	e := newAccessTestEnv(t)
	signInAs(t, e, "family", "friends")
	want := []string{"lpr_family:glm", "lpr_family:gpt-oss", "lpr_local:qwen-27b", "lpr_local:qwen-flash", "lpr_mixed:narrow", "lpr_mixed:open"}
	if got := visibleModels(t, e); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("with no rules the menu is the gateway's: %v, want %v", got, want)
	}
}

// A withdrawn rule returns its model to following the row, which is how a
// restriction is undone without touching the row.
func TestWithdrawingARuleReturnsTheModelToItsRow(t *testing.T) {
	e := newAccessTestEnv(t)
	saveModelRules(t, e, store.ModelAudience{ProviderID: "lpr_mixed", ModelID: "narrow", Groups: []string{"owner"}})
	signInAs(t, e, "family", "friends")
	if got := visibleModels(t, e); strings.Contains(strings.Join(got, ","), "lpr_mixed:narrow") {
		t.Fatalf("the rule must hide the model: %v", got)
	}
	saveModelRules(t, e) // no rules at all
	if got := visibleModels(t, e); !strings.Contains(strings.Join(got, ","), "lpr_mixed:narrow") {
		t.Fatalf("with the rule gone the model follows its open row again: %v", got)
	}
}

// The console's models matrix: one line per derived model of every row, one
// column per group burrow has seen, and a save that writes exactly the cells
// the page displayed.
func TestConsoleModelAccessRendersAndSaves(t *testing.T) {
	e := newModelConsoleEnv(t)

	resp, page := e.consoleGet(t, "/admin/models")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("matrix status %d", resp.StatusCode)
	}
	if !strings.Contains(page, "Model access") || !strings.Contains(page, "Nothing configured yet") {
		t.Fatalf("an unconfigured org must say so: %s", page)
	}
	for _, model := range []string{"gpt-5", "narrow", "open", "qwen-27b", "glm"} {
		if !strings.Contains(page, "value=\""+model+"\"") {
			t.Fatalf("the matrix lists every derived model as a row, missing %s: %s", model, page)
		}
	}

	form, groups, models := modelsMatrixForm(t, page)
	if len(groups) == 0 || len(models) == 0 {
		t.Fatalf("a column per group and a row per model: %v %v", groups, models)
	}
	form.Set("cell."+groups[adminGroup]+"."+models["lpr_mixed/narrow"], "1")
	resp, _ = e.consolePost(t, "/admin/models", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save status %d", resp.StatusCode)
	}
	rules, err := e.st.ListModelAudiences(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rules) != 1 || rules[0].ProviderID != "lpr_mixed" || rules[0].ModelID != "narrow" ||
		strings.Join(rules[0].Groups, ",") != adminGroup {
		t.Fatalf("saved rules = %+v, want lpr_mixed/narrow for %s", rules, adminGroup)
	}

	// The effect is immediate for a member whose groups burrow already knows,
	// and the cell comes back checked. bob holds only family, so the rule for
	// the admin group is not his.
	if got := memberModels(t, e, "bob-sub", "family"); strings.Contains(strings.Join(got, ","), "lpr_mixed:narrow") {
		t.Fatalf("the saved rule must already apply to a member who does not hold its group: %v", got)
	}
	resp, page = e.consoleGet(t, "/admin/models")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reread status %d", resp.StatusCode)
	}
	if len(checkedCellRE.FindAllString(page, -1)) != 1 {
		t.Fatalf("the saved cell must come back checked: %s", page)
	}

	// Saving the matrix again with that box unchecked withdraws the rule
	// instead of emptying it, and the model follows its row once more.
	form, groups, models = modelsMatrixForm(t, page)
	form.Del("cell." + groups[adminGroup] + "." + models["lpr_mixed/narrow"])
	resp, _ = e.consolePost(t, "/admin/models", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("withdraw status %d", resp.StatusCode)
	}
	if rules, err := e.st.ListModelAudiences(context.Background()); err != nil || len(rules) != 0 {
		t.Fatalf("an unchecked model carries no rule: %+v %v", rules, err)
	}
}

func TestConsoleModelAccessRefusesAStalePage(t *testing.T) {
	e := newModelConsoleEnv(t)
	saveModelRules(t, e, store.ModelAudience{ProviderID: "lpr_mixed", ModelID: "narrow", Groups: []string{"family"}})

	_, page := e.consoleGet(t, "/admin/models")
	form, groups, models := modelsMatrixForm(t, page)

	// Another admin widens the same model while this page is open.
	saveModelRules(t, e, store.ModelAudience{ProviderID: "lpr_mixed", ModelID: "narrow", Groups: []string{"family", "friends"}})

	form.Set("cell."+groups[adminGroup]+"."+models["lpr_mixed/narrow"], "1")
	resp, body := e.consolePost(t, "/admin/models", form)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "reload the page") {
		t.Fatalf("status %d: a save from a stale page must be refused: %s", resp.StatusCode, body)
	}
	rules, err := e.st.ListModelAudiences(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rules) != 1 || strings.Join(rules[0].Groups, ",") != "family,friends" {
		t.Fatalf("a refused save writes nothing: %+v", rules)
	}
}

func TestConsoleModelAccessIsGatedByAdminGroups(t *testing.T) {
	e := newModelConsoleEnv(t)
	_, page := e.consoleGet(t, "/admin/models")
	form, _, _ := modelsMatrixForm(t, page)

	signInAs(t, e, "family") // alice no longer holds the admin group
	resp, body := e.consolePost(t, "/admin/models", form)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "admin_required") {
		t.Fatalf("status %d: a non-admin save must answer admin_required: %s", resp.StatusCode, body)
	}
	if resp, _ := e.consoleGet(t, "/admin/models"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a non-admin must not read the matrix either: %d", resp.StatusCode)
	}
	rules, err := e.st.ListModelAudiences(context.Background())
	if err != nil || len(rules) != 0 {
		t.Fatalf("a refused save writes nothing: %+v %v", rules, err)
	}
}

// Deleting a row while the matrix page is open must not leave rules behind that
// would resurface if the id were ever reused.
func TestConsoleModelAccessRefusesADeletedRow(t *testing.T) {
	e := newModelConsoleEnv(t)
	_, page := e.consoleGet(t, "/admin/models")
	form, _, _ := modelsMatrixForm(t, page)

	row, err := e.st.GetProvider(context.Background(), "lpr_mixed")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := e.st.DeleteProvider(context.Background(), "lpr_mixed", row.UpdatedAt, "test"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp, body := e.consolePost(t, "/admin/models", form)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "no longer exists") {
		t.Fatalf("status %d: a save naming a deleted row must be refused: %s", resp.StatusCode, body)
	}
	if rules, err := e.st.ListModelAudiences(context.Background()); err != nil || len(rules) != 0 {
		t.Fatalf("a refused save writes nothing: %+v %v", rules, err)
	}
}

// newModelConsoleEnv is the access deployment behind the console's gate: the
// same four rows, with alice holding the admin group so she can edit.
func newModelConsoleEnv(t *testing.T) *testEnv {
	t.Helper()

	gw := newFakeLLMGateway(t, accessKeys())
	for env, key := range map[string]string{
		"KEY_LOCAL":  "sk-key_local",
		"KEY_FAMILY": "sk-key_family",
		"KEY_OWNER":  "sk-key_owner",
		"KEY_MIXED":  "sk-key_mixed",
	} {
		t.Setenv(env, key)
	}
	e := newTestEnv(t)
	e.cfg.AdminGroups = []string{adminGroup}
	signInAs(t, e, adminGroup)
	seedProviders(t, e,
		store.ProviderRow{ID: "lpr_local", ProviderID: "litellm", Name: "Local",
			BaseURL: gw.baseURL(), APIKeyEnv: "KEY_LOCAL", Groups: []string{"family", "friends"}},
		store.ProviderRow{ID: "lpr_family", ProviderID: "litellm", Name: "Family",
			BaseURL: gw.baseURL(), APIKeyEnv: "KEY_FAMILY", Groups: []string{"family"}},
		store.ProviderRow{ID: "lpr_owner", ProviderID: "litellm", Name: "Owner",
			BaseURL: gw.baseURL(), APIKeyEnv: "KEY_OWNER", Groups: []string{"owner"}},
		store.ProviderRow{ID: "lpr_mixed", ProviderID: "litellm", Name: "Mixed",
			BaseURL: gw.baseURL(), APIKeyEnv: "KEY_MIXED"})
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st,
		skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}),
		providers.NewRuntime(e.st, e.cfg.ModelsTTL, providers.Unconstrained()), providers.Unconstrained(), nil).Routes()
	return e
}

// The matrix's rendered shape: one hidden field per group column, two per model
// row, the revision the page was drawn from, and the boxes that are checked.
var (
	modelGroupFieldRE  = regexp.MustCompile(`name="group\.(\d+)" value="([^"]*)"`)
	modelHiddenFieldRE = regexp.MustCompile(`name="model\.(\d+)\.(provider|id)" value="([^"]*)"`)
	modelRevisionRE    = regexp.MustCompile(`name="revision" value="([0-9a-f]+)"`)
)

// modelsMatrixForm reads the rendered matrix back into the form a browser would
// post — every hidden field and every box currently checked — and indexes the
// columns by group name and the rows by "providerID/modelID", so a test names a
// model rather than a field.
func modelsMatrixForm(t *testing.T, page string) (url.Values, map[string]string, map[string]string) {
	t.Helper()
	form := url.Values{}
	groups := map[string]string{}
	for _, match := range modelGroupFieldRE.FindAllStringSubmatch(page, -1) {
		form.Set("group."+match[1], match[2])
		groups[match[2]] = match[1]
	}
	form.Set("groupCount", strconv.Itoa(len(groups)))
	revision := modelRevisionRE.FindStringSubmatch(page)
	if len(revision) != 2 {
		t.Fatal("the matrix must post the revision it was rendered from")
	}
	form.Set("revision", revision[1])

	rowFields := map[string][2]string{}
	for _, match := range modelHiddenFieldRE.FindAllStringSubmatch(page, -1) {
		fields := rowFields[match[1]]
		if match[2] == "provider" {
			fields[0] = match[3]
		} else {
			fields[1] = match[3]
		}
		rowFields[match[1]] = fields
		form.Set("model."+match[1]+"."+match[2], match[3])
	}
	models := map[string]string{}
	for index, fields := range rowFields {
		if fields[0] == "" || fields[1] == "" {
			t.Fatalf("model row %s is missing one of its two hidden fields", index)
		}
		models[fields[0]+"/"+fields[1]] = index
	}
	for _, match := range checkedCellRE.FindAllStringSubmatch(page, -1) {
		form.Set("cell."+match[1]+"."+match[2], "1")
	}
	return form, groups, models
}

// memberModels lists what a second member is served, as "row:model" pairs. A
// rule naming the admin group has to be measured against someone who does not
// hold it, which alice's own listing cannot show.
func memberModels(t *testing.T, e *testEnv, id string, groups ...string) []string {
	t.Helper()
	ctx := context.Background()
	if err := e.st.UpsertUser(ctx, store.User{ID: id, Email: id + "@example.test", Name: id, Groups: groups}); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	token, err := e.st.CreateSession(ctx, id, e.cfg.SessionTTL)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/api/den/v1/llm-providers", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list providers as %s: status %d", id, resp.StatusCode)
	}
	var payload any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var out []string
	for _, item := range listOf(t, mapOf(t, payload)["llmProviders"]) {
		row := mapOf(t, item)
		id := row["id"].(string)
		for _, model := range listOf(t, row["models"]) {
			out = append(out, id+":"+mapOf(t, model)["id"].(string))
		}
	}
	sort.Strings(out)
	return out
}

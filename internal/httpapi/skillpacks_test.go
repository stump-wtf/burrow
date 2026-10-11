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
)

// Governing: SPEC-0003 REQ "Library audience", REQ "Compatibility"
// (stump.wtf/burrow#12). The Library's audience filter is one rule with many
// faces: the Library list, the resource snapshot the client diffs, the
// marketplace and plugin resolutions, the MCP tools, and the discovery index.
// A skill hidden on one face and visible on another is not hidden, so every
// face is checked here against the same fixture.
//
// The fixture catalog is code-review and deploy-helper (helpers_test.go).

// declareFamily restricts the "family" group to code-review, leaving
// deploy-helper to everyone else's rules by not naming a rule for it.
func declareFamily(t *testing.T, e *testEnv, skills ...string) {
	t.Helper()
	if err := e.st.SetSkillPack(context.Background(), "family", skills); err != nil {
		t.Fatalf("declare pack: %v", err)
	}
}

func libraryNames(t *testing.T, e *testEnv) []string {
	t.Helper()
	var payload map[string]any
	if resp := e.getJSON(t, "/api/den/v1/me/library", &payload); resp.StatusCode != http.StatusOK {
		t.Fatalf("library status %d", resp.StatusCode)
	}
	var names []string
	for _, item := range listOf(t, payload["items"]) {
		names = append(names, mapOf(t, item)["name"].(string))
	}
	sort.Strings(names)
	return names
}

func snapshotSkillIDs(t *testing.T, e *testEnv) []string {
	t.Helper()
	var payload map[string]any
	if resp := e.getJSON(t, "/api/den/v1/resources", &payload); resp.StatusCode != http.StatusOK {
		t.Fatalf("resources status %d", resp.StatusCode)
	}
	marketplaces := mapOf(t, mapOf(t, payload["resources"])["marketplaces"])
	var ids []string
	for _, plugin := range listOf(t, mapOf(t, firstValue(t, marketplaces))["plugins"]) {
		ids = append(ids, mapOf(t, plugin)["pluginId"].(string))
	}
	sort.Strings(ids)
	return ids
}

func firstValue(t *testing.T, m map[string]any) any {
	t.Helper()
	for _, v := range m {
		return v
	}
	t.Fatalf("expected a non-empty object")
	return nil
}

// mcpIndexNames reads skill://index.json through the MCP endpoint.
func mcpIndexNames(t *testing.T, e *testEnv) []string {
	t.Helper()
	resp, body := e.mcpPost(t, e.mcpToken(t), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "resources/read",
		"params": map[string]any{"uri": "skill://index.json"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resources/read status %d: %s", resp.StatusCode, body)
	}
	var envelope struct {
		Result struct {
			Contents []struct {
				Text string `json:"text"`
			} `json:"contents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode index: %v (%s)", err, body)
	}
	if len(envelope.Result.Contents) != 1 {
		t.Fatalf("want one content block, got %s", body)
	}
	var index struct {
		Skills []struct {
			Name string `json:"name"`
		} `json:"skills"`
	}
	if err := json.Unmarshal([]byte(envelope.Result.Contents[0].Text), &index); err != nil {
		t.Fatalf("decode index body: %v", err)
	}
	var names []string
	for _, s := range index.Skills {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return names
}

// mcpToolCall calls a tool and returns its text content.
func mcpToolCall(t *testing.T, e *testEnv, tool string, args map[string]any) (string, int) {
	t.Helper()
	resp, body := e.mcpPost(t, e.mcpToken(t), map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s status %d: %s", tool, resp.StatusCode, body)
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode %s response: %v (%s)", tool, err, body)
	}
	if envelope.Error != nil {
		return envelope.Error.Message, envelope.Error.Code
	}
	if len(envelope.Result.Content) == 0 {
		return "", 0
	}
	return envelope.Result.Content[0].Text, 0
}

func TestLibraryIsWholeCatalogWithNoRules(t *testing.T) {
	e := newTestEnv(t)
	if err := e.st.SetSkillPack(context.Background(), "family", []string{"code-review"}); err != nil {
		t.Fatalf("declare pack: %v", err)
	}
	// alice holds no group, so no rule applies to her: REQ "Compatibility".
	if got := libraryNames(t, e); strings.Join(got, ",") != "Code Review,Deploy Helper" {
		t.Fatalf("a member no rule mentions must see everything: %v", got)
	}
	if got := mcpIndexNames(t, e); strings.Join(got, ",") != "code-review,deploy-helper" {
		t.Fatalf("the agent's index must match: %v", got)
	}
}

// The whole surface, one rule: every face must agree.
func TestSkillPackRestrictsEverySurface(t *testing.T) {
	e := newTestEnv(t)
	declareFamily(t, e, "code-review")
	signInAs(t, e, "family")

	if got := libraryNames(t, e); strings.Join(got, ",") != "Code Review" {
		t.Fatalf("library: %v", got)
	}
	if got := snapshotSkillIDs(t, e); strings.Join(got, ",") != "plg_code-review" {
		t.Fatalf("resource snapshot: %v", got)
	}

	var marketplaces map[string]any
	if resp := e.getJSON(t, "/api/den/v1/marketplaces", &marketplaces); resp.StatusCode != http.StatusOK {
		t.Fatalf("marketplaces status %d", resp.StatusCode)
	}
	item := mapOf(t, listOf(t, marketplaces["items"])[0])
	if count, _ := item["pluginCount"].(float64); count != 1 {
		t.Fatalf("marketplace pluginCount = %v, want 1", item["pluginCount"])
	}

	var resolved map[string]any
	if resp := e.getJSON(t, "/api/den/v1/marketplaces/mkt_stump-labs/resolved", &resolved); resp.StatusCode != http.StatusOK {
		t.Fatalf("resolved status %d", resp.StatusCode)
	}
	plugins := listOf(t, mapOf(t, resolved["item"])["plugins"])
	if len(plugins) != 1 || mapOf(t, plugins[0])["name"] != "Code Review" {
		t.Fatalf("resolved plugins: %v", plugins)
	}

	var caps map[string]any
	if resp := e.getJSON(t, "/api/den/v1/resources/marketplace-capabilities", &caps); resp.StatusCode != http.StatusOK {
		t.Fatalf("capabilities status %d", resp.StatusCode)
	}
	if items := listOf(t, caps["items"]); len(items) != 1 || mapOf(t, items[0])["configObjectId"] != "cfo_code-review" {
		t.Fatalf("capabilities items: %v", items)
	}

	// A hidden skill answers exactly like an absent one, on every read path.
	var ignored any
	if resp := e.getJSON(t, "/api/den/v1/plugins/plg_deploy-helper/resolved", &ignored); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("hidden plugin must look absent, got %d", resp.StatusCode)
	}
	if resp := e.getJSON(t, "/api/den/v1/plugins/plg_code-review/resolved", &ignored); resp.StatusCode != http.StatusOK {
		t.Fatalf("visible plugin: %d", resp.StatusCode)
	}
	if resp := e.getJSON(t, "/api/den/v1/config-objects/cfo_deploy-helper/versions/latest", &ignored); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("hidden config object must look absent, got %d", resp.StatusCode)
	}

	// The agent's three faces: index, search, read.
	if got := mcpIndexNames(t, e); strings.Join(got, ",") != "code-review" {
		t.Fatalf("skill index: %v", got)
	}
	if text, code := mcpToolCall(t, e, "search_capabilities", map[string]any{"query": ""}); code != 0 ||
		strings.Contains(text, "deploy-helper") || !strings.Contains(text, "code-review") {
		t.Fatalf("search_capabilities leaked or lost a skill (code %d): %s", code, text)
	}
	if _, code := mcpToolCall(t, e, "get_skill", map[string]any{"name": "deploy-helper"}); code != -32002 {
		t.Fatalf("get_skill on a hidden skill must answer unknown skill, got code %d", code)
	}
	if _, code := mcpToolCall(t, e, "execute_capability", map[string]any{"capability": "skill:deploy-helper"}); code != -32002 {
		t.Fatalf("execute_capability on a hidden skill must answer unknown capability, got code %d", code)
	}
	if text, code := mcpToolCall(t, e, "get_skill", map[string]any{"name": "code-review"}); code != 0 ||
		!strings.Contains(text, "Follow this skill for code-review") {
		t.Fatalf("get_skill on a visible skill (code %d): %s", code, text)
	}
}

// A member in a group whose rule allows nothing sees no skills at all —
// distinct from a member no rule mentions.
func TestDeclaredEmptyAllowanceShowsNothing(t *testing.T) {
	e := newTestEnv(t)
	declareFamily(t, e)
	signInAs(t, e, "family")

	if got := libraryNames(t, e); len(got) != 0 {
		t.Fatalf("an empty allowance shows nothing: %v", got)
	}
	if got := mcpIndexNames(t, e); len(got) != 0 {
		t.Fatalf("the agent's index must be empty too: %v", got)
	}
}

// Two groups on one member union their allowances; a group with no rule adds
// nothing to the union.
func TestTwoGroupsUnionTheirAllowances(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if err := e.st.SetSkillPack(ctx, "family", []string{"code-review"}); err != nil {
		t.Fatalf("declare family: %v", err)
	}
	if err := e.st.SetSkillPack(ctx, "ops", []string{"deploy-helper"}); err != nil {
		t.Fatalf("declare ops: %v", err)
	}
	signInAs(t, e, "family", "strangers")

	if got := libraryNames(t, e); strings.Join(got, ",") != "Code Review" {
		t.Fatalf("an undeclared group must add nothing: %v", got)
	}
	signInAs(t, e, "family", "ops")
	if got := libraryNames(t, e); strings.Join(got, ",") != "Code Review,Deploy Helper" {
		t.Fatalf("two declared groups union: %v", got)
	}
}

// An MCP token is its member's session seen from the agent's side: the agent
// gets exactly the skill set the signed-in member would.
func TestMCPTokenCarriesItsMembersAudience(t *testing.T) {
	e := newTestEnv(t)
	declareFamily(t, e, "code-review")

	if got := mcpIndexNames(t, e); strings.Join(got, ",") != "code-review,deploy-helper" {
		t.Fatalf("before the group exists, everything is visible: %v", got)
	}
	signInAs(t, e, "family")
	if got := mcpIndexNames(t, e); strings.Join(got, ",") != "code-review" {
		t.Fatalf("the token's member holds family, so the agent sees that member's slice: %v", got)
	}
}

var (
	groupFieldRE  = regexp.MustCompile(`name="group\.(\d+)" value="([^"]*)"`)
	skillFieldRE  = regexp.MustCompile(`name="skill\.(\d+)" value="([^"]*)"`)
	declareFormRE = regexp.MustCompile(`name="declare\.(\d+)" value="1" checked`)
	checkedCellRE = regexp.MustCompile(`name="cell\.(\d+)\.(\d+)" value="1" checked`)
)

// matrixForm rebuilds the posted form from the rendered page, carrying every
// field as the browser would see it — switches and cells already checked
// included — so the test drives the real form rather than a guess about it.
func matrixForm(t *testing.T, page string) (url.Values, map[string]string, map[string]string) {
	t.Helper()
	form := url.Values{}
	groups := map[string]string{}
	for _, m := range groupFieldRE.FindAllStringSubmatch(page, -1) {
		groups[m[2]] = m[1]
		form.Set("group."+m[1], m[2])
	}
	form.Set("groupCount", strconv.Itoa(len(groups)))
	skills := map[string]string{}
	for _, m := range skillFieldRE.FindAllStringSubmatch(page, -1) {
		skills[m[2]] = m[1]
		form.Set("skill."+m[1], m[2])
	}
	for _, m := range declareFormRE.FindAllStringSubmatch(page, -1) {
		form.Set("declare."+m[1], "1")
	}
	for _, m := range checkedCellRE.FindAllStringSubmatch(page, -1) {
		form.Set("cell."+m[1]+"."+m[2], "1")
	}
	return form, groups, skills
}

func TestConsoleMatrixRendersAndSaves(t *testing.T) {
	e := newConsoleEnv(t)

	resp, page := e.consoleGet(t, "/admin/skills")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("matrix status %d", resp.StatusCode)
	}
	if !strings.Contains(page, "Skill packs") || !strings.Contains(page, "Nothing configured yet") {
		t.Fatalf("an unconfigured Library must say so: %s", page)
	}
	if !strings.Contains(page, "name=\"cell.0.0\"") || !strings.Contains(page, "code-review") {
		t.Fatalf("the matrix must list the catalog as rows: %s", page)
	}

	form, groups, skills := matrixForm(t, page)
	if len(groups) == 0 {
		t.Fatalf("a column per group burrow has seen: %v", groups)
	}
	// Switch the admin's own group on, checking only code-review. Column and
	// row indices come from the page.
	column := groups[adminGroup]
	form.Set("declare."+column, "1")
	form.Set("cell."+column+"."+skills["code-review"], "1")

	resp, _ = e.consolePost(t, "/admin/skills", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save status %d", resp.StatusCode)
	}
	packs, err := e.st.ListSkillPacks(context.Background())
	if err != nil {
		t.Fatalf("list packs: %v", err)
	}
	if got := strings.Join(packs[adminGroup], ","); got != "code-review" {
		t.Fatalf("saved allowance = %q, want code-review", got)
	}

	// The effect is immediate for a member whose groups burrow already knows.
	if got := libraryNames(t, e); strings.Join(got, ",") != "Code Review" {
		t.Fatalf("library after save: %v", got)
	}

	// Switching the column back off withdraws the rule rather than emptying
	// it, which returns everyone to the org-wide default.
	resp, page = e.consoleGet(t, "/admin/skills")
	if !strings.Contains(page, "checked") {
		t.Fatalf("the saved rule must come back switched on: %s", page)
	}
	form, groups, _ = matrixForm(t, page)
	off := url.Values{}
	off.Set("groupCount", strconv.Itoa(len(groups)))
	for name, column := range groups {
		off.Set("group."+column, name)
	}
	if resp, _ = e.consolePost(t, "/admin/skills", off); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("withdraw status %d", resp.StatusCode)
	}
	packs, err = e.st.ListSkillPacks(context.Background())
	if err != nil {
		t.Fatalf("list after withdraw: %v", err)
	}
	if len(packs) != 0 {
		t.Fatalf("withdrawing a column's switch must leave no rule: %v", packs)
	}
}

// Saving rewrites the columns the page showed, so a column nobody touched has
// to come back exactly as it rendered — otherwise saving one group would empty
// every other group's rule, because unchecked cells are how a browser submits
// an untouched matrix.
func TestConsoleSaveKeepsUntouchedColumnsAsRendered(t *testing.T) {
	e := newConsoleEnv(t)
	if err := e.st.SetSkillPack(context.Background(), "family", []string{"code-review"}); err != nil {
		t.Fatalf("declare family: %v", err)
	}
	resp, page := e.consoleGet(t, "/admin/skills")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("matrix status %d", resp.StatusCode)
	}
	form, groups, skills := matrixForm(t, page)
	column := groups[adminGroup]
	form.Set("declare."+column, "1")
	form.Set("cell."+column+"."+skills["deploy-helper"], "1")
	if resp, _ = e.consolePost(t, "/admin/skills", form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save status %d", resp.StatusCode)
	}
	packs, err := e.st.ListSkillPacks(context.Background())
	if err != nil {
		t.Fatalf("list packs: %v", err)
	}
	if got := strings.Join(packs["family"], ","); got != "code-review" {
		t.Fatalf("a column nobody switched on keeps its rule: %v", packs)
	}
	if got := strings.Join(packs[adminGroup], ","); got != "deploy-helper" {
		t.Fatalf("the switched-on column saved: %v", packs)
	}
}

func TestSkillMatrixPageIsGatedByAdminGroups(t *testing.T) {
	e := newTestEnv(t) // no BURROW_ADMIN_GROUPS, no groups on alice
	resp, body := e.consoleGet(t, "/admin/skills")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "admin_required") {
		t.Fatalf("status %d: closed console must answer admin_required", resp.StatusCode)
	}
}

// A member directory row whose groups came from sign-in is the only source of
// columns, so a rule for a group nobody has signed in with is called out
// instead of sitting silently on the page.
func TestConsoleFlagsARuleNamingAnUnseenGroup(t *testing.T) {
	e := newConsoleEnv(t)
	if err := e.st.SetSkillPack(context.Background(), "typo-group", []string{"code-review"}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	resp, page := e.consoleGet(t, "/admin/skills")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(page, "typo-group") || !strings.Contains(page, "no member has signed in") {
		t.Fatalf("the page must flag a group nobody belongs to: %s", page)
	}
	// The store still carries it; this is a warning, not a refusal.
	packs, err := e.st.ListSkillPacks(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, declared := packs["typo-group"]; !declared {
		t.Fatalf("typo-group: %v", packs)
	}
}

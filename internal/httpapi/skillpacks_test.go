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

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
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
	// No rule written anywhere: REQ "Compatibility". An org that has never
	// touched the matrix behaves exactly as it did before it existed — this
	// is the one whole-catalog answer the fail-closed default still gives
	// (ADR-0006, stump.wtf/burrow#39).
	if got := libraryNames(t, e); strings.Join(got, ",") != "Code Review,Deploy Helper" {
		t.Fatalf("a member before the first rule must see everything: %v", got)
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

// The three drift paths of stump.wtf/burrow#39, failed closed on every face.
// Before the change each of these lifted every restriction — the member got
// the whole catalog, bodies included over MCP. The undeclared group is the
// path a brand-new IdP group takes before any rule names it; the others are
// recorded here once, and all three meet the same empty slice.
func TestUndeclaredGroupFailsClosedOnEverySurface(t *testing.T) {
	e := newTestEnv(t)
	declareFamily(t, e, "code-review")
	signInAs(t, e, "strangers")

	if got := libraryNames(t, e); len(got) != 0 {
		t.Fatalf("a member holding only an undeclared group must see nothing: %v", got)
	}
	if got := snapshotSkillIDs(t, e); len(got) != 0 {
		t.Fatalf("the resource snapshot must be empty: %v", got)
	}
	if got := mcpIndexNames(t, e); len(got) != 0 {
		t.Fatalf("the agent's index must be empty: %v", got)
	}
	if _, code := mcpToolCall(t, e, "get_skill", map[string]any{"name": "code-review"}); code != -32002 {
		t.Fatalf("get_skill for a failed-closed member must answer unknown skill, got code %d", code)
	}
}

// A sign-in that carried no groups claim at all (IdP config drift, or
// BURROW_OIDC_GROUPS_SCOPE=false) used to lift every restriction while rules
// existed. It now lands on the fail-closed default.
func TestSignInWithoutGroupsClaimFailsClosed(t *testing.T) {
	e := newTestEnv(t)
	declareFamily(t, e, "code-review")
	signInAs(t, e)

	if got := libraryNames(t, e); len(got) != 0 {
		t.Fatalf("an empty groups claim must see nothing while rules exist: %v", got)
	}
	if got := mcpIndexNames(t, e); len(got) != 0 {
		t.Fatalf("the agent's index must be empty too: %v", got)
	}
}

// An IdP rename or re-case of a declared group lifted its restriction at the
// next sign-in. Now the renamed group is simply undeclared — nothing until an
// admin declares the new name, which restores the pack.
func TestRenamedGroupFailsClosedUntilDeclaredAgain(t *testing.T) {
	e := newTestEnv(t)
	declareFamily(t, e, "code-review")
	signInAs(t, e, "Family")

	if got := libraryNames(t, e); len(got) != 0 {
		t.Fatalf("a renamed group must not lift its rule: %v", got)
	}
	if err := e.st.SetSkillPack(context.Background(), "Family", []string{"code-review"}); err != nil {
		t.Fatalf("declare the renamed group: %v", err)
	}
	if got := libraryNames(t, e); strings.Join(got, ",") != "Code Review" {
		t.Fatalf("declaring the renamed group restores its pack: %v", got)
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

	// alice holds no group yet: a rule exists, so the fail-closed default
	// answers — the agent's view is its member's view, quiet included
	// (ADR-0006, stump.wtf/burrow#39).
	if got := mcpIndexNames(t, e); len(got) != 0 {
		t.Fatalf("before the group exists the fail-closed default applies: %v", got)
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
	revisionRE    = regexp.MustCompile(`name="revision" value="([0-9a-f]+)"`)
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
	if m := revisionRE.FindStringSubmatch(page); m != nil {
		form.Set("revision", m[1])
	}
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
	// Match the switch and the cell themselves: the page's own help text
	// says "checked", so a bare substring test passes on any render.
	if len(declareFormRE.FindAllString(page, -1)) != 1 || len(checkedCellRE.FindAllString(page, -1)) != 1 {
		t.Fatalf("the saved rule must come back switched on with its one cell checked: %s", page)
	}
	form, groups, _ = matrixForm(t, page)
	off := url.Values{}
	off.Set("groupCount", strconv.Itoa(len(groups)))
	off.Set("revision", form.Get("revision"))
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
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Admin console not enabled") {
		t.Fatalf("status %d: a closed console must answer its notice page", resp.StatusCode)
	}
	if strings.Contains(body, "admin_required") {
		t.Fatal("a closed console is not a refusal of the member (ADR-0007)")
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

// The other half of the drift picture (stump.wtf/burrow#39): a group members
// sign in under that no rule names now puts them on the fail-closed default,
// so the page must name it — an admin has to be able to see the drift, not
// wait for a member to report a quiet Library.
func TestConsoleNamesASignedInGroupWithNoRule(t *testing.T) {
	e := newConsoleEnv(t)
	if err := e.st.SetSkillPack(context.Background(), "family", []string{"code-review"}); err != nil {
		t.Fatalf("declare family: %v", err)
	}
	signInAs(t, e, "drifted", adminGroup)
	resp, page := e.consoleGet(t, "/admin/skills")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(page, "drifted") || !strings.Contains(page, "no rule names that group") {
		t.Fatalf("the page must name signed-in groups no rule declares: %s", page)
	}
}

// Declaring a group by name gives it a column before any of its members has
// ever signed in — the fail-closed default would otherwise leave a new IdP
// group on an invisible rule of nothing (stump.wtf/burrow#39).
func TestConsoleDeclaresAGroupByName(t *testing.T) {
	e := newConsoleEnv(t)
	resp, page := e.consoleGet(t, "/admin/skills")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	form, _, _ := matrixForm(t, page)
	form.Set("newGroup", " newteam ")
	if resp, _ = e.consolePost(t, "/admin/skills", form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("declare status %d", resp.StatusCode)
	}
	packs, err := e.st.ListSkillPacks(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	allowed, declared := packs["newteam"]
	if !declared || len(allowed) != 0 {
		t.Fatalf("declaring by name writes a rule that allows nothing: %v", packs)
	}
	// The reloaded page gives it a column, so its cells can be checked.
	_, page = e.consoleGet(t, "/admin/skills")
	if !strings.Contains(page, `value="newteam"`) {
		t.Fatalf("the declared group must render as a column: %s", page)
	}
}

// A name typed into the box that the page already renders as a column is that
// column's business: re-declaring it empty from the text field would erase
// the rule drawn on the page.
func TestConsoleDeclareByNameLeavesRenderedColumnsAlone(t *testing.T) {
	e := newConsoleEnv(t)
	if err := e.st.SetSkillPack(context.Background(), "family", []string{"code-review"}); err != nil {
		t.Fatalf("declare family: %v", err)
	}
	resp, page := e.consoleGet(t, "/admin/skills")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	form, groups, skills := matrixForm(t, page)
	form.Set("declare."+groups["family"], "1")
	form.Set("cell."+groups["family"]+"."+skills["code-review"], "1")
	form.Set("newGroup", "family")
	if resp, _ = e.consolePost(t, "/admin/skills", form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save status %d", resp.StatusCode)
	}
	packs, err := e.st.ListSkillPacks(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := strings.Join(packs["family"], ","); got != "code-review" {
		t.Fatalf("a name typed that is already a column must not erase it: %v", packs)
	}
}

// A page left open while another admin saves is drawn from rules that no longer
// exist. Its save must change nothing: posted as-is it would read a rule
// declared since as a column "switched off" and withdraw it, and put back any
// skill removed since.
func TestConsoleRefusesASaveFromAStalePage(t *testing.T) {
	e := newConsoleEnv(t)
	ctx := context.Background()
	if err := e.st.UpsertUser(ctx, store.User{
		ID: "bob-sub", Email: "bob@example.test", Name: "Bob", Groups: []string{"contractors"},
	}); err != nil {
		t.Fatalf("seed a contractor: %v", err)
	}
	if err := e.st.SetSkillPack(ctx, "family", []string{"code-review", "deploy-helper"}); err != nil {
		t.Fatalf("declare family: %v", err)
	}
	_, stale := e.consoleGet(t, "/admin/skills") // admin A's tab

	// Admin B, meanwhile: restrict contractors, and take a skill off family.
	if err := e.st.SetSkillPack(ctx, "contractors", []string{"code-review"}); err != nil {
		t.Fatalf("declare contractors: %v", err)
	}
	if err := e.st.SetSkillPack(ctx, "family", []string{"code-review"}); err != nil {
		t.Fatalf("narrow family: %v", err)
	}

	// Admin A saves a change to their own column from the old page.
	form, groups, skills := matrixForm(t, stale)
	own := groups[adminGroup]
	form.Set("declare."+own, "1")
	form.Set("cell."+own+"."+skills["code-review"], "1")
	resp, body := e.consolePost(t, "/admin/skills", form)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "reload the page") {
		t.Fatalf("a stale save must be refused with a reload hint, got %d: %s", resp.StatusCode, body)
	}

	packs, err := e.st.ListSkillPacks(ctx)
	if err != nil {
		t.Fatalf("list packs: %v", err)
	}
	if got := strings.Join(packs["contractors"], ","); got != "code-review" {
		t.Fatalf("the rule declared since the page rendered must survive: %v", packs)
	}
	if got := strings.Join(packs["family"], ","); got != "code-review" {
		t.Fatalf("a skill removed since the page rendered must stay removed: %v", packs)
	}
	if _, declared := packs[adminGroup]; declared {
		t.Fatalf("a refused save writes nothing, not even its own column: %v", packs)
	}

	// Reloading gives a page that saves.
	_, fresh := e.consoleGet(t, "/admin/skills")
	form, groups, skills = matrixForm(t, fresh)
	own = groups[adminGroup]
	form.Set("declare."+own, "1")
	form.Set("cell."+own+"."+skills["code-review"], "1")
	if resp, body = e.consolePost(t, "/admin/skills", form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a fresh page must save, got %d: %s", resp.StatusCode, body)
	}
}

// A form with no revision at all — a page rendered by an older build — is
// stale by definition.
func TestConsoleRefusesASaveWithNoRevision(t *testing.T) {
	e := newConsoleEnv(t)
	_, page := e.consoleGet(t, "/admin/skills")
	form, groups, _ := matrixForm(t, page)
	form.Del("revision")
	form.Set("declare."+groups[adminGroup], "1")
	if resp, _ := e.consolePost(t, "/admin/skills", form); resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409", resp.StatusCode)
	}
	packs, err := e.st.ListSkillPacks(context.Background())
	if err != nil {
		t.Fatalf("list packs: %v", err)
	}
	if len(packs) != 0 {
		t.Fatalf("nothing may be written: %v", packs)
	}
}

// A SKILL.md with broken frontmatter is skipped by the loader, so its row is
// not on the page. Saving anything then must not strip that skill from the
// groups that had it: nobody unchecked it, and it would stay gone after the
// file was fixed.
func TestConsoleSaveKeepsSkillsThePageCouldNotShow(t *testing.T) {
	e := newConsoleEnv(t)
	ctx := context.Background()
	if err := e.st.SetSkillPack(ctx, "family", []string{"code-review", "deploy-helper"}); err != nil {
		t.Fatalf("declare family: %v", err)
	}
	// The page arrives as if deploy-helper had dropped out of the catalog:
	// same rules, one row fewer.
	_, page := e.consoleGet(t, "/admin/skills")
	form, _, skills := matrixForm(t, page)
	gone := skills["deploy-helper"]
	form.Del("skill." + gone)
	for key := range form {
		if strings.HasPrefix(key, "cell.") && strings.HasSuffix(key, "."+gone) {
			form.Del(key)
		}
	}
	if gone != strconv.Itoa(len(skills)-1) {
		t.Fatalf("this test drops the last row; deploy-helper is row %s of %d", gone, len(skills))
	}
	if resp, body := e.consolePost(t, "/admin/skills", form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save status %d: %s", resp.StatusCode, body)
	}
	packs, err := e.st.ListSkillPacks(ctx)
	if err != nil {
		t.Fatalf("list packs: %v", err)
	}
	if got := strings.Join(packs["family"], ","); got != "code-review,deploy-helper" {
		t.Fatalf("family's allowance after an untouched save = %q, want both skills", got)
	}
}

// The matrix's write is behind the same gate as its page: a signed-in member
// outside the admin groups changes nothing.
func TestSkillMatrixSaveIsGatedByAdminGroups(t *testing.T) {
	e := newConsoleEnv(t)
	_, page := e.consoleGet(t, "/admin/skills")
	form, groups, skills := matrixForm(t, page)
	own := groups[adminGroup]
	form.Set("declare."+own, "1")
	form.Set("cell."+own+"."+skills["code-review"], "1")

	signInAs(t, e, "family") // alice's next sign-in no longer carries the admin group
	resp, body := e.consolePost(t, "/admin/skills", form)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "admin_required") {
		t.Fatalf("status %d: a non-admin save must answer admin_required: %s", resp.StatusCode, body)
	}
	packs, err := e.st.ListSkillPacks(context.Background())
	if err != nil {
		t.Fatalf("list packs: %v", err)
	}
	if len(packs) != 0 {
		t.Fatalf("a refused save writes nothing: %v", packs)
	}
}

// signInThroughIssuer signs the fake issuer's member in through the real OIDC
// callback, so the groups claim reaches the store the way production records
// it rather than the way a test would write it.
func signInThroughIssuer(t *testing.T, e *testEnv, browser *http.Client) {
	t.Helper()
	followed := browser.CheckRedirect
	browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer func() { browser.CheckRedirect = followed }()

	start, err := browser.Get(e.srv.URL + "/auth/oidc/start")
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	authorize, err := url.Parse(start.Header.Get("Location"))
	if err != nil || start.StatusCode != http.StatusFound {
		t.Fatalf("start: status %d, location error %v", start.StatusCode, err)
	}
	// An issuer that redirects back names the callback itself; one that only
	// renders its authorize page leaves the test to open the callback.
	callback := e.srv.URL + "/auth/oidc/callback?code=c&state=" + url.QueryEscape(authorize.Query().Get("state"))
	approved, err := browser.Get(authorize.String())
	if err != nil {
		t.Fatal(err)
	}
	approved.Body.Close()
	if approved.StatusCode == http.StatusFound {
		callback = approved.Header.Get("Location")
	}
	done, err := browser.Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	done.Body.Close()
	if done.StatusCode != http.StatusOK {
		t.Fatalf("callback: status %d", done.StatusCode)
	}
}

// A group that reaches burrow with stray whitespace is still one group. The
// console trims the names it lists and the rules it saves, so a claim kept
// verbatim would put the member under a column whose rule can never match
// them: shown as restricted, served the whole Library.
func TestSkillPackAppliesToAGroupClaimedWithPadding(t *testing.T) {
	e := newConsoleEnv(t)
	issuer := newFakeIssuer(t, "burrow-desktop")
	issuer.groups = []any{"contractors "}
	signInThroughIssuer(t, e, newOIDCClientFor(t, e, issuer))

	// The admin sees the member's group as a column and restricts it.
	resp, page := e.consoleGet(t, "/admin/skills")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("matrix status %d", resp.StatusCode)
	}
	form, groups, skills := matrixForm(t, page)
	column, listed := groups["contractors"]
	if !listed {
		t.Fatalf("the member's group must be a column named contractors: %v", groups)
	}
	form.Set("declare."+column, "1")
	form.Set("cell."+column+"."+skills["code-review"], "1")
	if resp, _ = e.consolePost(t, "/admin/skills", form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save status %d", resp.StatusCode)
	}

	// The rest of the test is the member: their session, their MCP token.
	member, err := e.st.CreateSession(context.Background(), "signer-sub-1", e.cfg.SessionTTL)
	if err != nil {
		t.Fatalf("member session: %v", err)
	}
	e.token = member
	if got := libraryNames(t, e); strings.Join(got, ",") != "Code Review" {
		t.Fatalf("the rule saved for the member's column must apply to them: %v", got)
	}
	if _, code := mcpToolCall(t, e, "get_skill", map[string]any{"name": "deploy-helper"}); code != -32002 {
		t.Fatalf("get_skill on a skill outside the member's rule must answer unknown skill, got code %d", code)
	}
	if text, code := mcpToolCall(t, e, "get_skill", map[string]any{"name": "code-review"}); code != 0 ||
		!strings.Contains(text, "Follow this skill for code-review") {
		t.Fatalf("get_skill on the allowed skill (code %d): %s", code, text)
	}
}

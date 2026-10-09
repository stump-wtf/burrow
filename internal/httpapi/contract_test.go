package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// One contract test per Stage A endpoint, asserting the exact fields the
// desktop client reads (docs/client-contract.md, Stage A table). A field the
// client requires that burrow omits must fail here, not in the app.

func TestContractMe(t *testing.T) {
	e := newTestEnv(t)
	var payload any
	resp := e.getJSON(t, "/api/den/v1/me", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	user := mapOf(t, mapOf(t, payload)["user"])
	if nonEmptyString(t, user, "id") != aliceSub {
		t.Fatalf("user.id must be the subject: %#v", user)
	}
	if nonEmptyString(t, user, "email") != aliceEmail {
		t.Fatalf("user.email mismatch: %#v", user)
	}
	nonEmptyString(t, user, "name")
}

func TestContractMyOrgs(t *testing.T) {
	e := newTestEnv(t)
	var payload any
	resp := e.getJSON(t, "/api/den/v1/me/orgs", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	root := mapOf(t, payload)
	orgs := listOf(t, root["orgs"])
	if len(orgs) != 1 {
		t.Fatalf("stage A has exactly one org, got %d", len(orgs))
	}
	// The client drops a row missing ANY of the four fields.
	org := mapOf(t, orgs[0])
	nonEmptyString(t, org, "id")
	nonEmptyString(t, org, "name")
	nonEmptyString(t, org, "slug")
	nonEmptyString(t, org, "role")
	nonEmptyString(t, root, "activeOrgId")
	nonEmptyString(t, root, "activeOrgSlug")
}

func TestContractSetActiveOrganization(t *testing.T) {
	e := newTestEnv(t)
	var payload any
	resp := e.doJSON(t, http.MethodPost, "/api/den/v1/me/active-organization",
		map[string]string{"organizationId": e.cfg.OrgID()}, &payload, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestContractDesktopConfig(t *testing.T) {
	e := newTestEnv(t)
	var payload any
	resp := e.getJSON(t, "/api/den/v1/me/desktop-config", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	cfg := mapOf(t, payload)
	// connectEnabled must be true or the agent gets no remote skills at all.
	if cfg["connectEnabled"] != true {
		t.Fatalf("connectEnabled must be exactly true, got %#v", cfg["connectEnabled"])
	}
	if cfg["automationsEnabled"] != false {
		t.Fatalf("automationsEnabled must be false for stage A, got %#v", cfg["automationsEnabled"])
	}
	if cfg["dashboardEnabled"] != false {
		t.Fatalf("dashboardEnabled must be false for stage A, got %#v", cfg["dashboardEnabled"])
	}
}

func TestContractResourcesSnapshot(t *testing.T) {
	e := newTestEnv(t)
	var payload any
	resp := e.getJSON(t, "/api/den/v1/resources", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	root := mapOf(t, payload)
	nonEmptyString(t, root, "organizationId")
	nonEmptyString(t, root, "orgMemberId")
	if teams := listOf(t, root["teamIds"]); len(teams) != 0 {
		t.Fatalf("teamIds must be an empty list, got %#v", teams)
	}
	resources := mapOf(t, root["resources"])
	if len(mapOf(t, resources["llmProviders"])) != 0 {
		t.Fatalf("llmProviders must be an empty map for stage A")
	}
	marketplaces := mapOf(t, resources["marketplaces"])
	if len(marketplaces) != 1 {
		t.Fatalf("one marketplace expected, got %d", len(marketplaces))
	}
	for id, raw := range marketplaces {
		if !strings.HasPrefix(id, "mkt_") {
			t.Fatalf("marketplace key must be the marketplace id, got %q", id)
		}
		entry := mapOf(t, raw)
		nonEmptyString(t, entry, "lastUpdatedAt")
		plugins := listOf(t, entry["plugins"])
		if len(plugins) != 2 {
			t.Fatalf("two skills expected in the snapshot, got %d", len(plugins))
		}
		plugin := mapOf(t, plugins[0])
		nonEmptyString(t, plugin, "pluginId")
		nonEmptyString(t, plugin, "lastUpdatedAt")
		items := listOf(t, plugin["configItems"])
		if len(items) != 1 {
			t.Fatalf("one config item per skill, got %d", len(items))
		}
		nonEmptyString(t, mapOf(t, items[0]), "configItemId")
		nonEmptyString(t, mapOf(t, items[0]), "lastUpdatedAt")
	}
}

func TestContractOrgDirectory(t *testing.T) {
	e := newTestEnv(t)
	var payload any
	resp := e.getJSON(t, "/api/den/v1/org", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	root := mapOf(t, payload)
	if id := nonEmptyString(t, mapOf(t, root["currentMember"]), "id"); !strings.HasPrefix(id, "mem_") {
		t.Fatalf("currentMember.id must be a member id, got %q", id)
	}
	members := listOf(t, root["members"])
	if len(members) != 1 {
		t.Fatalf("one member expected, got %d", len(members))
	}
	member := mapOf(t, members[0])
	nonEmptyString(t, member, "id")
	user := mapOf(t, member["user"])
	nonEmptyString(t, user, "name")
	nonEmptyString(t, user, "email")
	if teams := listOf(t, root["teams"]); len(teams) != 0 {
		t.Fatalf("teams must be present and empty, got %#v", teams)
	}
}

func TestContractLibrary(t *testing.T) {
	e := newTestEnv(t)
	var payload any
	resp := e.getJSON(t, "/api/den/v1/me/library", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	items := listOf(t, mapOf(t, payload)["items"])
	if len(items) != 2 {
		t.Fatalf("two skills expected, got %d", len(items))
	}
	item := mapOf(t, items[0])
	if item["type"] != "plugin" {
		t.Fatalf("skills map to plugin items, got %v", item["type"])
	}
	nonEmptyString(t, item, "id")
	nonEmptyString(t, item, "name")
	if _, ok := item["description"].(string); !ok {
		t.Fatalf("description must be a string (possibly empty), got %#v", item["description"])
	}
	kinds := listOf(t, item["componentKinds"])
	if len(kinds) == 0 || kinds[0] != "skill" {
		t.Fatalf("componentKinds must include skill, got %#v", kinds)
	}
	if item["componentCount"] != float64(1) {
		t.Fatalf("componentCount must be 1, got %#v", item["componentCount"])
	}
	if item["role"] != "owner" {
		t.Fatalf("role must be a known role, got %#v", item["role"])
	}
	edges := listOf(t, item["edges"])
	if len(edges) == 0 || mapOf(t, edges[0])["kind"] != "org_wide" {
		t.Fatalf("edges must include org_wide, got %#v", edges)
	}
}

func TestContractMarketplaceCapabilities(t *testing.T) {
	e := newTestEnv(t)
	var payload any
	resp := e.getJSON(t, "/api/den/v1/resources/marketplace-capabilities", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	items := listOf(t, mapOf(t, payload)["items"])
	if len(items) != 2 {
		t.Fatalf("two capabilities expected, got %d", len(items))
	}
	item := mapOf(t, items[0])
	nonEmptyString(t, item, "configObjectId")
	// marketplaceId may be null or a string; burrow sets the marketplace.
	nonEmptyString(t, item, "marketplaceId")
	if item["objectType"] != "skill" {
		t.Fatalf("objectType must be skill (a value the client models), got %#v", item["objectType"])
	}
	nonEmptyString(t, item, "pluginId")
}

func TestContractMarketplaces(t *testing.T) {
	e := newTestEnv(t)
	var payload any
	resp := e.getJSON(t, "/api/den/v1/marketplaces?status=active&limit=100", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	items := listOf(t, mapOf(t, payload)["items"])
	if len(items) != 1 {
		t.Fatalf("one marketplace, got %d", len(items))
	}
	marketplace := mapOf(t, items[0])
	nonEmptyString(t, marketplace, "id")
	nonEmptyString(t, marketplace, "name")
	if marketplace["status"] != "active" {
		t.Fatalf("status must be active, got %#v", marketplace["status"])
	}
	if marketplace["pluginCount"] != float64(2) {
		t.Fatalf("pluginCount must be 2, got %#v", marketplace["pluginCount"])
	}
	nonEmptyString(t, marketplace, "updatedAt")
}

func TestContractMarketplaceResolved(t *testing.T) {
	e := newTestEnv(t)
	var payload any
	resp := e.getJSON(t, "/api/den/v1/marketplaces/mkt_stump-labs/resolved", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	item := mapOf(t, mapOf(t, payload)["item"])
	marketplace := mapOf(t, item["marketplace"])
	nonEmptyString(t, marketplace, "id")
	nonEmptyString(t, marketplace, "name")
	plugins := listOf(t, item["plugins"])
	if len(plugins) != 2 {
		t.Fatalf("two plugins, got %d", len(plugins))
	}
	plugin := mapOf(t, plugins[0])
	nonEmptyString(t, plugin, "id")
	nonEmptyString(t, plugin, "name")
	if plugin["status"] != "active" {
		t.Fatalf("plugin status must be active, got %#v", plugin["status"])
	}
	counts := mapOf(t, plugin["componentCounts"])
	if counts["skill"] != float64(1) {
		t.Fatalf("componentCounts.skill must be 1, got %#v", counts["skill"])
	}
}

func TestContractPluginResolved(t *testing.T) {
	e := newTestEnv(t)
	var payload any
	resp := e.getJSON(t, "/api/den/v1/plugins/plg_code-review/resolved", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	items := listOf(t, mapOf(t, payload)["items"])
	if len(items) != 1 {
		t.Fatalf("one membership, got %d", len(items))
	}
	membership := mapOf(t, items[0])
	nonEmptyString(t, membership, "id")
	nonEmptyString(t, membership, "pluginId")
	nonEmptyString(t, membership, "configObjectId")
	configObject := mapOf(t, membership["configObject"])
	nonEmptyString(t, configObject, "id")
	if configObject["objectType"] != "skill" {
		t.Fatalf("objectType must be skill, got %#v", configObject["objectType"])
	}
	nonEmptyString(t, configObject, "title")
	version := mapOf(t, configObject["latestVersion"])
	nonEmptyString(t, version, "id")
	if src, ok := version["rawSourceText"].(string); !ok || !strings.Contains(src, "code-review") {
		t.Fatalf("latestVersion.rawSourceText must carry the SKILL.md source, got %#v", version["rawSourceText"])
	}
}

func TestContractPluginConfigObjects(t *testing.T) {
	e := newTestEnv(t)
	var payload any
	resp := e.getJSON(t, "/api/den/v1/plugins/plg_code-review/config-objects", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	items := listOf(t, mapOf(t, payload)["items"])
	if len(items) != 1 {
		t.Fatalf("one config object, got %d", len(items))
	}
	row := mapOf(t, items[0])
	nonEmptyString(t, row, "configObjectId")
	configObject := mapOf(t, row["configObject"])
	if configObject["objectType"] != "skill" {
		t.Fatalf("objectType must be skill, got %#v", configObject["objectType"])
	}
	if _, removed := row["removedAt"]; removed {
		t.Fatalf("active config objects must not carry removedAt")
	}
}

func TestContractConfigObjectLatestVersion(t *testing.T) {
	e := newTestEnv(t)
	var payload any
	resp := e.getJSON(t, "/api/den/v1/config-objects/cfo_code-review/versions/latest", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	item := mapOf(t, mapOf(t, payload)["item"])
	nonEmptyString(t, item, "id")
	if src, ok := item["rawSourceText"].(string); !ok || !strings.Contains(src, "code-review") {
		t.Fatalf("rawSourceText must carry the SKILL.md source, got %#v", item["rawSourceText"])
	}
}

func TestContractUnknownIDsAre404(t *testing.T) {
	e := newTestEnv(t)
	for _, path := range []string{
		"/api/den/v1/plugins/plg_missing/resolved",
		"/api/den/v1/plugins/plg_missing/config-objects",
		"/api/den/v1/config-objects/cfo_missing/versions/latest",
		"/api/den/v1/marketplaces/mkt_missing/resolved",
	} {
		var payload any
		resp := e.getJSON(t, path, &payload)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: want 404, got %d", path, resp.StatusCode)
		}
	}
}

func TestContractStageStubs(t *testing.T) {
	e := newTestEnv(t)

	var llm any
	resp := e.getJSON(t, "/api/den/v1/llm-providers", &llm)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("llm-providers status %d", resp.StatusCode)
	}
	providers := listOf(t, mapOf(t, llm)["llmProviders"])
	if len(providers) != 0 {
		t.Fatalf("stage A llmProviders must be empty, got %#v", providers)
	}

	var connections any
	resp = e.getJSON(t, "/api/den/v1/mcp-connections?scope=usable", &connections)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mcp-connections status %d", resp.StatusCode)
	}
	if conns := listOf(t, mapOf(t, connections)["connections"]); len(conns) != 0 {
		t.Fatalf("stage A mcp-connections must be empty, got %#v", conns)
	}

	resp = e.getJSON(t, "/api/den/v1/inference-providers?scope=usable", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("inference-providers must 404 (the client reads that as none), got %d", resp.StatusCode)
	}
}

func TestContractRuntimeConfig(t *testing.T) {
	e := newTestEnv(t)
	var payload any
	resp := e.getJSON(t, "/api/runtime-config", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got := nonEmptyString(t, mapOf(t, payload), "denApiUrl")
	if want := e.srv.URL + "/api/den"; got != want {
		t.Fatalf("denApiUrl: want %q, got %q", want, got)
	}
}

func TestContractSignOutPath(t *testing.T) {
	// The client posts to <api base>/api/auth/sign-out — that double /api is
	// real (contract Authentication §4) and must resolve.
	e := newTestEnv(t)
	resp := e.doJSON(t, http.MethodPost, "/api/den/api/auth/sign-out", map[string]any{}, nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-out status %d", resp.StatusCode)
	}
	// The session is gone afterwards.
	resp = e.getJSON(t, "/api/den/v1/me", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session must be revoked after sign-out, got %d", resp.StatusCode)
	}
}

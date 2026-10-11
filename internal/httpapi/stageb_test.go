package httpapi_test

// Stage B contract surface over store-backed rows and gateway-derived menus
// (SPEC-0004 REQ "Visibility unchanged": the desktop shape is untouched,
// only the menu's source moved).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/httpapi"
	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// fakeLLMGateway is a stand-in LiteLLM: GET /v1/models answers the OpenAI
// listing scoped to the presented bearer key, which is how a row's own key
// reports its own menu (ADR-0004).
type fakeLLMGateway struct {
	srv   *httptest.Server
	mu    sync.Mutex
	byKey map[string][]string
}

func newFakeLLMGateway(t *testing.T, byKey map[string][]string) *fakeLLMGateway {
	t.Helper()
	g := &fakeLLMGateway{byKey: byKey}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		key := ""
		if h := r.Header.Get("Authorization"); len(h) > len("Bearer ") {
			key = h[len("Bearer "):]
		}
		g.mu.Lock()
		ids, ok := g.byKey[key]
		g.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		data := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]any{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeLLMGateway) baseURL() string { return g.srv.URL + "/v1" }

func (g *fakeLLMGateway) set(key string, ids ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.byKey[key] = ids
}

// seedProviders inserts rows so the test's rows exist before any request.
func seedProviders(t *testing.T, e *testEnv, rows ...store.ProviderRow) {
	t.Helper()
	ctx := t.Context()
	for _, p := range rows {
		if p.ProviderID == "" {
			p.ProviderID = "litellm"
		}
		if p.Name == "" {
			p.Name = p.ID
		}
		if err := e.st.CreateProvider(ctx, p, "test"); err != nil {
			t.Fatalf("seed %s: %v", p.ID, err)
		}
	}
}

// withProviderRuntime rewires the test server to serve the store's rows,
// deriving each row's menu from its gateway key.
func withProviderRuntime(e *testEnv) *testEnv {
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st,
		skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}),
		providers.NewRuntime(e.st, e.cfg.ModelsTTL), nil).Routes()
	return e
}

// newStageBTestEnv builds the deployment the Stage B contract was written
// for: one LiteLLM row whose gateway lists two models for its key.
func newStageBTestEnv(t *testing.T) *testEnv {
	t.Helper()

	gw := newFakeLLMGateway(t, map[string][]string{"sk-stageb-test": {"gpt-5"}})
	t.Setenv("LITELLM_API_KEY", "sk-stageb-test")

	e := newTestEnv(t)
	seedProviders(t, e, store.ProviderRow{
		ID: "lpr_litellm", ProviderID: "litellm", Name: "Stump LiteLLM",
		BaseURL: gw.baseURL(), APIKeyEnv: "LITELLM_API_KEY",
		NameOverrides: map[string]string{"gpt-5": "GPT-5"},
	})
	return withProviderRuntime(e)
}

// The row must satisfy every strict rule the client's sync enforces — one
// bad row fails the whole sync on the desktop side.
func TestContractStageBLLMProviders(t *testing.T) {
	e := newStageBTestEnv(t)

	for _, path := range []string{"/api/den/v1/llm-providers", "/api/den/v1/llm-providers?scope=usable"} {
		var payload any
		resp := e.getJSON(t, path, &payload)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
		rows := listOf(t, mapOf(t, payload)["llmProviders"])
		if len(rows) != 1 {
			t.Fatalf("%s: one provider, got %d", path, len(rows))
		}
		row := mapOf(t, rows[0])
		if nonEmptyString(t, row, "id") != "lpr_litellm" {
			t.Fatalf("id must be the lpr_-prefixed id: %#v", row)
		}
		nonEmptyString(t, row, "providerId")
		nonEmptyString(t, row, "name")
		if row["source"] != "custom" {
			t.Fatalf("source must be custom (never openwork), got %#v", row["source"])
		}
		if row["hasApiKey"] != true {
			t.Fatalf("hasApiKey must be true with the key provisioned, got %#v", row["hasApiKey"])
		}
		nonEmptyString(t, row, "updatedAt")
		providerConfig := mapOf(t, row["providerConfig"])
		if providerConfig["npm"] != "@ai-sdk/openai-compatible" {
			t.Fatalf("providerConfig.npm must be the openai-compatible adapter: %#v", providerConfig)
		}
		options := mapOf(t, providerConfig["options"])
		if nonEmptyString(t, options, "baseURL") != gwBase(t, e, "lpr_litellm") {
			t.Fatalf("providerConfig.options.baseURL must be the gateway URL: %#v", options)
		}
		envs := listOf(t, providerConfig["env"])
		if len(envs) != 1 || envs[0] != "LITELLM_API_KEY" {
			t.Fatalf("providerConfig.env must name the key env: %#v", envs)
		}
		models := listOf(t, row["models"])
		if len(models) != 1 {
			t.Fatalf("one model, got %d", len(models))
		}
		model := mapOf(t, models[0])
		if nonEmptyString(t, model, "id") != "gpt-5" {
			t.Fatalf("the menu is the gateway's listing: %#v", model)
		}
		if nonEmptyString(t, model, "name") != "GPT-5" {
			t.Fatalf("nameOverrides must rename the derived id: %#v", model)
		}
		if _, ok := model["config"].(map[string]any); !ok {
			t.Fatalf("model config must be an object: %#v", model["config"])
		}
	}
}

func TestContractStageBConnect(t *testing.T) {
	e := newStageBTestEnv(t)

	var payload any
	resp := e.getJSON(t, "/api/den/v1/llm-providers/lpr_litellm/connect", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	row := mapOf(t, mapOf(t, payload)["llmProvider"])
	if nonEmptyString(t, row, "id") != "lpr_litellm" {
		t.Fatalf("connect must return the provider row: %#v", row)
	}
	if got := nonEmptyString(t, row, "apiKey"); got != "sk-stageb-test" {
		t.Fatalf("apiKey must be the provisioned key value, got %q", got)
	}

	// Unknown provider ids 404.
	resp = e.getJSON(t, "/api/den/v1/llm-providers/lpr_missing/connect", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown connect must 404, got %d", resp.StatusCode)
	}
}

// The /v1/resources snapshot carries llmProviders id→updatedAt so the client
// notices provider changes without listing them.
func TestContractStageBResourcesSnapshot(t *testing.T) {
	e := newStageBTestEnv(t)

	var payload any
	resp := e.getJSON(t, "/api/den/v1/resources", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	resources := mapOf(t, mapOf(t, payload)["resources"])
	llmProviders := mapOf(t, resources["llmProviders"])
	if len(llmProviders) != 1 {
		t.Fatalf("snapshot must carry the provider, got %#v", llmProviders)
	}
	if ts, ok := llmProviders["lpr_litellm"].(string); !ok || ts == "" {
		t.Fatalf("snapshot must map id→timestamp, got %#v", llmProviders)
	}
}

// A row with no key provisioned is listed but serves no models: the menu is
// the key's view, and there is no key to ask (ADR-0004).
func TestContractStageBUnprovisionedKeyServesNoModels(t *testing.T) {
	gw := newFakeLLMGateway(t, map[string][]string{"sk-present": {"gpt-5"}})

	t.Setenv("LITELLM_API_KEY", "sk-present")
	t.Setenv("LITELLM_SECOND_KEY", "")

	e := newTestEnv(t)
	seedProviders(t, e,
		store.ProviderRow{ID: "lpr_primary", ProviderID: "litellm", Name: "Primary",
			BaseURL: gw.baseURL(), APIKeyEnv: "LITELLM_API_KEY"},
		store.ProviderRow{ID: "lpr_unprovisioned", ProviderID: "litellm", Name: "Unprovisioned",
			BaseURL: gw.baseURL(), APIKeyEnv: "LITELLM_SECOND_KEY"})
	withProviderRuntime(e)

	var payload any
	if resp := e.getJSON(t, "/api/den/v1/llm-providers", &payload); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rows := map[string]map[string]any{}
	for _, r := range listOf(t, mapOf(t, payload)["llmProviders"]) {
		row := mapOf(t, r)
		rows[row["id"].(string)] = row
	}
	if len(rows) != 2 {
		t.Fatalf("both rows are listed, got %#v", rows)
	}
	if got := listOf(t, rows["lpr_primary"]["models"]); len(got) != 1 {
		t.Fatalf("provisioned row serves its menu, got %#v", got)
	}
	if rows["lpr_unprovisioned"]["hasApiKey"] != false {
		t.Fatalf("hasApiKey must be false for an unset key env")
	}
	if got := listOf(t, rows["lpr_unprovisioned"]["models"]); len(got) != 0 {
		t.Fatalf("an unprovisioned row serves no models, got %#v", got)
	}
}

// Without any rows the Stage A empties hold: no rows, no keys.
func TestContractStageBNoProvidersConfigured(t *testing.T) {
	e := withProviderRuntime(newTestEnv(t))

	var payload any
	resp := e.getJSON(t, "/api/den/v1/llm-providers", &payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if rows := listOf(t, mapOf(t, payload)["llmProviders"]); len(rows) != 0 {
		t.Fatalf("no providers configured, got %#v", rows)
	}

	var resources any
	e.getJSON(t, "/api/den/v1/resources", &resources)
	snapshot := mapOf(t, mapOf(t, mapOf(t, resources)["resources"])["llmProviders"])
	if len(snapshot) != 0 {
		t.Fatalf("snapshot must be empty without providers, got %#v", snapshot)
	}
}

// gwBase reads the row's stored baseURL back out of the store, so the test
// asserts the served value equals the configured one rather than a copy of
// the httptest URL that could drift from the fixture.
func gwBase(t *testing.T, e *testEnv, id string) string {
	t.Helper()
	p, err := e.st.GetProvider(t.Context(), id)
	if err != nil {
		t.Fatalf("read back %s: %v", id, err)
	}
	return p.BaseURL
}

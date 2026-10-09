package httpapi_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/httpapi"
	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
)

// newStageBTestEnv builds an env with one LiteLLM provider configured and its
// API key present in the environment.
func newStageBTestEnv(t *testing.T) *testEnv {
	t.Helper()

	e := newTestEnv(t)

	providersJSON := `{
	  "providers": [
	    {
	      "id": "lpr_litellm",
	      "providerId": "litellm",
	      "name": "Stump LiteLLM",
	      "baseURL": "https://litellm.stump.rocks/v1",
	      "apiKeyEnv": "LITELLM_API_KEY",
	      "models": [
	        {"id": "gpt-5", "name": "GPT-5", "config": {"context": 128000}}
	      ]
	    }
	  ]
	}`
	providersPath := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(providersPath, []byte(providersJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := providers.Load(providersPath)
	if err != nil {
		t.Fatalf("load providers fixture: %v", err)
	}
	info, err := os.Stat(providersPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LITELLM_API_KEY", "sk-stageb-test")

	runtime := providers.NewRuntime(list, info.ModTime())
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st,
		skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}), runtime, nil).Routes()
	return e
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
		if nonEmptyString(t, options, "baseURL") != "https://litellm.stump.rocks/v1" {
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
		nonEmptyString(t, model, "id")
		nonEmptyString(t, model, "name")
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

// Without a providers file the Stage A empties hold: no rows, no keys.
func TestContractStageBNoProvidersConfigured(t *testing.T) {
	e := newTestEnv(t)

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

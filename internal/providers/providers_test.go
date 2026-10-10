package providers_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
)

func writeProvidersFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const validFile = `{
  "providers": [
    {
      "id": "lpr_litellm",
      "providerId": "litellm",
      "name": "Stump LiteLLM",
      "baseURL": "https://litellm.stump.rocks/v1",
      "apiKeyEnv": "LITELLM_API_KEY",
      "models": [
        {"id": "gpt-5", "name": "GPT-5", "config": {"context": 128000}},
        {"id": "claude-opus-5", "name": "Claude Opus 5"}
      ]
    }
  ]
}`

func TestLoadValid(t *testing.T) {
	list, err := providers.Load(writeProvidersFile(t, validFile))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 1 || list[0].ID != "lpr_litellm" || len(list[0].Models) != 2 {
		t.Fatalf("bad parse: %+v", list)
	}
}

// The client's sync drops the whole provider list on one bad row, so every
// strict rule must fail the load.
func TestLoadRejectsStrictRuleViolations(t *testing.T) {
	cases := map[string]string{
		"id without lpr_ prefix": `{"providers":[{"id":"litellm","providerId":"litellm","name":"X","baseURL":"https://x.example/v1","apiKeyEnv":"K"}]}`,
		"empty providerId":       `{"providers":[{"id":"lpr_x","providerId":"","name":"X","baseURL":"https://x.example/v1","apiKeyEnv":"K"}]}`,
		"empty name":             `{"providers":[{"id":"lpr_x","providerId":"x","name":"","baseURL":"https://x.example/v1","apiKeyEnv":"K"}]}`,
		"relative baseURL":       `{"providers":[{"id":"lpr_x","providerId":"x","name":"X","baseURL":"litellm.internal:4000","apiKeyEnv":"K"}]}`,
		"invalid apiKeyEnv":      `{"providers":[{"id":"lpr_x","providerId":"x","name":"X","baseURL":"https://x.example/v1","apiKeyEnv":"not an env"}]}`,
		"model without name":     `{"providers":[{"id":"lpr_x","providerId":"x","name":"X","baseURL":"https://x.example/v1","apiKeyEnv":"K","models":[{"id":"m"}]}]}`,
		"model without id":       `{"providers":[{"id":"lpr_x","providerId":"x","name":"X","baseURL":"https://x.example/v1","apiKeyEnv":"K","models":[{"name":"M"}]}]}`,
		"duplicate ids":          `{"providers":[{"id":"lpr_x","providerId":"x","name":"X","baseURL":"https://x.example/v1","apiKeyEnv":"K"},{"id":"lpr_x","providerId":"x","name":"Y","baseURL":"https://y.example/v1","apiKeyEnv":"K"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := providers.Load(writeProvidersFile(t, body)); err == nil {
				t.Fatalf("must reject: %s", body)
			}
		})
	}
}

func TestRuntimeRowsAndConnect(t *testing.T) {
	list, err := providers.Load(writeProvidersFile(t, validFile))
	if err != nil {
		t.Fatal(err)
	}
	runtime := providers.NewRuntime(list, time.Unix(1700000000, 0))

	t.Setenv("LITELLM_API_KEY", "sk-test-value")
	rows := runtime.RowsFor(nil)
	if len(rows) != 1 {
		t.Fatalf("one row, got %d", len(rows))
	}
	row := rows[0]
	if row.ID != "lpr_litellm" || row.ProviderID != "litellm" || row.Name != "Stump LiteLLM" {
		t.Fatalf("row identity: %+v", row)
	}
	if row.Source != "custom" {
		t.Fatalf("source must be custom, got %q", row.Source)
	}
	if row.HasAPIKey != true {
		t.Fatalf("hasApiKey must reflect the environment")
	}
	if row.ProviderConfig["npm"] != providers.NpmPackage {
		t.Fatalf("providerConfig.npm must be %q, got %#v", providers.NpmPackage, row.ProviderConfig["npm"])
	}
	options, ok := row.ProviderConfig["options"].(map[string]any)
	if !ok || options["baseURL"] != "https://litellm.stump.rocks/v1" {
		t.Fatalf("providerConfig.options.baseURL wrong: %#v", row.ProviderConfig)
	}
	if envs, ok := row.ProviderConfig["env"].([]string); !ok || len(envs) != 1 || envs[0] != "LITELLM_API_KEY" {
		t.Fatalf("providerConfig.env must name the key env: %#v", row.ProviderConfig["env"])
	}
	if len(row.Models) != 2 || row.Models[0].ID != "gpt-5" || row.Models[1].Config == nil {
		t.Fatalf("models: %+v", row.Models)
	}
	if row.UpdatedAt == "" {
		t.Fatalf("updatedAt must be set")
	}

	connectRow, key, ok := runtime.ConnectFor("lpr_litellm", nil)
	if !ok || key == nil || *key != "sk-test-value" || connectRow.ID != "lpr_litellm" {
		t.Fatalf("connect: %v %v", key, connectRow.ID)
	}
	if _, _, ok := runtime.ConnectFor("lpr_missing", nil); ok {
		t.Fatalf("unknown provider must not connect")
	}
}

func TestRuntimeWithoutKeyReportsHasAPIKeyFalse(t *testing.T) {
	list, err := providers.Load(writeProvidersFile(t, validFile))
	if err != nil {
		t.Fatal(err)
	}
	runtime := providers.NewRuntime(list, time.Unix(1700000000, 0))
	// Force the key absent even when the host shell carries one.
	t.Setenv("LITELLM_API_KEY", "")
	rows := runtime.RowsFor(nil)
	if rows[0].HasAPIKey != false {
		t.Fatalf("hasApiKey must be false when the env var is unset")
	}
	_, key, ok := runtime.ConnectFor("lpr_litellm", nil)
	if !ok || key != nil {
		t.Fatalf("connect without a provisioned key must omit the key: %v", key)
	}
}

// A nil runtime (no providers file configured) serves the Stage A empties.
func TestNilRuntimeServesNone(t *testing.T) {
	var runtime *providers.Runtime
	if rows := runtime.RowsFor(nil); len(rows) != 0 {
		t.Fatalf("nil runtime rows: %+v", rows)
	}
	if snap := runtime.SnapshotFor(nil); len(snap) != 0 {
		t.Fatalf("nil runtime snapshot: %+v", snap)
	}
	if _, _, ok := runtime.ConnectFor("lpr_x", nil); ok {
		t.Fatalf("nil runtime must not connect")
	}
}

func TestSnapshotCarriesUpdatedAt(t *testing.T) {
	list, err := providers.Load(writeProvidersFile(t, validFile))
	if err != nil {
		t.Fatal(err)
	}
	runtime := providers.NewRuntime(list, time.Unix(1700000000, 0))
	snap := runtime.SnapshotFor(nil)
	if snap["lpr_litellm"] != runtime.UpdatedAt() {
		t.Fatalf("snapshot must map id→updatedAt: %+v", snap)
	}
}

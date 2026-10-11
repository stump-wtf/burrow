// Store-backed runtime basics: file validation (the seed path), row shape
// and connect for store rows, and the nil-runtime empties. Gateway-derived
// menus have their own coverage in gateway_test.go.
package providers_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store/storetest"
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
		"burrow's own env var":   `{"providers":[{"id":"lpr_x","providerId":"x","name":"X","baseURL":"https://x.example/v1","apiKeyEnv":"BURROW_OIDC_CLIENT_SECRET"}]}`,
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

// seedGateway answers the OpenAI-compatible models listing; rows point at
// it through their baseURL.
func seedGateway(t *testing.T, ids ...string) *fakeGateway {
	t.Helper()
	gw := newFakeGateway(t)
	gw.wantKey = "sk-test-value"
	gw.add(ids...)
	return gw
}

func gatewayBase(gw *fakeGateway) string { return gw.URL() + "/v1" }

func seedRow(t *testing.T, st *store.Store, p store.ProviderRow) {
	t.Helper()
	if p.ProviderID == "" {
		p.ProviderID = "litellm"
	}
	if p.Name == "" {
		p.Name = p.ID
	}
	if err := st.CreateProvider(context.Background(), p, "test"); err != nil {
		t.Fatalf("seed %s: %v", p.ID, err)
	}
}

func litellmRow(baseURL string) store.ProviderRow {
	return store.ProviderRow{
		ID: "lpr_litellm", ProviderID: "litellm", Name: "Stump LiteLLM",
		BaseURL: baseURL, APIKeyEnv: "LITELLM_API_KEY",
		NameOverrides: map[string]string{"gpt-5": "GPT-5", "claude-opus-5": "Claude Opus 5"},
	}
}

func TestRuntimeRowsAndConnect(t *testing.T) {
	st := storetest.Open(t)
	url := gatewayBase(seedGateway(t, "gpt-5", "claude-opus-5"))
	seedRow(t, st, litellmRow(url))
	runtime := providers.NewRuntime(st, time.Minute)

	t.Setenv("LITELLM_API_KEY", "sk-test-value")
	rows, err := runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
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
	if !row.HasAPIKey {
		t.Fatalf("hasApiKey must reflect the environment")
	}
	if row.ProviderConfig["npm"] != providers.NpmPackage {
		t.Fatalf("providerConfig.npm must be %q, got %#v", providers.NpmPackage, row.ProviderConfig["npm"])
	}
	options, ok := row.ProviderConfig["options"].(map[string]any)
	if !ok || options["baseURL"] != url {
		t.Fatalf("providerConfig.options.baseURL wrong: %#v", row.ProviderConfig)
	}
	if envs, ok := row.ProviderConfig["env"].([]string); !ok || len(envs) != 1 || envs[0] != "LITELLM_API_KEY" {
		t.Fatalf("providerConfig.env must name the key env: %#v", row.ProviderConfig["env"])
	}
	if len(row.Models) != 2 || row.Models[0].ID != "gpt-5" || row.Models[0].Config == nil {
		t.Fatalf("models: %+v", row.Models)
	}
	if row.UpdatedAt == "" {
		t.Fatalf("updatedAt must be set")
	}

	connectRow, key, ok, err := runtime.ConnectFor(context.Background(), "lpr_litellm", nil)
	if err != nil || !ok || key == nil || *key != "sk-test-value" || connectRow.ID != "lpr_litellm" {
		t.Fatalf("connect: %v %v %v", key, connectRow.ID, ok)
	}
	if _, _, ok, err := runtime.ConnectFor(context.Background(), "lpr_missing", nil); ok || err != nil {
		t.Fatalf("unknown provider must not connect: %v %v", ok, err)
	}
}

func TestRuntimeWithoutKeyReportsHasAPIKeyFalse(t *testing.T) {
	st := storetest.Open(t)
	gw := seedGateway(t, "gpt-5")
	seedRow(t, st, litellmRow(gatewayBase(gw)))
	runtime := providers.NewRuntime(st, time.Minute)

	// Force the key absent even when the host shell carries one.
	t.Setenv("LITELLM_API_KEY", "")
	rows, err := runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	if rows[0].HasAPIKey {
		t.Fatalf("hasApiKey must be false when the env var is unset")
	}
	if len(rows[0].Models) != 0 {
		t.Fatalf("a row with no key serves no models, got %+v", rows[0].Models)
	}
	if gw.count() != 0 {
		t.Fatalf("a row with no key must never call the gateway, hits=%d", gw.count())
	}
	_, key, ok, err := runtime.ConnectFor(context.Background(), "lpr_litellm", nil)
	if err != nil || !ok || key != nil {
		t.Fatalf("connect without a provisioned key must omit the key: %v", key)
	}
}

// A nil runtime (burrow built without the provider surface) serves the
// Stage A empties.
func TestNilRuntimeServesNone(t *testing.T) {
	var runtime *providers.Runtime
	if rows, err := runtime.RowsFor(context.Background(), nil); err != nil || len(rows) != 0 {
		t.Fatalf("nil runtime rows: %+v %v", rows, err)
	}
	if snap, err := runtime.SnapshotFor(context.Background(), nil); err != nil || len(snap) != 0 {
		t.Fatalf("nil runtime snapshot: %+v %v", snap, err)
	}
	if _, _, ok, err := runtime.ConnectFor(context.Background(), "lpr_x", nil); ok || err != nil {
		t.Fatalf("nil runtime must not connect: %v %v", ok, err)
	}
}

// The snapshot carries each visible row's own updatedAt, so one row's menu
// change re-syncs that row alone.
func TestSnapshotCarriesPerRowUpdatedAt(t *testing.T) {
	st := storetest.Open(t)
	url := gatewayBase(seedGateway(t, "gpt-5"))
	seedRow(t, st, litellmRow(url))
	other := store.ProviderRow{ID: "lpr_other", ProviderID: "litellm", Name: "Other",
		BaseURL: url, APIKeyEnv: "LITELLM_API_KEY"}
	seedRow(t, st, other)
	runtime := providers.NewRuntime(st, time.Minute)

	created, err := st.GetProvider(context.Background(), "lpr_other")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := runtime.SnapshotFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap) != 2 {
		t.Fatalf("two rows, snapshot %v", snap)
	}
	if snap["lpr_other"] != providers.FormatUpdatedAt(created.UpdatedAt) {
		t.Fatalf("snapshot must map id→the row's updatedAt: %+v", snap)
	}
}

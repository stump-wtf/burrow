package httpapi_test

// The write gate under the provider allowlists (ADR-0005, #36): one admin
// write used to be enough to make burrow read any environment variable and
// send its value to any http(s) host on the next member poll. Both halves are
// refused here, before the row exists, with the variable an operator has to
// change named in the reason.

import (
	"net/http"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/httpapi"
	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
)

// newAllowlistedAdminEnv opens the admin surface with the given key-variable
// prefixes and gateway hosts, the way BURROW_PROVIDER_API_KEY_ENV_PREFIXES
// and BURROW_PROVIDER_BASE_URL_HOSTS do.
func newAllowlistedAdminEnv(t *testing.T, prefixes, hosts []string) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	e.cfg.AdminGroups = []string{"stumpcloud-admins"}
	pol := providers.NewPolicy(prefixes, hosts)
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st,
		skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}),
		providers.NewRuntime(e.st, e.cfg.ModelsTTL, pol), pol, nil).Routes()
	return e
}

func TestAdminWriteRefusedByProviderAllowlist(t *testing.T) {
	e := newAllowlistedAdminEnv(t, []string{"LITELLM_"}, []string{"gw.example"})
	signInAs(t, e, "stumpcloud-admins")

	cases := []struct {
		name  string
		id    string
		url   string
		env   string
		names string
	}{
		{"key variable", "lpr_env", "https://gw.example/v1", "PGPASSWORD", "BURROW_PROVIDER_API_KEY_ENV_PREFIXES"},
		{"gateway host", "lpr_host", "https://capture.example/v1", "LITELLM_BURROW_KEY", "BURROW_PROVIDER_BASE_URL_HOSTS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers",
				providerBody(tc.id, tc.name, tc.url, tc.env), &body, "", "")
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s write must be refused with 400, got %d: %#v", tc.name, resp.StatusCode, body)
			}
			if got := mapOf(t, body)["error"]; got != "invalid_row" {
				t.Fatalf("error code: %#v", body)
			}
			reason, _ := body["message"].(string)
			if !strings.Contains(reason, tc.names) {
				t.Fatalf("refusal must name %s: %q", tc.names, reason)
			}
			var listed map[string]any
			if resp := e.getJSON(t, "/api/den/v1/admin/llm-providers", &listed); resp.StatusCode != http.StatusOK {
				t.Fatalf("list: %d", resp.StatusCode)
			}
			if rows, _ := listed["llmProviders"].([]any); len(rows) != 0 {
				t.Fatalf("a refused write must not land a row: %#v", listed)
			}
		})
	}

	// What both lists do allow writes, unchanged.
	var created map[string]any
	resp := adminDo(t, e, http.MethodPost, "/api/den/v1/admin/llm-providers",
		providerBody("lpr_ok", "OK", "https://gw.example/v1", "LITELLM_BURROW_KEY"), &created, "", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("an allowlisted write must succeed, got %d: %#v", resp.StatusCode, created)
	}
}

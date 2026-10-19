package providers_test

// The two allowlists a provider row is held to (ADR-0005, #36): what a row
// may name as its key variable, and which host that key may be sent to. Each
// is enforced where a row is written and again where burrow uses it, so a row
// that predates the configuration is covered too.

import (
	"errors"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

func TestPolicyAllowsListedEnvPrefix(t *testing.T) {
	pol := providers.NewPolicy([]string{"litellm_"}, []string{"gw.example"})

	for _, name := range []string{"LITELLM_BURROW_KEY", "litellm_burrow_key", "LITELLM_KEY_FAMILY"} {
		if err := pol.CheckAPIKeyEnv(name); err != nil {
			t.Errorf("apiKeyEnv %q must be allowed: %v", name, err)
		}
	}
	for _, name := range []string{"PGPASSWORD", "LITELLM", "OPENAI_API_KEY", "GW_KEY"} {
		err := pol.CheckAPIKeyEnv(name)
		if err == nil {
			t.Fatalf("apiKeyEnv %q must be refused", name)
		}
		if !errors.Is(err, providers.ErrNotAllowed) {
			t.Errorf("apiKeyEnv refusal must be marked as an allowlist refusal: %v", err)
		}
		if !strings.Contains(err.Error(), "BURROW_PROVIDER_API_KEY_ENV_PREFIXES") {
			t.Errorf("refusal must name the variable to change: %v", err)
		}
	}
}

func TestPolicyHostMatching(t *testing.T) {
	cases := []struct {
		name  string
		hosts []string
		url   string
		ok    bool
	}{
		{"listed host", []string{"gw.example"}, "https://gw.example/v1", true},
		{"default port is implied", []string{"gw.example"}, "https://gw.example:443/v1", true},
		{"configured with its port", []string{"gw.example:443"}, "https://gw.example/v1", true},
		{"uppercase entry", []string{"GW.Example"}, "https://gw.example/v1", true},
		{"typed as a URL", []string{"https://gw.example/"}, "https://gw.example/v1", true},
		{"another host", []string{"gw.example"}, "https://elsewhere.example/v1", false},
		{"a port that is not the default", []string{"gw.example"}, "https://gw.example:8443/v1", false},
		{"a listed port matches", []string{"gw.example:8443"}, "https://gw.example:8443/v1", true},
		{"443 on http is not implied", []string{"gw.example"}, "http://gw.example:443/v1", false},
		{"plain http", []string{"gw.example"}, "http://gw.example/v1", true},
		{"no host configured", nil, "https://gw.example/v1", false},
		{"userinfo", []string{"gw.example"}, "https://u:p@gw.example/v1", false},
		{"query string", []string{"gw.example"}, "https://gw.example/v1?k=v", false},
		{"not a URL", []string{"gw.example"}, "gw.example/v1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := providers.NewPolicy(nil, tc.hosts).CheckBaseURL(tc.url)
			if tc.ok && err != nil {
				t.Fatalf("%s must be allowed: %v", tc.url, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("%s must be refused", tc.url)
				}
				if !errors.Is(err, providers.ErrNotAllowed) {
					t.Errorf("refusal must be marked as an allowlist refusal: %v", err)
				}
			}
		})
	}
}

func TestPolicyErrorNamesHostAndList(t *testing.T) {
	err := providers.NewPolicy([]string{"LITELLM_"}, []string{"gw.example"}).CheckBaseURL("https://evil.example/v1")
	if err == nil {
		t.Fatal("an unlisted host must be refused")
	}
	for _, want := range []string{"evil.example", "gw.example", "BURROW_PROVIDER_BASE_URL_HOSTS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "sk-") {
		t.Errorf("a refusal must not echo anything but the host: %v", err)
	}
}

// Unconstrained is the policy a caller with no provider surface passes; it
// must not be what production ends up with, so NewPolicy with no entries has
// to refuse rather than allow.
func TestUnconstrainedAllowsAndNewPolicyDenies(t *testing.T) {
	if err := providers.Unconstrained().CheckRow("PGPASSWORD", "https://anything.example/v1"); err != nil {
		t.Fatalf("the unconstrained policy must check nothing: %v", err)
	}
	empty := providers.NewPolicy(nil, nil)
	if err := empty.CheckAPIKeyEnv("LITELLM_BURROW_KEY"); err == nil {
		t.Error("a policy built with no prefix must allow none")
	}
	if err := empty.CheckBaseURL("https://gw.example/v1"); err == nil {
		t.Error("a policy built with no host must allow none")
	}
}

func TestValidateStoreRowUnderPolicy(t *testing.T) {
	pol := providers.NewPolicy([]string{"LITELLM_"}, []string{"gw.example"})
	row := store.ProviderRow{
		ID: "lpr_one", ProviderID: "litellm", Name: "One",
		BaseURL: "https://gw.example/v1", APIKeyEnv: "LITELLM_BURROW_KEY",
	}
	if err := providers.ValidateStoreRow(row, pol); err != nil {
		t.Fatalf("an allowlisted row must be accepted: %v", err)
	}

	badEnv := row
	badEnv.APIKeyEnv = "PGPASSWORD"
	if err := providers.ValidateStoreRow(badEnv, pol); !errors.Is(err, providers.ErrNotAllowed) {
		t.Errorf("a write naming an unlisted variable must be refused, got %v", err)
	}

	badHost := row
	badHost.BaseURL = "https://capture.example/v1"
	if err := providers.ValidateStoreRow(badHost, pol); !errors.Is(err, providers.ErrNotAllowed) {
		t.Errorf("a write naming an unlisted host must be refused, got %v", err)
	}

	// The old rule is still there: burrow's own namespace is nameable by no
	// prefix, however the lists are configured.
	burrowNS := row
	burrowNS.APIKeyEnv = "BURROW_DATABASE_URL"
	if err := providers.ValidateStoreRow(burrowNS, providers.NewPolicy([]string{"BURROW_"}, []string{"gw.example"})); err == nil {
		t.Error("a variable in burrow's own namespace must stay refused even when a prefix would allow it")
	}
}

// Package providers loads the org's LLM provider definitions and serves the
// rows the OpenWork desktop client syncs (contract, Stage B).
//
// Governing: ADR-0001 (org LLM providers pointing at LiteLLM), SPEC
// stage-b-org-models. Definitions live in a JSON file named by
// BURROW_PROVIDERS_FILE; API keys are never in that file — each provider
// names an environment variable (OpenBao-fed in deployment) whose value is
// read at request time and only ever served on /connect.
package providers

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Model is one model served through a provider.
type Model struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Config map[string]any `json:"config,omitempty"`
	// Groups narrows this model's audience below its provider's (SPEC-0003
	// REQ "Visibility"). It does not narrow the row's key; see the
	// key-scope warning.
	Groups []string `json:"groups,omitempty"`
}

// Provider is one validated provider definition.
type Provider struct {
	ID         string  `json:"id"`
	ProviderID string  `json:"providerId"`
	Name       string  `json:"name"`
	BaseURL    string  `json:"baseURL"`
	APIKeyEnv  string  `json:"apiKeyEnv"`
	Models     []Model `json:"models"`
	// Groups is the row's audience: members holding any of these OIDC
	// groups. Absent means every member (SPEC-0003 REQ "Visibility").
	Groups []string `json:"groups,omitempty"`
}

// file is the on-disk shape of the providers file.
type file struct {
	Providers []Provider `json:"providers"`
}

var (
	idPrefixRE = regexp.MustCompile(`(?i)^lpr_`)
	envNameRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Load reads and validates the providers file. Validation is deliberately
// strict and fails the whole load: the client's sync drops the entire
// provider list on one bad row (contract, Stage B, "strict rules"), so a bad
// definition must stop burrow at boot rather than poison every poll.
func Load(path string) ([]Provider, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("providers: read %s: %w", path, err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("providers: parse %s: %w", path, err)
	}
	seen := map[string]bool{}
	for i, p := range f.Providers {
		if err := validate(p); err != nil {
			return nil, fmt.Errorf("providers: file %s: provider %d: %w", path, i, err)
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("providers: file %s: duplicate provider id %s", path, p.ID)
		}
		seen[p.ID] = true
	}
	return f.Providers, nil
}

func validate(p Provider) error {
	if !idPrefixRE.MatchString(p.ID) {
		return fmt.Errorf("id %q must match /^lpr_/i", p.ID)
	}
	if strings.TrimSpace(p.ProviderID) == "" {
		return fmt.Errorf("providerId must not be empty")
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("name must not be empty")
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("baseURL %q must be an absolute http(s) URL", p.BaseURL)
	}
	if !envNameRE.MatchString(p.APIKeyEnv) {
		return fmt.Errorf("apiKeyEnv %q must be an environment variable name", p.APIKeyEnv)
	}
	if err := validateGroups(p.Groups); err != nil {
		return err
	}
	for i, m := range p.Models {
		if strings.TrimSpace(m.ID) == "" {
			return fmt.Errorf("model %d: id must not be empty", i)
		}
		if strings.TrimSpace(m.Name) == "" {
			return fmt.Errorf("model %q: name must not be empty", m.ID)
		}
		if err := validateGroups(m.Groups); err != nil {
			return fmt.Errorf("model %q: %w", m.ID, err)
		}
	}
	return nil
}

// validateGroups rejects an explicit empty list and empty entries (SPEC-0003
// REQ "Group declarations"): "visible to nobody" written as [] cannot be told
// apart from a mistake. An absent list (nil) means everyone.
func validateGroups(groups []string) error {
	if groups != nil && len(groups) == 0 {
		return fmt.Errorf("groups must not be an empty list; omit it to mean every member")
	}
	for _, g := range groups {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("groups must not contain an empty name")
		}
	}
	return nil
}

// ModelRow is a model as the client reads it.
type ModelRow struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Config map[string]any `json:"config"`
}

// Row is a provider as the client reads it on /v1/llm-providers. The field
// set is exactly what the client's strict sync requires.
type Row struct {
	ID             string         `json:"id"`
	ProviderID     string         `json:"providerId"`
	Name           string         `json:"name"`
	Source         string         `json:"source"`
	ProviderConfig map[string]any `json:"providerConfig"`
	HasAPIKey      bool           `json:"hasApiKey"`
	Models         []ModelRow     `json:"models"`
	UpdatedAt      string         `json:"updatedAt"`
}

// SourceCustom is the only source burrow serves: "custom" (never "openwork",
// which would make the client set vendor-hosted env vars).
const SourceCustom = "custom"

// NpmPackage is the openai-compatible adapter package the client installs
// for a LiteLLM-style gateway.
const NpmPackage = "@ai-sdk/openai-compatible"

// Runtime serves the loaded provider set. A nil Runtime serves none.
type Runtime struct {
	providers []Provider
	updatedAt time.Time
}

// NewRuntime wraps validated providers with their definition file's mtime.
func NewRuntime(list []Provider, updatedAt time.Time) *Runtime {
	return &Runtime{providers: list, updatedAt: updatedAt}
}

// UpdatedAt is when the definition file last changed.
func (r *Runtime) UpdatedAt() string {
	if r == nil || r.updatedAt.IsZero() {
		return time.Unix(0, 0).UTC().Format(time.RFC3339)
	}
	return r.updatedAt.UTC().Format(time.RFC3339)
}

// lookupKey reads the provider's key from the environment; a set-but-empty
// variable counts as not provisioned.
func lookupKey(env string) (string, bool) {
	value, ok := os.LookupEnv(env)
	if value == "" {
		return "", false
	}
	return value, ok
}

// Row builds the client-facing row for one provider, computing hasApiKey
// from the live environment.
func (r *Runtime) Row(p Provider) Row {
	_, hasKey := lookupKey(p.APIKeyEnv)
	return Row{
		ID:         p.ID,
		ProviderID: p.ProviderID,
		Name:       p.Name,
		Source:     SourceCustom,
		ProviderConfig: map[string]any{
			"npm":     NpmPackage,
			"options": map[string]any{"baseURL": p.BaseURL},
			"env":     []string{p.APIKeyEnv},
		},
		HasAPIKey: hasKey,
		Models:    modelRows(p.Models),
		UpdatedAt: r.UpdatedAt(),
	}
}

func modelRows(models []Model) []ModelRow {
	rows := make([]ModelRow, 0, len(models))
	for _, m := range models {
		config := m.Config
		if config == nil {
			config = map[string]any{}
		}
		rows = append(rows, ModelRow{ID: m.ID, Name: m.Name, Config: config})
	}
	return rows
}

// audience returns the model's effective audience: its own groups, else its
// provider's, else nil (every member).
func audience(p Provider, m Model) []string {
	if m.Groups != nil {
		return m.Groups
	}
	return p.Groups
}

// allowed reports whether a member holding memberGroups may see something
// whose audience is aud. A nil audience is open to every member.
func allowed(aud, memberGroups []string) bool {
	if aud == nil {
		return true
	}
	for _, want := range aud {
		for _, have := range memberGroups {
			if want == have {
				return true
			}
		}
	}
	return false
}

// visible returns p with only the models the member may see, and whether the
// row is served at all: a row is served when at least one model is visible,
// or, having no models, when the provider's own audience allows the member.
func visible(p Provider, memberGroups []string) (Provider, bool) {
	if len(p.Models) == 0 {
		return p, allowed(p.Groups, memberGroups)
	}
	shown := make([]Model, 0, len(p.Models))
	for _, m := range p.Models {
		if allowed(audience(p, m), memberGroups) {
			shown = append(shown, m)
		}
	}
	if len(shown) == 0 {
		return Provider{}, false
	}
	p.Models = shown
	return p, true
}

// UsesGroups reports whether any provider or model declares groups, which is
// when sign-in must request the groups scope (SPEC-0003 REQ "Compatibility").
func (r *Runtime) UsesGroups() bool {
	if r == nil {
		return false
	}
	for _, p := range r.providers {
		if p.Groups != nil {
			return true
		}
		for _, m := range p.Models {
			if m.Groups != nil {
				return true
			}
		}
	}
	return false
}

// MixedAudienceRows returns the ids of rows whose models do not all share one
// audience. Such a row's single key reaches every model in it, so the narrower
// models are hidden from listing but not from the key (SPEC-0003 REQ
// "Key-scope warning").
func (r *Runtime) MixedAudienceRows() []string {
	if r == nil {
		return nil
	}
	var ids []string
	for _, p := range r.providers {
		for _, m := range p.Models {
			if !sameGroups(audience(p, m), audience(p, p.Models[0])) {
				ids = append(ids, p.ID)
				break
			}
		}
	}
	return ids
}

func sameGroups(a, b []string) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, g := range a {
		seen[g]++
	}
	for _, g := range b {
		seen[g]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// RowsFor returns the rows a member holding memberGroups may see, each listing
// only its visible models (SPEC-0003 REQ "Visibility").
func (r *Runtime) RowsFor(memberGroups []string) []Row {
	if r == nil {
		return []Row{}
	}
	rows := make([]Row, 0, len(r.providers))
	for _, p := range r.providers {
		if shown, ok := visible(p, memberGroups); ok {
			rows = append(rows, r.Row(shown))
		}
	}
	return rows
}

// SnapshotFor is Snapshot restricted to the rows the member may see
// (SPEC-0003 REQ "Resource snapshot follows visibility").
func (r *Runtime) SnapshotFor(memberGroups []string) map[string]string {
	if r == nil {
		return map[string]string{}
	}
	snapshot := map[string]string{}
	updatedAt := r.UpdatedAt()
	for _, p := range r.providers {
		if _, ok := visible(p, memberGroups); ok {
			snapshot[p.ID] = updatedAt
		}
	}
	return snapshot
}

// ConnectFor is Connect for a member: a row the member cannot see answers
// exactly like an unknown id, so its key is never handed out and its
// existence is not revealed (SPEC-0003 REQ "Connect follows visibility").
func (r *Runtime) ConnectFor(id string, memberGroups []string) (Row, *string, bool) {
	if r == nil {
		return Row{}, nil, false
	}
	for _, p := range r.providers {
		if p.ID != id {
			continue
		}
		shown, ok := visible(p, memberGroups)
		if !ok {
			return Row{}, nil, false
		}
		if key, ok := lookupKey(p.APIKeyEnv); ok {
			return r.Row(shown), &key, true
		}
		return r.Row(shown), nil, true
	}
	return Row{}, nil, false
}

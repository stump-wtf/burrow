// Package providers serves the org's LLM provider rows to the OpenWork
// desktop client (contract, Stage B). Rows live in the Postgres store; each
// row's model menu is derived from the row's own gateway key via
// GET {baseURL}/models, cached, and served stale when the gateway is
// unreachable.
//
// Governing: ADR-0001 (org LLM providers pointing at LiteLLM), ADR-0004
// (gateway-derived model menus, store-backed rows), SPEC-0004. API keys are
// never in the store — each row names an environment variable (OpenBao-fed in
// deployment) whose value is read at request time and only ever served on
// /connect or sent to the gateway as that row's bearer token.
package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// Model is one model listed in the seed file. Derived menus carry no model
// audience: SPEC-0004 supersedes SPEC-0003's model-level groups — a derived
// model's audience is its row's audience (ADR-0004).
type Model struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Config map[string]any `json:"config,omitempty"`
}

// Provider is one validated provider definition as the seed file carries it.
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

// file is the on-disk shape of the seed file. ModelGroups exists only to
// notice model-level groups so the seed can warn about dropping them
// (SPEC-0004 REQ "Rows in the store", scenario "First-boot seed").
type file struct {
	Providers []fileProvider `json:"providers"`
}

type fileProvider struct {
	ID         string      `json:"id"`
	ProviderID string      `json:"providerId"`
	Name       string      `json:"name"`
	BaseURL    string      `json:"baseURL"`
	APIKeyEnv  string      `json:"apiKeyEnv"`
	Models     []fileModel `json:"models"`
	Groups     []string    `json:"groups,omitempty"`
}

type fileModel struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Config map[string]any  `json:"config,omitempty"`
	Groups json.RawMessage `json:"groups,omitempty"`
}

var (
	// idRE is the contract's /^lpr_/i prefix plus burrow's own charset and
	// length. The id is a path segment on /connect, on the admin routes and
	// in the console's links, so anything that would need escaping there —
	// or that reads as a route, like "lpr_a/delete" — is not an id. Spelled
	// out rather than (?i), which would fold non-ASCII letters into [a-z].
	idRE      = regexp.MustCompile(`^[lL][pP][rR]_[A-Za-z0-9_-]{1,64}$`)
	envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// reservedEnvPrefix is burrow's configuration namespace (internal/config),
// which apiKeyEnv may never name.
const reservedEnvPrefix = "BURROW_"

// Load reads and validates the seed file. Validation is deliberately strict
// and fails the whole load: the client's sync drops the entire provider list
// on one bad row (contract, Stage B, "strict rules"), so a bad definition
// must stop burrow at boot rather than poison every poll.
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
	list := make([]Provider, 0, len(f.Providers))
	for i, fp := range f.Providers {
		p := Provider{
			ID: fp.ID, ProviderID: fp.ProviderID, Name: fp.Name,
			BaseURL: fp.BaseURL, APIKeyEnv: fp.APIKeyEnv, Groups: fp.Groups,
		}
		for _, m := range fp.Models {
			p.Models = append(p.Models, Model{ID: m.ID, Name: m.Name, Config: m.Config})
		}
		if err := validate(p); err != nil {
			return nil, fmt.Errorf("providers: file %s: provider %d: %w", path, i, err)
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("providers: file %s: duplicate provider id %s", path, p.ID)
		}
		seen[p.ID] = true
		list = append(list, p)
	}
	return list, nil
}

// modelGroupRows returns the ids of file rows whose models declare groups.
// Those declarations are dropped at seed (ADR-0004); main.go logs the warning.
func modelGroupRows(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	var ids []string
	for _, fp := range f.Providers {
		for _, m := range fp.Models {
			if len(m.Groups) > 0 {
				ids = append(ids, fp.ID)
				break
			}
		}
	}
	return ids, nil
}

// validate checks a file provider against the contract's strict rules.
func validate(p Provider) error {
	if err := validateIdentity(p.ID, p.ProviderID, p.Name, p.BaseURL, p.APIKeyEnv, p.Groups); err != nil {
		return err
	}
	for i, m := range p.Models {
		if strings.TrimSpace(m.ID) == "" {
			return fmt.Errorf("model %d: id must not be empty", i)
		}
		if strings.TrimSpace(m.Name) == "" {
			return fmt.Errorf("model %q: name must not be empty", m.ID)
		}
	}
	return nil
}

// validateIdentity checks the fields every row — file or store — carries.
func validateIdentity(id, providerID, name, baseURL, apiKeyEnv string, groups []string) error {
	if !idRE.MatchString(id) {
		// The id is not echoed: it may be any length and any bytes.
		return fmt.Errorf("id must be lpr_ followed by 1 to 64 letters, digits, '_' or '-'")
	}
	if strings.TrimSpace(providerID) == "" {
		return fmt.Errorf("providerId must not be empty")
	}
	if hasControl(providerID) {
		return fmt.Errorf("providerId must not contain control characters")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name must not be empty")
	}
	if hasControl(name) {
		return fmt.Errorf("name must not contain control characters")
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("baseURL %q must be an absolute http(s) URL", baseURL)
	}
	// The row's key is sent to {baseURL}/models and baseURL itself is served
	// to every member in providerConfig, so it is an origin and a path and
	// nothing else. A query or fragment would swallow "/models" and let the
	// writer aim the key at any path on that host; userinfo is a credential
	// at rest in a row, which is why the refusal does not echo the value.
	if u.User != nil {
		return fmt.Errorf("baseURL must not carry credentials (user:password@host); the key belongs in apiKeyEnv")
	}
	if strings.ContainsAny(baseURL, "?#") {
		return fmt.Errorf("baseURL %q must not carry a query string or fragment", baseURL)
	}
	if !envNameRE.MatchString(apiKeyEnv) {
		return fmt.Errorf("apiKeyEnv %q must be an environment variable name", apiKeyEnv)
	}
	// A row's key is handed to members on /connect and sent to the row's
	// baseURL as a bearer, and rows are now writable at runtime: naming one
	// of burrow's own variables (the database URL, the OIDC client secret)
	// would turn an admin write into reading burrow's credentials.
	if strings.HasPrefix(strings.ToUpper(apiKeyEnv), reservedEnvPrefix) {
		return fmt.Errorf("apiKeyEnv %q is in burrow's own %s namespace; name the provider key's own variable", apiKeyEnv, reservedEnvPrefix)
	}
	return validateGroups(groups)
}

// hasControl reports whether s carries a control character. Postgres TEXT
// cannot hold a NUL, so one that passed validation failed in the store
// instead: a 500 on the admin API, and at boot a seed that stopped after the
// rows before it were committed. The rest (newlines, escapes, DEL, C1) have
// no use in a field a human reads or a group burrow matches.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

// validateStoreRow applies the same validation a file Load performed
// (SPEC-0004 REQ "Rows in the store"): an invalid write is rejected before
// it can touch the served set.
func validateStoreRow(p store.ProviderRow) error {
	if err := validateIdentity(p.ID, p.ProviderID, p.Name, p.BaseURL, p.APIKeyEnv, p.Groups); err != nil {
		return err
	}
	for id, name := range p.NameOverrides {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("nameOverrides: key must not be empty")
		}
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("nameOverrides[%q]: display name must not be empty", id)
		}
	}
	return nil
}

// validateGroups rejects an explicit empty list and empty entries (SPEC-0003
// REQ "Group declarations"): "visible to nobody" written as [] cannot be told
// apart from a mistake. An absent list (nil) means everyone. A name with
// surrounding whitespace is the same mistake in another spelling: groups are
// compared exactly, so " family" is an audience no member holds. It is
// refused, not trimmed, so the writer sees what they sent.
func validateGroups(groups []string) error {
	if groups != nil && len(groups) == 0 {
		return fmt.Errorf("groups must not be an empty list; omit it to mean every member")
	}
	for _, g := range groups {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("groups must not contain an empty name")
		}
		if hasControl(g) {
			return fmt.Errorf("groups must not contain control characters")
		}
		if g != strings.TrimSpace(g) {
			return fmt.Errorf("groups: %q has leading or trailing whitespace and would match no member", g)
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

// FormatUpdatedAt is the one timestamp format for a row's updatedAt: the
// desktop field, the resource snapshot value, and the admin etag (quoted)
// must agree exactly, or If-Match comparisons drift from what rows carry.
func FormatUpdatedAt(t time.Time) string {
	if t.IsZero() {
		t = time.Unix(0, 0)
	}
	return t.UTC().Format(time.RFC3339Nano)
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

// menuEntry is the per-row derived-menu cache (SPEC-0004 REQ "Gateway-derived
// model menus"). known distinguishes "never fetched" from "fetched empty";
// nextTryAt is the failure cooldown (~TTL/4) that keeps a downed gateway
// from being re-queried once per request.
type menuEntry struct {
	mu        sync.Mutex
	source    string // baseURL and apiKeyEnv the cached ids came from
	known     bool
	ids       []string
	fetchedAt time.Time
	nextTryAt time.Time
}

// Runtime serves the store's provider rows with gateway-derived menus. A nil
// Runtime serves none.
type Runtime struct {
	st   *store.Store
	ttl  time.Duration
	http *http.Client

	mu    sync.Mutex
	cache map[string]*menuEntry
}

// NewRuntime builds the store-backed runtime. modelsTTL bounds how long a
// derived menu is trusted before the gateway is asked again.
func NewRuntime(st *store.Store, modelsTTL time.Duration) *Runtime {
	if modelsTTL <= 0 {
		modelsTTL = time.Minute
	}
	return &Runtime{
		st:    st,
		ttl:   modelsTTL,
		http:  &http.Client{Timeout: 10 * time.Second},
		cache: map[string]*menuEntry{},
	}
}

// RowsFor returns the rows a member holding memberGroups may see (SPEC-0003
// REQ "Visibility", unchanged by SPEC-0004 except that a model's audience is
// now always its row's audience).
func (r *Runtime) RowsFor(ctx context.Context, memberGroups []string) ([]Row, error) {
	if r == nil {
		return []Row{}, nil
	}
	rows, err := r.st.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	r.prune(rows)
	out := make([]Row, 0, len(rows))
	for _, p := range rows {
		if !allowed(p.Groups, memberGroups) {
			continue
		}
		out = append(out, r.row(ctx, p))
	}
	return out, nil
}

// MenuFor returns the row's current derived model ids and whether its key is
// provisioned in this process's environment — the admin console's model
// counts and editor view (SPEC-0004 REQ "Admin console"). It runs the same
// cache/TTL path a desktop listing would, so the count is the menu the
// runtime serves. A nil Runtime answers no menu and no key.
func (r *Runtime) MenuFor(ctx context.Context, p store.ProviderRow) ([]string, bool) {
	if r == nil {
		return nil, false
	}
	_, hasKey := lookupKey(p.APIKeyEnv)
	ids, _ := r.menuIDs(ctx, p)
	return ids, hasKey
}

// MenuKnown reports whether the menu MenuFor returns for row id is an answer
// from the row's gateway or the lack of one. It is false until a fetch from
// the row's current baseURL and key has succeeded — a failed or refused
// fetch, the failure cooldown, and an unprovisioned key all leave it false —
// so the console can say "unknown" where it would otherwise print a count of
// zero it never established. A nil Runtime knows no menu.
func (r *Runtime) MenuKnown(id string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	e := r.cache[id]
	r.mu.Unlock()
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.known
}

// SnapshotFor is the resource snapshot for one member: id→updatedAt for the
// rows they may see (SPEC-0003 REQ "Resource snapshot follows visibility").
// Deriving each row's menu is what advances updated_at when the gateway's
// scope changed, so the snapshot must run the same freshness check a listing
// would — otherwise a model added at the gateway would never be noticed by a
// client that only polls /v1/resources.
func (r *Runtime) SnapshotFor(ctx context.Context, memberGroups []string) (map[string]string, error) {
	if r == nil {
		return map[string]string{}, nil
	}
	rows, err := r.st.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	snapshot := map[string]string{}
	for _, p := range rows {
		if !allowed(p.Groups, memberGroups) {
			continue
		}
		_, bumpedAt := r.menuIDs(ctx, p) // may advance updated_at (ADR-0004)
		if !bumpedAt.IsZero() {
			p.UpdatedAt = bumpedAt
		}
		snapshot[p.ID] = FormatUpdatedAt(p.UpdatedAt)
	}
	return snapshot, nil
}

// ConnectFor is /connect for a member: a row the member cannot see answers
// exactly like an unknown id, so its key is never handed out and its
// existence is not revealed (SPEC-0003 REQ "Connect follows visibility").
func (r *Runtime) ConnectFor(ctx context.Context, id string, memberGroups []string) (Row, *string, bool, error) {
	if r == nil {
		return Row{}, nil, false, nil
	}
	p, err := r.st.GetProvider(ctx, id)
	if err != nil {
		if err == store.ErrNotFound {
			return Row{}, nil, false, nil
		}
		return Row{}, nil, false, err
	}
	if !allowed(p.Groups, memberGroups) {
		return Row{}, nil, false, nil
	}
	row := r.row(ctx, p)
	var key *string
	if value, ok := lookupKey(p.APIKeyEnv); ok {
		key = &value
	}
	return row, key, true, nil
}

// row builds the client-facing row for one stored provider, deriving its
// menu and computing hasApiKey from the live environment.
func (r *Runtime) row(ctx context.Context, p store.ProviderRow) Row {
	_, hasKey := lookupKey(p.APIKeyEnv)
	ids, bumpedAt := r.menuIDs(ctx, p)
	if !bumpedAt.IsZero() {
		p.UpdatedAt = bumpedAt
	}
	models := make([]ModelRow, 0, len(ids))
	for _, id := range ids {
		name := id
		if override, ok := p.NameOverrides[id]; ok && override != "" {
			name = override
		}
		models = append(models, ModelRow{ID: id, Name: name, Config: map[string]any{}})
	}
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
		Models:    models,
		UpdatedAt: FormatUpdatedAt(p.UpdatedAt),
	}
}

// entry returns the row's cache entry, creating it on first need.
func (r *Runtime) entry(id string) *menuEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.cache[id]
	if e == nil {
		e = &menuEntry{}
		r.cache[id] = e
	}
	return e
}

// prune drops cache entries for rows that no longer exist, so a deleted row
// cannot pin its menu in memory.
func (r *Runtime) prune(rows []store.ProviderRow) {
	seen := make(map[string]bool, len(rows))
	for _, p := range rows {
		seen[p.ID] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.cache {
		if !seen[id] {
			delete(r.cache, id)
		}
	}
}

// menuIDs returns the row's derived model ids, beside the moment its stored
// updated_at was advanced because the derived set changed (zero when it was
// not). Fresh menus come straight from cache; expired ones from the gateway;
// failures serve last-known ids with a cooldown so a downed gateway is
// retried at most once per TTL/4 (SPEC-0004 REQ "Gateway-derived model
// menus", scenario "Gateway unreachable"). A row whose key is not
// provisioned serves no models and never calls out.
func (r *Runtime) menuIDs(ctx context.Context, p store.ProviderRow) ([]string, time.Time) {
	e := r.entry(p.ID)
	e.mu.Lock()
	defer e.mu.Unlock()

	// A row repointed at another gateway or key starts over: the old ids
	// are another key's scope. The admin write already advanced updated_at,
	// so the first fetch from the new source need not bump it again.
	if source := p.BaseURL + "\x00" + p.APIKeyEnv; e.source != source {
		e.source, e.known, e.ids, e.fetchedAt, e.nextTryAt = source, false, nil, time.Time{}, time.Time{}
	}

	now := time.Now()
	if now.Before(e.nextTryAt) {
		return e.ids, time.Time{} // cooling down after a failure; serve last-known
	}
	if e.known && now.Before(e.fetchedAt.Add(r.ttl)) {
		return e.ids, time.Time{} // fresh cache: no gateway call
	}
	key, ok := lookupKey(p.APIKeyEnv)
	if !ok {
		return e.ids, time.Time{} // no key: no menu, and nothing to fetch
	}
	ids, err := r.fetch(ctx, p.BaseURL, key)
	if err != nil {
		e.nextTryAt = now.Add(r.ttl / 4)
		if !e.nextTryAt.After(now) {
			e.nextTryAt = now.Add(time.Second)
		}
		return e.ids, time.Time{} // serve stale
	}
	changed := e.known && !sameIDSet(e.ids, ids)
	e.ids, e.known, e.fetchedAt, e.nextTryAt = ids, true, now, time.Time{}
	if !changed {
		return ids, time.Time{}
	}
	// The derived set changed since this process last served it: advance
	// the stored updated_at so resource snapshots trigger a re-sync
	// (ADR-0004). The first fetch after boot never bumps — there is no
	// previously served set to differ from.
	bumpedAt, err := r.st.BumpProviderUpdatedAt(ctx, p.ID)
	if err != nil {
		// The menu is still correct; the snapshot just stays quiet.
		slog.Warn("advance provider updated_at after menu change", "provider", p.ID, "error", err)
		return ids, time.Time{}
	}
	return ids, bumpedAt
}

// fetch asks the gateway what the key may use: the OpenAI-compatible
// GET {baseURL}/models, answered as {"data":[{"id":…}]}.
func (r *Runtime) fetch(ctx context.Context, baseURL, key string) ([]string, error) {
	// Joined as a path element: appended as text, "/models" lands in the
	// query of a baseURL that carries one.
	endpoint, err := url.JoinPath(baseURL, "models")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("providers: gateway %s returned %s", endpoint, resp.Status)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("providers: gateway %s sent unparseable models: %w", endpoint, err)
	}
	ids := make([]string, 0, len(body.Data))
	seen := map[string]bool{}
	for _, m := range body.Data {
		if m.ID != "" && !seen[m.ID] {
			seen[m.ID] = true
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

func sameIDSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	count := make(map[string]int, len(a))
	for _, id := range a {
		count[id]++
	}
	for _, id := range b {
		count[id]--
		if count[id] < 0 {
			return false
		}
	}
	return true
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

// ValidateStoreRow is the exported write gate for the admin API: the same
// validation a file Load applied, on one candidate row.
func ValidateStoreRow(p store.ProviderRow) error {
	return validateStoreRow(p)
}

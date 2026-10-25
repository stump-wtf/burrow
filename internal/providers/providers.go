// Package providers serves the org's LLM provider rows to the OpenWork
// desktop client (contract, Stage B). Rows live in the Postgres store; each
// row's model menu is derived from the row's own gateway key via
// GET {baseURL}/models, cached, refreshed behind the requests that read it,
// and served stale when the gateway is unreachable.
//
// Governing: ADR-0001 (org LLM providers pointing at LiteLLM), ADR-0004
// (gateway-derived model menus, store-backed rows), ADR-0005 (what a row may
// name), SPEC-0004. API keys are never in the store — each row names an
// environment variable (OpenBao-fed in deployment) whose value is read at
// request time and only ever served on /connect or sent to the gateway as that
// row's bearer token, and only for a row whose variable and host are both on
// the allowlists in policy.go.
package providers

import (
	"context"
	"encoding/json"
	"errors"
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

// validate checks a file provider against the contract's strict rules. The
// allowlists are not part of it: the seed file is operator-owned config that
// the deployment renders, and a row there that names something outside them
// is a deploy mistake the seed path warns about, not one that stops boot
// before the config that would allow it has converged (ADR-0005: "Seeding").
func validate(p Provider) error {
	if err := validateIdentity(p.ID, p.ProviderID, p.Name, p.BaseURL, p.APIKeyEnv, p.Groups, Unconstrained()); err != nil {
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
func validateIdentity(id, providerID, name, baseURL, apiKeyEnv string, groups []string, pol Policy) error {
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
	// The namespace check only excludes burrow itself: every other variable
	// in the container was still nameable, which is #36. The allowlists turn
	// "name a variable" into "name one of the org's provider keys", and are
	// checked again wherever a row is used (ADR-0005).
	if err := pol.CheckRow(apiKeyEnv, baseURL); err != nil {
		return err
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
// (SPEC-0004 REQ "Rows in the store") under the policy the caller enforces:
// an invalid write is rejected before it can touch the served set.
func validateStoreRow(p store.ProviderRow, pol Policy) error {
	if err := validateIdentity(p.ID, p.ProviderID, p.Name, p.BaseURL, p.APIKeyEnv, p.Groups, pol); err != nil {
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

// keyFor is lookupKey under the allowlist: a variable name that fell outside
// it (or predates it) reads nothing, so the value never becomes a bearer
// token or a /connect payload whatever the row in the store says (ADR-0005).
func (r *Runtime) keyFor(p store.ProviderRow) (string, bool) {
	if err := r.pol.CheckAPIKeyEnv(p.APIKeyEnv); err != nil {
		r.logRefusal(r.entry(p.ID), p, err)
		return "", false
	}
	return lookupKey(p.APIKeyEnv)
}

// logRefusal records an allowlist refusal in the log, at most once per TTL
// per row: a row nobody corrects would otherwise repeat itself on every
// poll. The name, the host and the reason are logged; never a value.
func (r *Runtime) logRefusal(e *menuEntry, p store.ProviderRow, reason error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r.logRefusalLocked(e, p, reason)
}

// logRefusalLocked is logRefusal for a caller already holding e.mu.
func (r *Runtime) logRefusalLocked(e *menuEntry, p store.ProviderRow, reason error) {
	now := time.Now()
	if !e.refusedAt.IsZero() && now.Before(e.refusedAt.Add(r.ttl)) {
		return
	}
	e.refusedAt = now
	slog.Warn("providers: provider row is outside the allowlist; it serves nothing until the row or the configuration is corrected",
		"provider", p.ID, "reason", reason)
}

// menuEntry is the per-row derived-menu cache (SPEC-0004 REQ "Gateway-derived
// model menus"). known distinguishes "never fetched" from "fetched empty";
// nextTryAt is the failure cooldown (~TTL/4) that keeps a downed gateway
// from being re-queried once per request; refusedAt is when the row last
// logged an allowlist refusal, so one nobody corrects says it about once per
// TTL rather than once per poll. mu guards the fields and is never
// held across a gateway call: a hung gateway must not hold up a reader.
type menuEntry struct {
	mu        sync.Mutex
	source    string // baseURL and apiKeyEnv the cached ids came from
	known     bool
	ids       []string
	fetchedAt time.Time
	nextTryAt time.Time
	refusedAt time.Time
	// served is whether ids has gone out to a reader since the row took
	// its current source. An unanswered or failing row serves no models,
	// and that empty menu is as much a served set as a full one.
	served bool
	// inFlight is the row's one gateway call, closed when its answer has
	// been recorded; nil when none is running.
	inFlight chan struct{}
	// bumpedAt is the updated_at the last menu change wrote, for a reader
	// whose copy of the row predates that write.
	bumpedAt time.Time
}

// Runtime serves the store's provider rows with gateway-derived menus. A nil
// Runtime serves none.
type Runtime struct {
	st   *store.Store
	ttl  time.Duration
	http *http.Client
	// pol is what a row may name: the key variable prefixes and the gateway
	// hosts, checked wherever a row is used, not only where it is written
	// (ADR-0005).
	pol Policy
	// coldWait is the longest one request waits for rows that have no
	// answer yet; http's Timeout is what bounds a gateway call.
	coldWait time.Duration
	// calls counts gateway calls in flight, so tests can wait them out.
	calls sync.WaitGroup

	mu    sync.Mutex
	cache map[string]*menuEntry
}

// The desktop client gives every request 12 seconds (contract §3), and a
// listing, the resource snapshot and /connect all read menus. A gateway call
// keeps the whole fetchTimeout, because it runs behind the requests; a
// request waits at most firstFetchWait, once, and only for rows that have
// never had an answer.
const (
	fetchTimeout   = 10 * time.Second
	firstFetchWait = 4 * time.Second
)

// NewRuntime builds the store-backed runtime. modelsTTL bounds how long a
// derived menu is trusted before the gateway is asked again. pol is the
// policy a row is held to: build it from the config with NewPolicy, or pass
// Unconstrained() where there is no surface to police.
func NewRuntime(st *store.Store, modelsTTL time.Duration, pol Policy) *Runtime {
	if modelsTTL <= 0 {
		modelsTTL = time.Minute
	}
	return &Runtime{
		st:       st,
		ttl:      modelsTTL,
		http:     &http.Client{Timeout: fetchTimeout},
		coldWait: firstFetchWait,
		cache:    map[string]*menuEntry{},
		pol:      pol,
	}
}

// RowsFor returns the rows a member holding memberGroups may see (SPEC-0003
// REQ "Visibility"), each listing only the models they may see. A row whose
// models are governed by rules and whose derived menu then leaves nothing
// visible to that member is not served at all; a row with no rules is served as
// SPEC-0004 describes it, whatever its gateway is doing (SPEC-0003 REQ
// "Model audience").
func (r *Runtime) RowsFor(ctx context.Context, memberGroups []string) ([]Row, error) {
	if r == nil {
		return []Row{}, nil
	}
	rows, err := r.st.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	aud, err := r.audiences(ctx)
	if err != nil {
		return nil, err
	}
	r.prune(rows)
	rows = visibleToAudiences(rows, aud, memberGroups)
	r.awaitFirst(ctx, rows)
	out := make([]Row, 0, len(rows))
	for _, p := range rows {
		row, served := r.rowFor(ctx, p, aud, memberGroups)
		if !served {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// audiences reads the declared model audiences (SPEC-0003 REQ "Model
// audience"). It is one query per listing, as the Library's mapping is, so a
// save takes effect on the next request rather than at a sign-in.
func (r *Runtime) audiences(ctx context.Context) (Audiences, error) {
	rules, err := r.st.ListModelAudiences(ctx)
	if err != nil {
		return nil, err
	}
	return AudiencesOf(rules), nil
}

// MenuFor returns the row's current derived model ids and whether its key is
// provisioned in this process's environment — the admin console's model
// counts and editor view (SPEC-0004 REQ "Admin console"). It runs the same
// cache/TTL path a desktop listing would, first-fetch wait included, so the
// count is the menu the runtime serves. A nil Runtime answers no menu and no
// key.
//
// The ids are the caller's own copy: the console sorts them for display, and
// sorting the cache's slice would reorder the menu every member is served,
// outside the entry lock.
func (r *Runtime) MenuFor(ctx context.Context, p store.ProviderRow) ([]string, bool) {
	if r == nil {
		return nil, false
	}
	_, hasKey := r.keyFor(p)
	r.awaitFirst(ctx, []store.ProviderRow{p})
	ids, _ := r.menuIDs(ctx, p)
	return append([]string(nil), ids...), hasKey
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

// CachedMenuCount is the console list's count for row p: the size of its
// derived menu, and true when the row's current gateway and key have
// answered. It reads the cache and nothing else, so it starts no gateway
// call and writes nothing: no row's updated_at can move because an admin
// opened a page. It reports 0 and false when the row has no menu of record
// yet, which the console renders as deriving (SPEC-0004 REQ "Admin console").
// A row repointed at another gateway or key has no menu of record: the cached
// ids are another key's scope. MenuFor is the call that asks the gateway;
// this one only reports what asking already produced. A nil Runtime has
// cached nothing.
func (r *Runtime) CachedMenuCount(p store.ProviderRow) (int, bool) {
	if r == nil {
		return 0, false
	}
	r.mu.Lock()
	e := r.cache[p.ID]
	r.mu.Unlock()
	if e == nil {
		return 0, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.known || p.BaseURL+"\x00"+p.APIKeyEnv != e.source {
		return 0, false
	}
	return len(e.ids), true
}

// KeyProvisioned reports whether row p's key variable is set, under the same
// allowlist a gateway call would apply. It reads no gateway and never a
// value, so a page can tell "key unset" from "menu not fetched yet" without
// asking anything (SPEC-0004: burrow never holds a key). A nil Runtime holds
// no key.
func (r *Runtime) KeyProvisioned(p store.ProviderRow) bool {
	if r == nil {
		return false
	}
	_, ok := r.keyFor(p)
	return ok
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
	aud, err := r.audiences(ctx)
	if err != nil {
		return nil, err
	}
	rows = visibleToAudiences(rows, aud, memberGroups)
	r.awaitFirst(ctx, rows)
	snapshot := map[string]string{}
	for _, p := range rows {
		ids, bumpedAt := r.menuIDs(ctx, p) // starts the refresh that may advance updated_at (ADR-0004)
		if !aud.servesRow(p, memberGroups, ids, r.MenuKnown(p.ID)) {
			continue
		}
		if bumpedAt.After(p.UpdatedAt) {
			p.UpdatedAt = bumpedAt
		}
		snapshot[p.ID] = FormatUpdatedAt(p.UpdatedAt)
	}
	return snapshot, nil
}

// ConnectFor is /connect for a member: a row the member cannot see answers
// exactly like an unknown id, so its key is never handed out and its
// existence is not revealed (SPEC-0003 REQ "Connect follows visibility"). A row
// whose models are governed by rules answers the same way when the menu burrow
// has derived for it leaves the member no visible model: the key reaches every
// model in the row, so it goes only to someone who can see at least one of them
// (SPEC-0003 REQ "Model audience"). Whether the member is entitled at all is
// judged from the rules, never from the gateway. It hands over the key with
// whatever menu is known and never waits on the gateway, not even for a row's
// first fetch (ADR-0004).
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
	aud, err := r.audiences(ctx)
	if err != nil {
		return Row{}, nil, false, err
	}
	if !aud.entitled(p, memberGroups) {
		return Row{}, nil, false, nil
	}
	// Only allowlisted config is handed out: a row whose key variable or host
	// fell outside the allowlist answers like a row the member cannot see, so
	// neither its key nor its existence is revealed (ADR-0005).
	if err := r.pol.CheckRow(p.APIKeyEnv, p.BaseURL); err != nil {
		r.logRefusal(r.entry(p.ID), p, err)
		return Row{}, nil, false, nil
	}
	row, served := r.rowFor(ctx, p, aud, memberGroups)
	if !served {
		return Row{}, nil, false, nil
	}
	var key *string
	if value, ok := r.keyFor(p); ok {
		key = &value
	}
	return row, key, true, nil
}

// rowFor builds the client-facing row as one member holding memberGroups sees
// it, and reports whether they are served the row at all. hasApiKey comes from
// the live environment; models carries the visible part of the menu the cache
// holds now.
func (r *Runtime) rowFor(ctx context.Context, p store.ProviderRow, aud Audiences, memberGroups []string) (Row, bool) {
	_, hasKey := r.keyFor(p)
	ids, bumpedAt := r.menuIDs(ctx, p)
	if !aud.servesRow(p, memberGroups, ids, r.MenuKnown(p.ID)) {
		return Row{}, false
	}
	if bumpedAt.After(p.UpdatedAt) {
		p.UpdatedAt = bumpedAt
	}
	visible := aud.visibleModels(p, ids, memberGroups)
	models := make([]ModelRow, 0, len(visible))
	for _, id := range visible {
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
	}, true
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

// menuIDs returns the row's last-known model ids without waiting on the
// gateway, beside the updated_at the menu's last change wrote (zero when it
// never changed). A menu that is due a gateway call gets one started behind
// this request; until it lands the last-known ids stand, which is nothing
// for a row with no key or no successful fetch (SPEC-0004 REQ
// "Gateway-derived model menus").
func (r *Runtime) menuIDs(ctx context.Context, p store.ProviderRow) ([]string, time.Time) {
	e := r.entry(p.ID)
	e.mu.Lock()
	defer e.mu.Unlock()
	r.refresh(ctx, p, e)
	e.served = true
	return e.ids, e.bumpedAt
}

// awaitFirst starts the gateway call for every row that is due one, then
// waits for the rows that have no answer yet — together, and for coldWait at
// most. Asking them one after another, each for the gateway's full timeout,
// put a listing over two rows of a hung gateway past the client's 12 seconds.
// A caller that gives up stops waiting; the calls carry on for the next one.
func (r *Runtime) awaitFirst(ctx context.Context, rows []store.ProviderRow) {
	var first []<-chan struct{}
	for _, p := range rows {
		e := r.entry(p.ID)
		e.mu.Lock()
		if landed := r.refresh(ctx, p, e); landed != nil {
			first = append(first, landed)
		}
		e.mu.Unlock()
	}
	if len(first) == 0 {
		return
	}
	patience := time.NewTimer(r.coldWait)
	defer patience.Stop()
	for _, landed := range first {
		select {
		case <-landed:
		case <-patience.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// refresh starts the row's gateway call when its menu is due one and none is
// in flight: at most one call per row, however many requests arrive. Fresh
// menus need none; a failure is retried at most once per TTL/4 (scenario
// "Gateway unreachable"); a row whose key is not provisioned never calls
// out. It returns the call's channel only for a row with no answer yet —
// neither a menu nor a recorded failure — the one case worth a request's
// wait. The caller holds e.mu.
func (r *Runtime) refresh(ctx context.Context, p store.ProviderRow, e *menuEntry) <-chan struct{} {
	// The allowlist is re-checked here, and not only at the write, so a row
	// that predates it or outlived its configuration starts no gateway call
	// and keeps no menu: its cached ids belong to a source burrow will not
	// use again (ADR-0005).
	if err := r.pol.CheckRow(p.APIKeyEnv, p.BaseURL); err != nil {
		r.logRefusalLocked(e, p, err)
		source := p.BaseURL + "\x00" + p.APIKeyEnv
		e.source, e.known, e.ids, e.fetchedAt, e.nextTryAt, e.inFlight = source, false, nil, time.Time{}, time.Time{}, nil
		e.served = false
		return nil
	}

	// A row repointed at another gateway or key starts over: the old ids
	// are another key's scope, and so is the answer to a call still in
	// flight. The admin write already advanced updated_at, so the first
	// fetch from the new source need not bump it again — unless the row
	// is served empty before that fetch succeeds.
	if source := p.BaseURL + "\x00" + p.APIKeyEnv; e.source != source {
		e.source, e.known, e.ids, e.fetchedAt, e.nextTryAt, e.inFlight = source, false, nil, time.Time{}, time.Time{}, nil
		e.served = false
	}

	now := time.Now()
	if now.Before(e.nextTryAt) {
		return nil // cooling down after a failure; serve last-known
	}
	if e.known && now.Before(e.fetchedAt.Add(r.ttl)) {
		return nil // fresh cache: no gateway call
	}
	key, ok := lookupKey(p.APIKeyEnv)
	if !ok {
		return nil // no key: no menu, and nothing to fetch
	}
	if e.inFlight == nil {
		e.inFlight = make(chan struct{})
		r.calls.Add(1)
		// The call belongs to the row, not to the request that happened
		// to trigger it. A member who closes the laptop mid-fetch has not
		// made the gateway fail: on the request's own context that
		// cancellation started the cooldown, and every other member got
		// an empty menu until it ran out. The client's own timeout still
		// bounds the call.
		go r.derive(context.WithoutCancel(ctx), p, key, e, e.inFlight)
	}
	if e.known || !e.nextTryAt.IsZero() {
		return nil // a last-known answer stands while the call runs
	}
	return e.inFlight
}

// derive is one gateway call for one row, run behind the requests: it
// records the answer, or the failure and its cooldown, and advances the
// stored updated_at when the derived set changed.
func (r *Runtime) derive(ctx context.Context, p store.ProviderRow, key string, e *menuEntry, landed chan struct{}) {
	defer r.calls.Done()
	ids, err := r.fetch(ctx, p.BaseURL, key)

	e.mu.Lock()
	defer e.mu.Unlock()
	defer close(landed)
	if e.inFlight != landed {
		return // the row was repointed meanwhile: this is another key's answer
	}
	e.inFlight = nil
	// Counted from now, not from when the call started: a gateway that
	// takes its whole timeout to fail would otherwise have spent the
	// cooldown before it began, and be asked again by the next request.
	now := time.Now()
	if err != nil {
		// A failed request has to show in production logs (AGENTS.md):
		// once per failed call, which the cooldown keeps to once per
		// TTL/4 for each row rather than once per request. fetch's
		// errors name the gateway without its credentials, and never
		// the key.
		slog.Warn("providers: model menu fetch failed; serving the last-known menu", "provider", p.ID, "error", err)
		e.nextTryAt = now.Add(r.ttl / 4)
		if !e.nextTryAt.After(now) {
			e.nextTryAt = now.Add(time.Second)
		}
		return // serve stale
	}
	changed := e.served && !sameIDSet(e.ids, ids)
	e.ids, e.known, e.fetchedAt, e.nextTryAt = ids, true, now, time.Time{}
	if !changed {
		return
	}
	// The derived set differs from the one this process last served:
	// advance the stored updated_at so resource snapshots trigger a
	// re-sync (ADR-0004). What was served counts, not what was fetched: a
	// row that went out with no models while its first fetch failed, or
	// was still in flight, has to be re-synced when the menu fills, or the
	// client keeps the empty one. A first fetch that lands before anything
	// was served has no served set to differ from, and never bumps — nor
	// does the first one after a repoint, whose admin write already
	// advanced the stamp. The entry stays locked across the write so no
	// reader pairs the new menu with the old stamp.
	ctx, cancel := context.WithTimeout(ctx, r.http.Timeout)
	defer cancel()
	bumpedAt, err := r.st.BumpProviderUpdatedAt(ctx, p.ID)
	if err != nil {
		// The menu is still correct; the snapshot just stays quiet.
		slog.Warn("advance provider updated_at after menu change", "provider", p.ID, "error", err)
		return
	}
	e.bumpedAt = bumpedAt
}

// fetch asks the gateway what the key may use: the OpenAI-compatible
// GET {baseURL}/models, answered as {"data":[{"id":…}]}. Only a body that
// carries the data array is an answer: {"data":[]} says the key may use
// nothing, while a 200 with an error envelope, {} or null says nothing
// about the key, and reading it as an empty menu would wipe the last-known
// one and re-sync every client to it.
func (r *Runtime) fetch(ctx context.Context, baseURL, key string) ([]string, error) {
	// Checked again here, at the one place a key leaves the process: burrow
	// sends a bearer only to a host the deployment named, whatever the row in
	// the store says (ADR-0005).
	if err := r.pol.CheckBaseURL(baseURL); err != nil {
		return nil, err
	}
	// Joined as a path element: appended as text, "/models" lands in the
	// query of a baseURL that carries one.
	endpoint, err := url.JoinPath(baseURL, "models")
	if err != nil {
		return nil, fmt.Errorf("providers: gateway baseURL is not a URL: %w", withoutURL(err))
	}
	// Every error below is logged, so each names the gateway as where —
	// the endpoint without userinfo or query — and none repeats the URL
	// net/http and net/url quote in theirs.
	where := loggableURL(endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("providers: gateway %s: %w", where, withoutURL(err))
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("providers: gateway %s: %w", where, withoutURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("providers: gateway %s returned %s", where, resp.Status)
	}
	var body struct {
		Data *[]struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("providers: gateway %s sent unparseable models: %w", where, err)
	}
	if body.Data == nil {
		return nil, fmt.Errorf("providers: gateway %s answered %s without a models listing (no data array)", where, resp.Status)
	}
	ids := make([]string, 0, len(*body.Data))
	seen := map[string]bool{}
	for _, m := range *body.Data {
		if m.ID != "" && !seen[m.ID] {
			seen[m.ID] = true
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// loggableURL is a gateway endpoint as it may appear in an error or a log
// line: a baseURL can carry credentials as userinfo or in its query, and
// neither belongs in a log.
func loggableURL(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "(unparseable baseURL)"
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment = nil, "", "", ""
	return u.String()
}

// withoutURL unwraps the *url.Error net/http and net/url return, whose text
// quotes the request URL with only the password masked.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
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
// validation a file Load applied, on one candidate row, under the policy the
// server enforces.
func ValidateStoreRow(p store.ProviderRow, pol Policy) error {
	return validateStoreRow(p, pol)
}

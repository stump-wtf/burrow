package providers_test

// Gateway-derived model menus (SPEC-0004 REQ "Gateway-derived model menus",
// ADR-0004): the cache contract, TTL expiry, stale-on-error with a bounded
// cooldown, the updated_at bump when the derived set changes, and display
// names from name overrides.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store/storetest"
)

// fakeGateway is a stand-in LiteLLM: an OpenAI-compatible GET /models whose
// scope (the ids it reports), availability, and hit count the test controls.
type fakeGateway struct {
	t    *testing.T
	srv  *httptest.Server
	mu   sync.Mutex
	ids  []string
	hits int
	fail bool
	// wantKey, when set, must arrive as the bearer token.
	wantKey string
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{t: t, wantKey: "sk-gateway-secret"}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		g.mu.Lock()
		g.hits++
		fail := g.fail
		ids := append([]string(nil), g.ids...)
		g.mu.Unlock()
		if g.wantKey != "" && r.Header.Get("Authorization") != "Bearer "+g.wantKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		data := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]any{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) URL() string { return g.srv.URL }

func (g *fakeGateway) add(ids ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ids = append(g.ids, ids...)
}

func (g *fakeGateway) setIDs(ids ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ids = append([]string(nil), ids...)
}

func (g *fakeGateway) setFail(fail bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fail = fail
}

func (g *fakeGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits
}

func (g *fakeGateway) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hits = 0
}

func menuIDs(models []providers.ModelRow) string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return strings.Join(ids, ",")
}

func gatewayRow(url string) store.ProviderRow {
	return store.ProviderRow{
		ID: "lpr_gw", ProviderID: "litellm", Name: "GW",
		BaseURL: url + "/v1", APIKeyEnv: "GW_KEY",
	}
}

func TestMenuDerivedFromGatewayWithCacheHit(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	gw.add("gpt-5", "claude-opus-5")
	t.Setenv("GW_KEY", "sk-gateway-secret")
	seedRow(t, st, gatewayRow(gw.URL()))
	runtime := providers.NewRuntime(st, time.Minute)

	for i := 0; i < 3; i++ {
		rows, err := runtime.RowsFor(context.Background(), nil)
		if err != nil {
			t.Fatalf("rows: %v", err)
		}
		if got := menuIDs(rows[0].Models); got != "gpt-5,claude-opus-5" {
			t.Fatalf("derived menu: %s", got)
		}
	}
	if n := gw.count(); n != 1 {
		t.Fatalf("the TTL covers the whole wave: exactly one gateway call expected, got %d", n)
	}
}

func TestMenuRefetchedAfterTTL(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	gw.add("gpt-5")
	t.Setenv("GW_KEY", "sk-gateway-secret")
	seedRow(t, st, gatewayRow(gw.URL()))
	runtime := providers.NewRuntime(st, 60*time.Millisecond)

	if _, err := runtime.RowsFor(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	gw.add("gpt-5-mini")
	if _, err := runtime.RowsFor(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if n := gw.count(); n != 1 {
		t.Fatalf("inside the TTL the cache answers, gateway calls %d", n)
	}
	time.Sleep(80 * time.Millisecond)
	rows, err := runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := menuIDs(rows[0].Models); got != "gpt-5,gpt-5-mini" {
		t.Fatalf("after TTL expiry the new menu must be served, got %s", got)
	}
	if n := gw.count(); n != 2 {
		t.Fatalf("expiry must trigger exactly one refetch, calls %d", n)
	}
}

func TestGatewayFailureServesStaleWithCooldown(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	gw.add("gpt-5")
	t.Setenv("GW_KEY", "sk-gateway-secret")
	seedRow(t, st, gatewayRow(gw.URL()))
	// TTL 80ms → cooldown 20ms.
	runtime := providers.NewRuntime(st, 80*time.Millisecond)

	rows, err := runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	first := rows[0].UpdatedAt

	gw.setFail(true)
	time.Sleep(90 * time.Millisecond) // expire the cache

	// A request wave against the downed gateway: stale menu, and at most
	// one retry for the whole wave inside the cooldown window.
	var last []providers.ModelRow
	for i := 0; i < 5; i++ {
		rows, err := runtime.RowsFor(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		last = rows[0].Models
		if rows[0].UpdatedAt != first {
			t.Fatalf("a failing gateway must not churn updated_at: %v vs %v", rows[0].UpdatedAt, first)
		}
	}
	if got := menuIDs(last); got != "gpt-5" {
		t.Fatalf("the last-known menu must be served while the gateway is down, got %s", got)
	}
	if n := gw.count(); n > 3 {
		// Expected: the first fetch, then one retry after expiry; the
		// cooldown must absorb the rest of the wave even on a slow host.
		t.Fatalf("the cooldown must bound retries, calls %d", n)
	}

	// After the cooldown the gateway is asked again, and recovery serves
	// the fresh set.
	gw.setFail(false)
	gw.add("gpt-5-mini")
	time.Sleep(40 * time.Millisecond)
	rows, err = runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := menuIDs(rows[0].Models); got != "gpt-5,gpt-5-mini" {
		t.Fatalf("after recovery the new menu must be served, got %s", got)
	}
}

func TestMenuChangeBumpsUpdatedAtOnce(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	gw.add("gpt-5")
	t.Setenv("GW_KEY", "sk-gateway-secret")
	seedRow(t, st, gatewayRow(gw.URL()))
	runtime := providers.NewRuntime(st, 60*time.Millisecond)
	ctx := context.Background()

	rows, err := runtime.RowsFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	created, err := st.GetProvider(ctx, "lpr_gw")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].UpdatedAt != providers.FormatUpdatedAt(created.UpdatedAt) {
		t.Fatalf("the first fetch after boot must not bump updated_at: %v vs %v", rows[0].UpdatedAt, created.UpdatedAt)
	}

	// The gateway's scope gains a model (the SPEC scenario: an ordinary
	// LiteLLM edit).
	gw.add("gpt-5-mini")
	time.Sleep(80 * time.Millisecond)
	rows, err = runtime.RowsFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	bumped, err := st.GetProvider(ctx, "lpr_gw")
	if err != nil {
		t.Fatal(err)
	}
	if !bumped.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("a changed derived set must advance the stored updated_at so snapshots re-sync: %v → %v", created.UpdatedAt, bumped.UpdatedAt)
	}
	if rows[0].UpdatedAt != providers.FormatUpdatedAt(bumped.UpdatedAt) {
		t.Fatalf("the row must serve the bumped timestamp: %v vs %v", rows[0].UpdatedAt, bumped.UpdatedAt)
	}

	// Serving the unchanged set again leaves the timestamp alone.
	time.Sleep(80 * time.Millisecond)
	if _, err := runtime.RowsFor(ctx, nil); err != nil {
		t.Fatal(err)
	}
	again, err := st.GetProvider(ctx, "lpr_gw")
	if err != nil {
		t.Fatal(err)
	}
	if !again.UpdatedAt.Equal(bumped.UpdatedAt) {
		t.Fatalf("an unchanged set must not bump again: %v vs %v", again.UpdatedAt, bumped.UpdatedAt)
	}
}

// The resource-snapshot path derives too: a client that only polls
// /v1/resources must still see the bump.
func TestSnapshotPathAlsoDerives(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	gw.add("gpt-5")
	t.Setenv("GW_KEY", "sk-gateway-secret")
	seedRow(t, st, gatewayRow(gw.URL()))
	runtime := providers.NewRuntime(st, 60*time.Millisecond)
	ctx := context.Background()

	snap, err := runtime.SnapshotFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := snap["lpr_gw"]

	gw.add("gpt-5-mini")
	time.Sleep(80 * time.Millisecond)
	snap, err = runtime.SnapshotFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap["lpr_gw"] == before {
		t.Fatalf("the snapshot value must advance when the derived set changes")
	}
}

func TestNameOverridesApplyToDerivedModels(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	gw.add("gpt-5", "anon-model")
	t.Setenv("GW_KEY", "sk-gateway-secret")
	row := gatewayRow(gw.URL())
	row.NameOverrides = map[string]string{"gpt-5": "GPT-5 (org flagship)"}
	seedRow(t, st, row)
	runtime := providers.NewRuntime(st, time.Minute)

	rows, err := runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, m := range rows[0].Models {
		names[m.ID] = m.Name
	}
	if names["gpt-5"] != "GPT-5 (org flagship)" {
		t.Fatalf("the override must be the display name: %+v", names)
	}
	if names["anon-model"] != "anon-model" {
		t.Fatalf("an unlisted id displays as its id: %+v", names)
	}
}

// A gateway that answers with an unparseable body or a wrong shape is a
// fetch failure, not a panic: the stale menu stands.
// The console must be able to tell "the gateway said zero models" from "the
// gateway was never heard from": MenuKnown is false for an unprovisioned
// key, a failed first fetch and its cooldown, and true once the gateway has
// answered — even with an empty menu, and through a later outage that
// serves the last-known ids.
func TestMenuKnownSeparatesUnansweredFromEmpty(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	row := gatewayRow(gw.URL())
	seedRow(t, st, row)
	// TTL 80ms → cooldown 20ms.
	runtime := providers.NewRuntime(st, 80*time.Millisecond)
	ctx := context.Background()
	menu := func() (int, bool) {
		ids, _ := runtime.MenuFor(ctx, row)
		return len(ids), runtime.MenuKnown(row.ID)
	}

	if runtime.MenuKnown(row.ID) {
		t.Fatal("a row never asked about has no known menu")
	}
	if n, known := menu(); n != 0 || known {
		t.Fatalf("an unset key: want 0 ids and unknown, got %d known=%v", n, known)
	}

	t.Setenv("GW_KEY", "sk-gateway-secret")
	gw.setFail(true)
	if n, known := menu(); n != 0 || known {
		t.Fatalf("a failed first fetch: want 0 ids and unknown, got %d known=%v", n, known)
	}
	hits := gw.count()
	if n, known := menu(); n != 0 || known || gw.count() != hits {
		t.Fatalf("inside the cooldown: want 0 ids, unknown, and no gateway call, got %d known=%v calls %d→%d", n, known, hits, gw.count())
	}

	// The gateway answers, with nothing in scope: zero is now a fact.
	gw.setFail(false)
	time.Sleep(40 * time.Millisecond)
	if n, known := menu(); n != 0 || !known {
		t.Fatalf("an answered empty menu: want 0 ids and known, got %d known=%v", n, known)
	}

	// A later outage serves the last-known menu, which is still an answer.
	gw.setIDs("gpt-5")
	time.Sleep(90 * time.Millisecond)
	if n, known := menu(); n != 1 || !known {
		t.Fatalf("a refreshed menu: want 1 id and known, got %d known=%v", n, known)
	}
	gw.setFail(true)
	time.Sleep(90 * time.Millisecond)
	if n, known := menu(); n != 1 || !known {
		t.Fatalf("an outage after an answer: want the last-known id and known, got %d known=%v", n, known)
	}

	var none *providers.Runtime
	if none.MenuKnown(row.ID) {
		t.Fatal("a nil Runtime knows no menu")
	}
}

func TestGarbageGatewayResponseServesStale(t *testing.T) {
	st := storetest.Open(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html>not json</html>`)
	}))
	t.Cleanup(bad.Close)
	t.Setenv("GW_KEY", "sk-gateway-secret")
	seedRow(t, st, gatewayRow(bad.URL))
	runtime := providers.NewRuntime(st, 40*time.Millisecond)

	rows, err := runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Models) != 0 {
		t.Fatalf("a never-succeeded fetch serves no models: %+v", rows)
	}
}

// A row whose key the gateway rejects serves no models — the fetch failed —
// while /connect still answers (burrow never blocks connect on the gateway).
func TestRejectedKeyServesNoModelsButConnects(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	gw.add("gpt-5")
	t.Setenv("GW_KEY", "sk-not-accepted")
	seedRow(t, st, gatewayRow(gw.URL()))
	runtime := providers.NewRuntime(st, time.Minute)
	ctx := context.Background()

	rows, err := runtime.RowsFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows[0].Models) != 0 {
		t.Fatalf("a rejected key must not serve a menu: %+v", rows[0].Models)
	}
	if !rows[0].HasAPIKey {
		t.Fatal("hasApiKey is about the environment, not the gateway's opinion")
	}
	_, key, ok, err := runtime.ConnectFor(ctx, "lpr_gw", nil)
	if err != nil || !ok || key == nil || *key != "sk-not-accepted" {
		t.Fatalf("connect must not be blocked by the gateway: %v %v", key, ok)
	}
}

// Repointing a row at another gateway or key must not keep serving the old
// key's menu for the rest of the TTL: the cache belongs to the row's source,
// not just its id.
func TestMenuFollowsARepointedRow(t *testing.T) {
	st := storetest.Open(t)
	oldGW := newFakeGateway(t)
	oldGW.add("gpt-5")
	newGW := newFakeGateway(t)
	newGW.add("qwen-27b")
	t.Setenv("GW_KEY", "sk-gateway-secret")
	row := gatewayRow(oldGW.URL())
	seedRow(t, st, row)
	runtime := providers.NewRuntime(st, time.Hour)

	rows, err := runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := menuIDs(rows[0].Models); got != "gpt-5" {
		t.Fatalf("old gateway menu: %s", got)
	}

	read, err := st.GetProvider(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.BaseURL = newGW.URL() + "/v1"
	if _, err := st.UpdateProvider(context.Background(), row, read.UpdatedAt, "test"); err != nil {
		t.Fatal(err)
	}
	rows, err = runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := menuIDs(rows[0].Models); got != "qwen-27b" {
		t.Fatalf("a repointed row must serve its new gateway's menu at once, got %s", got)
	}
}

// The other half of a repoint: the same gateway, another key. The menu is the
// key's scope, so a row switched to a second key must stop serving the first
// key's models at once, not when the TTL runs out. Keying the cache on baseURL
// alone passes TestMenuFollowsARepointedRow and fails this.
func TestMenuFollowsARowRepointedByKeyOnly(t *testing.T) {
	st := storetest.Open(t)
	byKey := map[string]string{"sk-old-key": "gpt-5", "sk-new-key": "qwen-27b"}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := byKey[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"data":[{"id":%q}]}`, id)
	}))
	t.Cleanup(gw.Close)
	t.Setenv("GW_KEY", "sk-old-key")
	t.Setenv("GW_KEY_NEW", "sk-new-key")
	row := gatewayRow(gw.URL)
	seedRow(t, st, row)
	runtime := providers.NewRuntime(st, time.Hour)

	rows, err := runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := menuIDs(rows[0].Models); got != "gpt-5" {
		t.Fatalf("old key's menu: %s", got)
	}

	read, err := st.GetProvider(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.APIKeyEnv = "GW_KEY_NEW" // baseURL unchanged
	if _, err := st.UpdateProvider(context.Background(), row, read.UpdatedAt, "test"); err != nil {
		t.Fatal(err)
	}
	rows, err = runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := menuIDs(rows[0].Models); got != "qwen-27b" {
		t.Fatalf("a row repointed at another key must serve that key's menu at once, got %s", got)
	}
}

// "/models" is a path element under baseURL, never text appended to it. A row
// stored before validation refused a query string (or written past the
// validator) must not have the suffix swallowed by that query: the writer of
// the row would then pick the whole request path the row's key is sent to.
func TestMenuFetchAppendsModelsToThePath(t *testing.T) {
	var mu sync.Mutex
	var paths, queries, auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		queries = append(queries, r.URL.RawQuery)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		fmt.Fprint(w, `{"data":[]}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GW_KEY", "sk-gateway-secret")

	cases := []struct{ id, base, wantPath string }{
		{"lpr_plain", "/v1", "/v1/models"},
		{"lpr_slash", "/v1/", "/v1/models"},
		{"lpr_root", "", "/models"},
		{"lpr_query", "/latest/meta-data/iam/security-credentials/role?x=", "/latest/meta-data/iam/security-credentials/role/models"},
	}
	st := storetest.Open(t)
	for _, tc := range cases {
		seedRow(t, st, store.ProviderRow{ID: tc.id, BaseURL: srv.URL + tc.base, APIKeyEnv: "GW_KEY"})
	}
	if _, err := providers.NewRuntime(st, time.Minute).RowsFor(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != len(cases) {
		t.Fatalf("one fetch per row, got %v", paths)
	}
	for i, tc := range cases {
		if paths[i] != tc.wantPath {
			t.Errorf("baseURL %q: the gateway saw path %q query %q (bearer sent: %v), want path %q",
				tc.base, paths[i], queries[i], auths[i] != "", tc.wantPath)
		}
	}
}

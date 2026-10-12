package providers_test

// The menu cache under concurrency and a misbehaving gateway (SPEC-0004 REQ
// "Gateway-derived model menus", ADR-0004 "burrow never blocks /connect on
// the gateway"): what a caller may do with a menu it was handed, what a
// cancelled or hung gateway call may cost the next request, when a filled
// menu advances updated_at, and what counts as an answer at all.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store/storetest"
)

// heldGateway is a gateway the test can freeze: while held it accepts a
// models call and does not answer it — a hung upstream — and once let go it
// answers every call, held or new, with its ids (or its failure status).
type heldGateway struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	ids    []string
	status int
	gate   chan struct{} // open while holding; closed once let go
	held   bool
	hits   int
	open   int // calls being held right now
	peak   int // the most calls ever held at once
}

func newHeldGateway(t *testing.T, ids ...string) *heldGateway {
	t.Helper()
	g := &heldGateway{t: t, ids: ids, status: http.StatusOK, gate: make(chan struct{}), held: true}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.hits++
		g.open++
		if g.open > g.peak {
			g.peak = g.open
		}
		gate := g.gate
		g.mu.Unlock()
		select {
		case <-gate:
		case <-r.Context().Done(): // the caller gave up on us
		}
		g.mu.Lock()
		g.open--
		status := g.status
		ids := append([]string(nil), g.ids...)
		g.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		data := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]any{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	// Let go before closing: Close waits for the calls still being held.
	t.Cleanup(func() {
		g.letGo()
		g.srv.Close()
	})
	return g
}

func (g *heldGateway) URL() string { return g.srv.URL }

func (g *heldGateway) hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.held {
		g.gate, g.held = make(chan struct{}), true
	}
}

func (g *heldGateway) letGo() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held {
		close(g.gate)
		g.held = false
	}
}

func (g *heldGateway) set(status int, ids ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.status, g.ids = status, append([]string(nil), ids...)
}

func (g *heldGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits
}

func (g *heldGateway) peakHeld() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

// awaitHeld blocks until n calls are inside the gateway at once.
func (g *heldGateway) awaitHeld(n int) {
	g.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		open := g.open
		g.mu.Unlock()
		if open >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	g.t.Fatalf("the gateway never held %d call(s)", n)
}

// promptly runs fn and fails the test when it has not returned inside limit:
// the assertion for "this request must not wait on the gateway".
func promptly(t *testing.T, limit time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("%s was still waiting on the gateway after %v", what, limit)
	}
}

// The console sorts the ids MenuFor hands it. When that slice was the cache's
// own, a console render reordered the menu every member is served, without
// the lock, so a listing could read it mid-sort.
func TestMenuForHandsOutItsOwnCopy(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	gw.add("zeta", "alpha", "mid", "beta")
	t.Setenv("GW_KEY", "sk-gateway-secret")
	row := gatewayRow(gw.URL())
	seedRow(t, st, row)
	runtime := providers.NewRuntime(st, time.Minute)
	ctx := context.Background()

	ids, _ := runtime.MenuFor(ctx, row)
	sort.Strings(ids)
	if got := strings.Join(ids, ","); got != "alpha,beta,mid,zeta" {
		t.Fatalf("the caller's sort: %s", got)
	}
	rows, err := runtime.RowsFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := menuIDs(rows[0].Models); got != "zeta,alpha,mid,beta" {
		t.Fatalf("a caller sorting its menu must not reorder the served one, got %s", got)
	}
}

// Console renders and desktop listings overlap in production. Run under
// -race: a caller writing to the slice it was handed must not be writing to
// what a listing is reading.
func TestMenuForCallerMayWriteWhileListingsRead(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	gw.add("zeta", "alpha", "mid", "beta")
	t.Setenv("GW_KEY", "sk-gateway-secret")
	row := gatewayRow(gw.URL())
	seedRow(t, st, row)
	runtime := providers.NewRuntime(st, time.Minute)
	ctx := context.Background()
	if _, err := runtime.RowsFor(ctx, nil); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				ids, _ := runtime.MenuFor(ctx, row)
				for k := range ids {
					ids[k] = "scribbled"
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				rows, err := runtime.RowsFor(ctx, nil)
				if err != nil {
					t.Error(err)
					return
				}
				if got := menuIDs(rows[0].Models); got != "zeta,alpha,mid,beta" {
					t.Errorf("a listing read a menu another caller was rewriting: %s", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// A member whose request is cancelled mid-fetch (a closed laptop, a client
// timeout) must not cost the next member the menu: the cancellation is the
// caller's, not the gateway's, and may not start the failure cooldown.
func TestCancelledCallerDoesNotPoisonTheRow(t *testing.T) {
	st := storetest.Open(t)
	gw := newHeldGateway(t, "gpt-5")
	defer gw.letGo()
	t.Setenv("GW_KEY", "sk-gateway-secret")
	seedRow(t, st, gatewayRow(gw.URL()))
	// An hour's TTL is a fifteen-minute cooldown: a poisoned row stays
	// empty for longer than this test lives.
	runtime := providers.NewRuntime(st, time.Hour)

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		_, _ = runtime.RowsFor(ctxA, nil)
	}()
	gw.awaitHeld(1) // member A's listing is inside the gateway
	cancelA()
	// Leave the cancellation time to reach the gateway call, if it can.
	select {
	case <-doneA:
	case <-time.After(300 * time.Millisecond):
	}
	gw.letGo() // the gateway was healthy all along
	select {
	case <-doneA:
	case <-time.After(5 * time.Second):
		t.Fatal("member A's listing never returned")
	}
	runtime.WaitRefreshes()

	rows, err := runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := menuIDs(rows[0].Models); got != "gpt-5" {
		t.Fatalf("member B must get the healthy gateway's menu after member A's cancellation, got %q (gateway calls %d)", got, gw.count())
	}
}

// /connect hands over the key; the menu on it is a courtesy. It must answer
// while the gateway is hung, for a row that has never been fetched too.
func TestConnectNeverWaitsOnTheGateway(t *testing.T) {
	st := storetest.Open(t)
	gw := newHeldGateway(t, "gpt-5")
	defer gw.letGo()
	t.Setenv("GW_KEY", "sk-gateway-secret")
	seedRow(t, st, gatewayRow(gw.URL()))
	runtime := providers.NewRuntime(st, time.Minute)
	ctx := context.Background()

	var (
		key *string
		ok  bool
		err error
	)
	promptly(t, 2*time.Second, "/connect", func() {
		_, key, ok, err = runtime.ConnectFor(ctx, "lpr_gw", nil)
	})
	if err != nil || !ok || key == nil || *key != "sk-gateway-secret" {
		t.Fatalf("connect must hand over the key while the gateway is hung: %v %v %v", key, ok, err)
	}

	// The call /connect started still lands, for the listings after it.
	gw.letGo()
	runtime.WaitRefreshes()
	rows, err := runtime.RowsFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := menuIDs(rows[0].Models); got != "gpt-5" {
		t.Fatalf("the menu must arrive once the gateway answers, got %q", got)
	}
}

// Two rows on one gateway that accepts and never answers: the desktop client
// gives a request 12 seconds (contract §3), so the rows are asked together
// and the request waits for them once, briefly — not a full gateway timeout
// per row. Once the calls have failed, that failure is the last-known answer
// and nothing waits at all.
func TestHungGatewayCostsARequestOneBoundedWait(t *testing.T) {
	for _, surface := range []string{"listing", "snapshot"} {
		t.Run(surface, func(t *testing.T) {
			st := storetest.Open(t)
			gw := newHeldGateway(t)
			defer gw.letGo()
			t.Setenv("GW_KEY", "sk-gateway-secret")
			for _, id := range []string{"lpr_a", "lpr_b"} {
				row := gatewayRow(gw.URL())
				row.ID = id
				seedRow(t, st, row)
			}
			runtime := providers.NewRuntime(st, time.Minute)
			runtime.SetTimeouts(10*time.Second, 200*time.Millisecond)
			ctx := context.Background()
			request := func() {
				if surface == "snapshot" {
					snap, err := runtime.SnapshotFor(ctx, nil)
					if err != nil || len(snap) != 2 {
						t.Errorf("snapshot: %v %v", snap, err)
					}
					return
				}
				rows, err := runtime.RowsFor(ctx, nil)
				if err != nil || len(rows) != 2 || len(rows[0].Models)+len(rows[1].Models) != 0 {
					t.Errorf("a hung gateway serves both rows with no models: %+v %v", rows, err)
				}
			}

			promptly(t, 3*time.Second, "a "+surface+" over two unanswered rows", request)
			if peak := gw.peakHeld(); peak != 2 {
				t.Fatalf("both rows must be asked at once, the gateway held %d", peak)
			}

			gw.set(http.StatusBadGateway)
			gw.letGo()
			runtime.WaitRefreshes()
			calls := gw.count()
			gw.hold()
			promptly(t, time.Second, "a "+surface+" after the failure was recorded", request)
			runtime.WaitRefreshes()
			if n := gw.count(); n != calls {
				t.Fatalf("inside the cooldown the gateway must not be asked again, calls %d → %d", calls, n)
			}
		})
	}
}

// The cooldown is counted from when the failed call returned. Counted from
// when it started, a gateway that takes its whole timeout to fail has spent
// the cooldown before it begins, and is asked again by the very next request.
func TestCooldownStartsWhenTheFetchReturns(t *testing.T) {
	st := storetest.Open(t)
	gw := newHeldGateway(t)
	defer gw.letGo()
	t.Setenv("GW_KEY", "sk-gateway-secret")
	seedRow(t, st, gatewayRow(gw.URL()))
	// TTL 2s → cooldown 500ms, shorter than the 700ms the call takes to fail.
	runtime := providers.NewRuntime(st, 2*time.Second)
	runtime.SetTimeouts(700*time.Millisecond, 50*time.Millisecond)
	ctx := context.Background()

	if _, err := runtime.RowsFor(ctx, nil); err != nil {
		t.Fatal(err)
	}
	runtime.WaitRefreshes() // the hung call has timed out
	if n := gw.count(); n != 1 {
		t.Fatalf("one call so far, got %d", n)
	}
	if _, err := runtime.RowsFor(ctx, nil); err != nil {
		t.Fatal(err)
	}
	runtime.WaitRefreshes()
	if n := gw.count(); n != 1 {
		t.Fatalf("the request right after a failed call must be inside the cooldown, gateway calls %d", n)
	}
}

// An expired menu is still the last-known answer: it is served at once, and
// exactly one call per row goes to the gateway behind it however many
// requests arrive meanwhile.
func TestExpiredMenuIsServedWhileOneRefreshRuns(t *testing.T) {
	st := storetest.Open(t)
	gw := newHeldGateway(t, "gpt-5")
	gw.letGo()
	t.Setenv("GW_KEY", "sk-gateway-secret")
	seedRow(t, st, gatewayRow(gw.URL()))
	runtime := providers.NewRuntime(st, 60*time.Millisecond)
	ctx := context.Background()

	rows, err := runtime.RowsFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := menuIDs(rows[0].Models); got != "gpt-5" {
		t.Fatalf("first menu: %s", got)
	}

	gw.hold()
	defer gw.letGo()
	gw.set(http.StatusOK, "gpt-5", "gpt-5-mini")
	time.Sleep(80 * time.Millisecond) // expire the cache

	for i := 0; i < 3; i++ {
		promptly(t, 2*time.Second, "a listing over an expired menu", func() {
			rows, err = runtime.RowsFor(ctx, nil)
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := menuIDs(rows[0].Models); got != "gpt-5" {
			t.Fatalf("the last-known menu is served while the refresh runs, got %s", got)
		}
	}
	promptly(t, 2*time.Second, "/connect over an expired menu", func() {
		_, _, _, err = runtime.ConnectFor(ctx, "lpr_gw", nil)
	})
	if err != nil {
		t.Fatal(err)
	}

	gw.letGo()
	runtime.WaitRefreshes()
	if n := gw.count(); n != 2 {
		t.Fatalf("the first fetch and one refresh, however many requests waited behind it: gateway calls %d", n)
	}
	rows, err = runtime.RowsFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := menuIDs(rows[0].Models); got != "gpt-5,gpt-5-mini" {
		t.Fatalf("the refreshed menu is served once it lands, got %s", got)
	}
}

// While the first fetch fails burrow serves models: [] at the row's stamp. A
// client that synced that has to be told when the menu fills, or it keeps
// the empty one: its /v1/resources diff never sees a change.
func TestMenuThatFillsAfterServingEmptyAdvancesUpdatedAt(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	gw.add("gpt-5")
	gw.setFail(true)
	t.Setenv("GW_KEY", "sk-gateway-secret")
	seedRow(t, st, gatewayRow(gw.URL()))
	// TTL 80ms → cooldown 20ms.
	runtime := providers.NewRuntime(st, 80*time.Millisecond)
	ctx := context.Background()

	rows, err := runtime.RowsFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows[0].Models) != 0 {
		t.Fatalf("a failing first fetch serves no models: %+v", rows[0].Models)
	}
	snap, err := runtime.SnapshotFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	served := snap["lpr_gw"]
	if rows[0].UpdatedAt != served {
		t.Fatalf("the empty menu was served at the row's stamp: %s vs %s", rows[0].UpdatedAt, served)
	}
	runtime.WaitRefreshes()

	gw.setFail(false)
	time.Sleep(100 * time.Millisecond) // past the cooldown
	if _, err := runtime.RowsFor(ctx, nil); err != nil {
		t.Fatal(err)
	}
	runtime.WaitRefreshes()
	rows, err = runtime.RowsFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := menuIDs(rows[0].Models); got != "gpt-5" {
		t.Fatalf("the recovered menu: %q", got)
	}
	if rows[0].UpdatedAt == served {
		t.Fatalf("the row went from models: [] to %s at an unchanged updatedAt %s", menuIDs(rows[0].Models), served)
	}
	snap, err = runtime.SnapshotFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap["lpr_gw"] == served {
		t.Fatalf("the snapshot must advance when an empty served menu fills, still %s", served)
	}
}

// The same after a repoint whose new gateway fails first: the admin write's
// stamp went out with models: [], so the first real answer must move it.
func TestRepointWhoseFirstAnswerFailsAdvancesOnRecovery(t *testing.T) {
	st := storetest.Open(t)
	oldGW := newFakeGateway(t)
	oldGW.add("gpt-5")
	newGW := newFakeGateway(t)
	newGW.add("qwen-27b")
	newGW.setFail(true)
	t.Setenv("GW_KEY", "sk-gateway-secret")
	row := gatewayRow(oldGW.URL())
	seedRow(t, st, row)
	runtime := providers.NewRuntime(st, 80*time.Millisecond)
	ctx := context.Background()

	if _, err := runtime.RowsFor(ctx, nil); err != nil {
		t.Fatal(err)
	}
	runtime.WaitRefreshes()
	read, err := st.GetProvider(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.BaseURL = newGW.URL() + "/v1"
	if _, err := st.UpdateProvider(ctx, row, read.UpdatedAt, "test"); err != nil {
		t.Fatal(err)
	}
	rows, err := runtime.RowsFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows[0].Models) != 0 {
		t.Fatalf("the new gateway is failing, so the repointed row serves no models: %+v", rows[0].Models)
	}
	served := rows[0].UpdatedAt
	runtime.WaitRefreshes()

	newGW.setFail(false)
	time.Sleep(100 * time.Millisecond) // past the cooldown
	if _, err := runtime.RowsFor(ctx, nil); err != nil {
		t.Fatal(err)
	}
	runtime.WaitRefreshes()
	rows, err = runtime.RowsFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := menuIDs(rows[0].Models); got != "qwen-27b" {
		t.Fatalf("the new gateway's menu: %q", got)
	}
	if rows[0].UpdatedAt == served {
		t.Fatalf("the repointed row went from models: [] to its menu at an unchanged updatedAt %s", served)
	}
}

// scriptedGateway answers every models call 200 with whatever body the test
// last set: the gateway that is up but not answering the question.
type scriptedGateway struct {
	srv  *httptest.Server
	mu   sync.Mutex
	body string
}

func newScriptedGateway(t *testing.T, body string) *scriptedGateway {
	t.Helper()
	g := &scriptedGateway{body: body}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		body := g.body
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *scriptedGateway) set(body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.body = body
}

// A 200 that carries no data array is not "this key has no models": an error
// envelope, an empty object or null must leave the last-known menu and the
// stamp alone, like any other failed fetch. {"data":[]} is a real answer.
func TestOKWithoutAModelListingIsAFailedFetch(t *testing.T) {
	const listing = `{"object":"list","data":[{"id":"gpt-5","object":"model"}]}`
	refetch := func(t *testing.T, body string) (before, after providers.Row) {
		t.Helper()
		st := storetest.Open(t)
		gw := newScriptedGateway(t, listing)
		t.Setenv("GW_KEY", "sk-gateway-secret")
		seedRow(t, st, gatewayRow(gw.srv.URL))
		runtime := providers.NewRuntime(st, 60*time.Millisecond)
		ctx := context.Background()

		rows, err := runtime.RowsFor(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		before = rows[0]
		if got := menuIDs(before.Models); got != "gpt-5" {
			t.Fatalf("first menu: %q", got)
		}
		gw.set(body)
		time.Sleep(80 * time.Millisecond) // expire the cache
		if _, err := runtime.RowsFor(ctx, nil); err != nil {
			t.Fatal(err)
		}
		runtime.WaitRefreshes()
		rows, err = runtime.RowsFor(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		runtime.WaitRefreshes()
		return before, rows[0]
	}

	for name, body := range map[string]string{
		"error envelope": `{"error":{"message":"database unavailable","type":"internal_error"}}`,
		"empty object":   `{}`,
		"null":           `null`,
		"null data":      `{"object":"list","data":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			before, after := refetch(t, body)
			if got := menuIDs(after.Models); got != "gpt-5" {
				t.Fatalf("a 200 %s must leave the last-known menu standing, got %q", body, got)
			}
			if after.UpdatedAt != before.UpdatedAt {
				t.Fatalf("a 200 %s must not advance updatedAt: %s → %s", body, before.UpdatedAt, after.UpdatedAt)
			}
		})
	}

	t.Run("empty listing", func(t *testing.T) {
		before, after := refetch(t, `{"object":"list","data":[]}`)
		if len(after.Models) != 0 {
			t.Fatalf("an empty data array is the key's real scope, got %q", menuIDs(after.Models))
		}
		if after.UpdatedAt == before.UpdatedAt {
			t.Fatalf("a menu emptied by the gateway is a change, updatedAt stayed %s", before.UpdatedAt)
		}
	})
}

// lockedBuffer is a log sink the test reads while other goroutines write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// warningsFor returns the Warn records that name the provider.
func warningsFor(t *testing.T, logs, provider string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if record["level"] == "WARN" && record["provider"] == provider {
			out = append(out, record)
		}
	}
	return out
}

// A gateway failure has to be visible in production logs (AGENTS.md: a failed
// request shows at Warn): one line per failed call, naming the row and the
// reason — never the key, nor credentials riding in the row's URL.
func TestFailedGatewayFetchIsLoggedAtWarn(t *testing.T) {
	gw := newFakeGateway(t)
	gw.setFail(true)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close() // nothing listens here any more

	withSecrets := func(base string) string {
		return strings.Replace(base, "http://", "http://ops:hunter2@", 1) + "/v1?token=q-secret"
	}
	for name, tc := range map[string]struct {
		baseURL string
		want    string
	}{
		"gateway answers 500":                           {gw.URL() + "/v1", "500"},
		"status behind a credentialed URL":              {withSecrets(gw.URL()), "500"},
		"gateway unreachable behind a credentialed URL": {withSecrets(down.URL), "connect"},
	} {
		t.Run(name, func(t *testing.T) {
			logs := &lockedBuffer{}
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
			t.Cleanup(func() { slog.SetDefault(restore) })

			st := storetest.Open(t)
			t.Setenv("GW_KEY", "sk-gateway-secret")
			seedRow(t, st, store.ProviderRow{
				ID: "lpr_logged", ProviderID: "litellm", Name: "Logged",
				BaseURL: tc.baseURL, APIKeyEnv: "GW_KEY",
			})
			// An hour's TTL: one failed call, then the cooldown.
			runtime := providers.NewRuntime(st, time.Hour)
			ctx := context.Background()
			for i := 0; i < 3; i++ {
				if _, err := runtime.RowsFor(ctx, nil); err != nil {
					t.Fatal(err)
				}
				runtime.WaitRefreshes()
			}

			out := logs.String()
			warnings := warningsFor(t, out, "lpr_logged")
			if len(warnings) != 1 {
				t.Fatalf("one failed gateway call is one Warn line naming the provider, got %d:\n%s", len(warnings), out)
			}
			reason, _ := warnings[0]["error"].(string)
			if !strings.Contains(reason, tc.want) {
				t.Fatalf("the Warn line must carry the reason (want %q): %v", tc.want, warnings[0])
			}
			for _, secret := range []string{"sk-gateway-secret", "hunter2", "ops:", "q-secret"} {
				if strings.Contains(out, secret) {
					t.Fatalf("the log must not carry %q:\n%s", secret, out)
				}
			}
		})
	}
}

package providers_test

// Enforcement at the second place a row is used, not only where it is
// written (ADR-0005, #36): a row that already exists in the store, and which
// no write validated, must not be able to read an environment variable or
// carry a value to a host the deployment never named.

import (
	"context"
	"net/url"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store/storetest"
)

// gatewayHost is the fake gateway's host as an allowlist entry: 127.0.0.1
// beside whatever port the listener landed on.
func gatewayHost(t *testing.T, gw *fakeGateway) string {
	t.Helper()
	u, err := url.Parse(gw.URL())
	if err != nil {
		t.Fatalf("gateway URL: %v", err)
	}
	return u.Host
}

func allowRow(env string) store.ProviderRow {
	return store.ProviderRow{
		ID: "lpr_allow", ProviderID: "litellm", Name: "Allow",
		BaseURL: "https://gw.example/v1", APIKeyEnv: env,
	}
}

// A row that names a variable outside the prefix list reads nothing: no
// gateway call, no key on /connect, no menu, whatever the row says.
func TestRowOutsideEnvPrefixServesNothing(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	gw.add("gpt-5")
	t.Setenv("GW_KEY", "sk-gateway-secret")
	seedRow(t, st, allowRow("GW_KEY"))

	pol := providers.NewPolicy([]string{"LITELLM_"}, []string{"gw.example"})
	runtime := providers.NewRuntime(st, time.Minute, pol)

	rows, err := runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the row is still listed so an admin can see and fix it, got %d rows", len(rows))
	}
	if len(rows[0].Models) != 0 || rows[0].HasAPIKey {
		t.Fatalf("a refused row serves no menu and no key: %#v", rows[0])
	}
	runtime.WaitRefreshes()
	if gw.count() != 0 {
		t.Fatalf("a refused row must not reach the gateway, %d calls", gw.count())
	}

	_, key, found, err := runtime.ConnectFor(context.Background(), "lpr_allow", nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if found || key != nil {
		t.Fatalf("only allowlisted config is handed out, got found=%v key=%v", found, key != nil)
	}

	row, err := st.GetProvider(context.Background(), "lpr_allow")
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if ids, hasKey := runtime.MenuFor(context.Background(), row); len(ids) != 0 || hasKey || runtime.MenuKnown(row.ID) {
		t.Fatalf("the console must see no menu and no key: ids=%v hasKey=%v", ids, hasKey)
	}
}

// The host list is the other half: a key variable the deployment does allow
// still cannot be carried to a host it never named.
func TestRowOutsideHostAllowlistServesNothing(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	gw.add("gpt-5")
	t.Setenv("LITELLM_BURROW_KEY", "sk-gateway-secret")
	row := allowRow("LITELLM_BURROW_KEY")
	row.BaseURL = gw.URL() + "/v1" // the writer's own host, not on the list
	seedRow(t, st, row)

	pol := providers.NewPolicy([]string{"LITELLM_"}, []string{"gw.example"})
	runtime := providers.NewRuntime(st, time.Minute, pol)

	rows, err := runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(rows) != 1 || len(rows[0].Models) != 0 {
		t.Fatalf("a row pointing off the list serves no menu: %#v", rows)
	}
	runtime.WaitRefreshes()
	if gw.count() != 0 {
		t.Fatalf("a refused host must not be dialed, %d calls", gw.count())
	}
	if _, key, found, _ := runtime.ConnectFor(context.Background(), "lpr_allow", nil); found || key != nil {
		t.Fatalf("connect must not hand out config for a refused host, found=%v", found)
	}
}

// The other half of the fix: a row that fits both lists behaves exactly as it
// did before, so closing #36 costs the deployment nothing it already ran.
func TestAllowlistedRowStillServesItsMenu(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	gw.add("gpt-5")
	t.Setenv("LITELLM_BURROW_KEY", "sk-gateway-secret")
	row := allowRow("LITELLM_BURROW_KEY")
	row.BaseURL = gw.URL() + "/v1"
	seedRow(t, st, row)

	runtime := providers.NewRuntime(st, time.Minute,
		providers.NewPolicy([]string{"LITELLM_"}, []string{gatewayHost(t, gw)}))

	rows, err := runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(rows) != 1 || menuIDs(rows[0].Models) != "gpt-5" || !rows[0].HasAPIKey {
		t.Fatalf("allowlisted row must serve its menu: %#v", rows)
	}
	runtime.WaitRefreshes()
	if gw.count() == 0 {
		t.Fatal("an allowlisted row must reach its gateway")
	}
	served, key, found, err := runtime.ConnectFor(context.Background(), "lpr_allow", nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !found || key == nil || *key != "sk-gateway-secret" || served.ID != "lpr_allow" {
		t.Fatalf("connect must hand out the allowlisted row and its key, found=%v key=%v", found, key != nil)
	}
}

// Seeding is the one place an allowlist miss is a deploy mistake rather than
// a write: the file is the deployment's own rendering, and the configuration
// may well converge after the image. So the rows land, loudly logged, and
// serve nothing until one side is corrected.
func TestSeedOutsideAllowlistSeedsButServesNothing(t *testing.T) {
	st := storetest.Open(t)
	pol := providers.NewPolicy([]string{"LITELLM_"}, []string{"gw.example"})
	seeded, _, err := providers.SeedFromFile(context.Background(), st,
		writeProvidersFile(t, seedFile), pol)
	if err != nil {
		t.Fatalf("a row outside the allowlist must still seed: %v", err)
	}
	if seeded != 2 {
		t.Fatalf("seeded %d rows, want 2", seeded)
	}

	runtime := providers.NewRuntime(st, time.Minute, pol)
	rows, err := runtime.RowsFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	for _, row := range rows {
		if len(row.Models) != 0 || row.HasAPIKey {
			t.Fatalf("seeded row %s is outside the allowlist and must serve nothing: %#v", row.ID, row)
		}
	}
}

package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// servedMenu reads one row off the desktop listing: its model ids in served
// order, and its updatedAt.
func servedMenu(t *testing.T, e *testEnv, id string) (string, string) {
	t.Helper()
	var payload any
	if resp := e.getJSON(t, "/api/den/v1/llm-providers", &payload); resp.StatusCode != http.StatusOK {
		t.Fatalf("listing: status %d", resp.StatusCode)
	}
	for _, item := range listOf(t, mapOf(t, payload)["llmProviders"]) {
		row := mapOf(t, item)
		if row["id"] != id {
			continue
		}
		var ids []string
		for _, m := range listOf(t, row["models"]) {
			ids = append(ids, nonEmptyString(t, mapOf(t, m), "id"))
		}
		return strings.Join(ids, " "), nonEmptyString(t, row, "updatedAt")
	}
	t.Fatalf("the listing has no row %s", id)
	return "", ""
}

// The console shows a row's models sorted. That is the console's view: an
// admin opening /admin must not reorder the menu the desktop listing serves
// (the gateway's own order), least of all at an unchanged updatedAt.
func TestConsoleRenderLeavesTheServedMenuAlone(t *testing.T) {
	gw := newFakeLLMGateway(t, map[string][]string{"sk-console-order": {"zeta", "alpha", "mid", "beta"}})
	t.Setenv("LLM_ORDER_KEY", "sk-console-order")
	e := newConsoleEnv(t)
	seedConsoleRow(t, e, store.ProviderRow{
		ID: "lpr_order", ProviderID: "litellm", Name: "Order",
		BaseURL: gw.baseURL(), APIKeyEnv: "LLM_ORDER_KEY",
	})

	before, stamp := servedMenu(t, e, "lpr_order")
	if before != "zeta alpha mid beta" {
		t.Fatalf("the listing serves the gateway's order, got [%s]", before)
	}
	if resp, _ := e.consoleGet(t, "/admin"); resp.StatusCode != http.StatusOK {
		t.Fatalf("console: status %d", resp.StatusCode)
	}
	after, stampAfter := servedMenu(t, e, "lpr_order")
	if after != before {
		t.Fatalf("GET /admin reordered the served menu: [%s] → [%s] (updatedAt %s → %s)", before, after, stamp, stampAfter)
	}
}

// The provider list is a reading of the menu cache, never a reason to call a
// gateway (SPEC-0004 REQ "Admin console", scenario "Console list meets a cold
// menu cache"). An admin opening it costs one store read, moves no row's
// updatedAt, and reads deriving until something that does ask the gateway has
// answered; after that it reads the cached count without asking again.
func TestConsoleListNeverReachesTheGateway(t *testing.T) {
	gw := newFakeLLMGateway(t, map[string][]string{"sk-console-list": {"m1", "m2", "m3"}})
	t.Setenv("LLM_LIST_KEY", "sk-console-list")
	e := newConsoleEnv(t)
	seedConsoleRow(t, e, store.ProviderRow{
		ID: "lpr_list", ProviderID: "litellm", Name: "List",
		BaseURL: gw.baseURL(), APIKeyEnv: "LLM_LIST_KEY",
	})
	stamped, err := e.st.GetProvider(context.Background(), "lpr_list")
	if err != nil {
		t.Fatalf("read the seeded row: %v", err)
	}

	resp, page := e.consoleGet(t, "/admin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("console list: status %d", resp.StatusCode)
	}
	mustContain(t, page, "deriving…")
	mustNotContain(t, page, "<td>0</td>")
	if n := gw.count(); n != 0 {
		t.Fatalf("the console list reached the gateway %d times", n)
	}

	// A member's listing is what asks. The console then reads that answer.
	if menu, _ := servedMenu(t, e, "lpr_list"); menu != "m1 m2 m3" {
		t.Fatalf("the listing serves the row's menu, got [%s]", menu)
	}
	asked := gw.count()
	_, page = e.consoleGet(t, "/admin")
	mustContain(t, page, "<td>3</td>")
	mustNotContain(t, page, "deriving…")
	if n := gw.count(); n != asked {
		t.Fatalf("a warm console list must stay a cache hit: %d calls, was %d", n, asked)
	}

	now, err := e.st.GetProvider(context.Background(), "lpr_list")
	if err != nil {
		t.Fatalf("re-read the row: %v", err)
	}
	if !now.UpdatedAt.Equal(stamped.UpdatedAt) {
		t.Fatalf("opening the console advanced the row's updatedAt: %s to %s", stamped.UpdatedAt, now.UpdatedAt)
	}
}

package httpapi_test

import (
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

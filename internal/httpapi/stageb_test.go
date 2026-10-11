package httpapi_test

// Stage B contract surface over store-backed rows and gateway-derived menus
// (SPEC-0004 REQ "Visibility unchanged": the desktop shape is untouched,
// only the menu's source moved).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/httpapi"
	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// fakeLLMGateway is a stand-in LiteLLM: GET /v1/models answers the OpenAI
// listing scoped to the presented bearer key, which is how a row's own key
// reports its own menu (ADR-0004).
type fakeLLMGateway struct {
	srv   *httptest.Server
	mu    sync.Mutex
	byKey map[string][]string
}

func newFakeLLMGateway(t *testing.T, byKey map[string][]string) *fakeLLMGateway {
	t.Helper()
	g := &fakeLLMGateway{byKey: byKey}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		key := ""
		if h := r.Header.Get("Authorization"); len(h) > len("Bearer ") {
			key = h[len("Bearer "):]
		}
		g.mu.Lock()
		ids, ok := g.byKey[key]
		g.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		data := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]any{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeLLMGateway) baseURL() string { return g.srv.URL + "/v1" }

func (g *fakeLLMGateway) set(key string, ids ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.byKey[key] = ids
}

// seedProviders inserts rows so the test's rows exist before any request.
func seedProviders(t *testing.T, rows ...store.ProviderRow) {
	t.Helper()
	for _, p := range rows {
		if err := e_st(t).CreateProvider(t.Context(), p, "test"); err != nil {
			t.Fatalf("seed %s: %v", p.ID, err)
		}
	}
}

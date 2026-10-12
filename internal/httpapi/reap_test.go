package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// GET /auth/oidc/start is unauthenticated and writes one sign-in state per
// request, so anyone can grow oidc_states at will. The sweep is what bounds
// it: every abandoned state is removed once it has expired.
func TestAbandonedSignInStatesAreReaped(t *testing.T) {
	e := newOIDCTestEnv(t)
	e.cfg.OIDCStateTTL = -time.Second // abandoned and expired as soon as written
	client := e.srv.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }

	const hits = 20
	for i := 0; i < hits; i++ {
		resp, err := client.Get(e.srv.URL + "/auth/oidc/start")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("start status %d", resp.StatusCode)
		}
	}

	purged, err := e.st.PurgeExpired(context.Background())
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if purged.OIDCStates != hits {
		t.Fatalf("%d anonymous sign-in starts must leave %d rows for the sweep to remove, got %d", hits, hits, purged.OIDCStates)
	}
	if again, err := e.st.PurgeExpired(context.Background()); err != nil || again.OIDCStates != 0 {
		t.Fatalf("the swept states must be gone: %+v (err %v)", again, err)
	}
}

// A purge between sign-in and exchange must not cost the member their grant:
// only expired grants are swept, and an expired one that has not been swept
// yet is still refused by the exchange itself.
func TestPurgeLeavesALiveGrantExchangeable(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	live, err := e.st.MintGrant(ctx, aliceSub, e.cfg.GrantTTL)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := e.st.MintGrant(ctx, aliceSub, -time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	var refused any
	resp := e.doJSON(t, http.MethodPost, "/api/den/v1/auth/desktop-handoff/exchange",
		map[string]string{"grant": stale}, &refused, false)
	if resp.StatusCode != http.StatusUnauthorized || mapOf(t, refused)["error"] != "invalid_grant" {
		t.Fatalf("an unswept expired grant must be refused as invalid_grant, got %d %v", resp.StatusCode, refused)
	}

	if _, err := e.st.PurgeExpired(ctx); err != nil {
		t.Fatalf("purge: %v", err)
	}
	resp = e.doJSON(t, http.MethodPost, "/api/den/v1/auth/desktop-handoff/exchange",
		map[string]string{"grant": live}, nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a live grant must still exchange after a purge, got %d", resp.StatusCode)
	}
}

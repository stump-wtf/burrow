package httpapi_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/httpapi"
	"gitea.stump.rocks/stump.wtf/burrow/internal/oidc"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// Governing: SPEC-0003 (org-access-control), carried onto store rows by
// SPEC-0004 — each member sees only the rows their OIDC groups allow, and
// /connect hands out only those keys. A row's menu is what the gateway
// answers for that row's own key (ADR-0004), so this fixture declares models
// per key rather than per row.

// accessKeys is the deployment the spec was written for: one row per
// audience, each with its own key (the enforceable pattern). lpr_mixed names
// no groups, so it is open to every member — under ADR-0004 a derived
// model's audience is its row's audience, so there is no narrowed model
// inside an open row any more.
func accessKeys() map[string][]string {
	return map[string][]string{
		"sk-key_local":  {"qwen-27b", "qwen-flash"},
		"sk-key_family": {"gpt-oss", "glm"},
		"sk-key_owner":  {"gpt-5"},
		"sk-key_mixed":  {"open", "narrow"},
	}
}

func newAccessTestEnv(t *testing.T) *testEnv {
	t.Helper()

	gw := newFakeLLMGateway(t, accessKeys())
	for env, key := range map[string]string{
		"KEY_LOCAL":  "sk-key_local",
		"KEY_FAMILY": "sk-key_family",
		"KEY_OWNER":  "sk-key_owner",
		"KEY_MIXED":  "sk-key_mixed",
	} {
		t.Setenv(env, key)
	}

	e := newTestEnv(t)
	seedProviders(t, e,
		store.ProviderRow{ID: "lpr_local", ProviderID: "litellm", Name: "Local",
			BaseURL: gw.baseURL(), APIKeyEnv: "KEY_LOCAL",
			Groups: []string{"family", "friends"}},
		store.ProviderRow{ID: "lpr_family", ProviderID: "litellm", Name: "Family",
			BaseURL: gw.baseURL(), APIKeyEnv: "KEY_FAMILY",
			Groups: []string{"family"}},
		store.ProviderRow{ID: "lpr_owner", ProviderID: "litellm", Name: "Owner",
			BaseURL: gw.baseURL(), APIKeyEnv: "KEY_OWNER",
			Groups: []string{"owner"}},
		store.ProviderRow{ID: "lpr_mixed", ProviderID: "litellm", Name: "Mixed",
			BaseURL: gw.baseURL(), APIKeyEnv: "KEY_MIXED"})
	return withProviderRuntime(e)
}

// signInAs records alice's groups the way the OIDC callback does at sign-in.
func signInAs(t *testing.T, e *testEnv, groups ...string) {
	t.Helper()
	if err := e.st.UpsertUser(context.Background(), store.User{
		ID: aliceSub, Email: aliceEmail, Name: "Alice", Groups: groups,
	}); err != nil {
		t.Fatalf("record groups: %v", err)
	}
}

// visibleModels returns "row:model" pairs alice is served, sorted.
func visibleModels(t *testing.T, e *testEnv) []string {
	t.Helper()
	var payload any
	if resp := e.getJSON(t, "/api/den/v1/llm-providers", &payload); resp.StatusCode != http.StatusOK {
		t.Fatalf("list providers: status %d", resp.StatusCode)
	}
	var got []string
	for _, r := range listOf(t, mapOf(t, payload)["llmProviders"]) {
		row := mapOf(t, r)
		if _, leaked := row["groups"]; leaked {
			t.Fatalf("group names must not reach the client: %#v", row)
		}
		for _, m := range listOf(t, row["models"]) {
			got = append(got, row["id"].(string)+":"+mapOf(t, m)["id"].(string))
		}
	}
	sort.Strings(got)
	return got
}

func TestAccessRowsFollowGroups(t *testing.T) {
	e := newAccessTestEnv(t)
	cases := []struct {
		name   string
		groups []string
		want   []string
	}{
		{"no groups: only open rows", nil,
			[]string{"lpr_mixed:narrow", "lpr_mixed:open"}},
		{"friends: local plus open", []string{"friends"},
			[]string{"lpr_local:qwen-27b", "lpr_local:qwen-flash", "lpr_mixed:narrow", "lpr_mixed:open"}},
		{"family: local and family", []string{"family"},
			[]string{"lpr_family:glm", "lpr_family:gpt-oss", "lpr_local:qwen-27b", "lpr_local:qwen-flash",
				"lpr_mixed:narrow", "lpr_mixed:open"}},
		{"owner and family: everything", []string{"family", "owner"},
			[]string{"lpr_family:glm", "lpr_family:gpt-oss", "lpr_local:qwen-27b", "lpr_local:qwen-flash",
				"lpr_mixed:narrow", "lpr_mixed:open", "lpr_owner:gpt-5"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			signInAs(t, e, tc.groups...)
			if got := visibleModels(t, e); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("groups %v:\n got %v\nwant %v", tc.groups, got, tc.want)
			}
		})
	}
}

func TestAccessConnectHidesInvisibleRowsAndKeys(t *testing.T) {
	e := newAccessTestEnv(t)
	signInAs(t, e, "friends")

	var unknown, hidden any
	unknownResp := e.getJSON(t, "/api/den/v1/llm-providers/lpr_nope/connect", &unknown)
	hiddenResp := e.getJSON(t, "/api/den/v1/llm-providers/lpr_family/connect", &hidden)
	if hiddenResp.StatusCode != http.StatusNotFound {
		t.Fatalf("a row the member cannot see must 404, got %d", hiddenResp.StatusCode)
	}
	if unknownResp.StatusCode != hiddenResp.StatusCode {
		t.Fatalf("hidden (%d) must answer exactly like unknown (%d)", hiddenResp.StatusCode, unknownResp.StatusCode)
	}
	if mapOf(t, hidden)["error"] != mapOf(t, unknown)["error"] {
		t.Fatalf("hidden and unknown bodies differ: %#v vs %#v", hidden, unknown)
	}

	var visible any
	if resp := e.getJSON(t, "/api/den/v1/llm-providers/lpr_local/connect", &visible); resp.StatusCode != http.StatusOK {
		t.Fatalf("visible row connect: status %d", resp.StatusCode)
	}
	if got := mapOf(t, mapOf(t, visible)["llmProvider"])["apiKey"]; got != "sk-key_local" {
		t.Fatalf("visible row must hand over its own key, got %#v", got)
	}
}

func TestAccessResourceSnapshotFollowsGroups(t *testing.T) {
	e := newAccessTestEnv(t)
	signInAs(t, e, "friends")
	var payload any
	if resp := e.getJSON(t, "/api/den/v1/resources", &payload); resp.StatusCode != http.StatusOK {
		t.Fatalf("resources: status %d", resp.StatusCode)
	}
	snap := mapOf(t, mapOf(t, mapOf(t, payload)["resources"])["llmProviders"])
	var ids []string
	for id := range snap {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "lpr_local,lpr_mixed" {
		t.Fatalf("snapshot must list only visible rows, got %v", ids)
	}
}

// End to end through the OIDC callback: the claim is requested, recorded at
// sign-in, and replaced (not merged) at the next sign-in.
func TestAccessGroupsClaimRecordedAtSignIn(t *testing.T) {
	e := newAccessTestEnv(t)
	issuer := newFakeIssuer(t, "burrow-desktop")
	client, err := oidc.New(context.Background(), issuer.srv.URL, "burrow-desktop", "secret",
		e.srv.URL+"/auth/oidc/callback", true)
	if err != nil {
		t.Fatalf("oidc client against fake issuer: %v", err)
	}
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st, skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}), nil, client).Routes()

	signIn := func(claim any) []string {
		t.Helper()
		issuer.groups = claim
		noRedirect := newBrowser(t)
		start, err := noRedirect.Get(e.srv.URL + "/auth/oidc/start")
		if err != nil {
			t.Fatal(err)
		}
		start.Body.Close()
		loc, err := url.Parse(start.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(" "+loc.Query().Get("scope")+" ", " groups ") {
			t.Fatalf("sign-in must request the groups scope: %q", loc.Query().Get("scope"))
		}
		cb, err := noRedirect.Get(approve(t, loc.String()))
		if err != nil {
			t.Fatal(err)
		}
		cb.Body.Close()
		if cb.StatusCode != http.StatusOK {
			t.Fatalf("callback: status %d", cb.StatusCode)
		}
		u, err := e.st.GetUser(context.Background(), "signer-sub-1")
		if err != nil {
			t.Fatal(err)
		}
		return u.Groups
	}

	if got := signIn([]any{"family", "friends"}); strings.Join(got, ",") != "family,friends" {
		t.Fatalf("array claim: got %v", got)
	}
	if got := signIn("owner"); strings.Join(got, ",") != "owner" {
		t.Fatalf("single-string claim must replace the earlier groups, got %v", got)
	}
	if got := signIn(nil); len(got) != 0 || got == nil {
		t.Fatalf("a missing claim must record no groups (non-nil), got %#v", got)
	}
}

// A sign-in that holds no declared group while rules exist is identity
// provider drift — the claim that stopped arriving, the group renamed or
// recased (stump.wtf/burrow#39). Under the fail-closed default that member's
// Library goes quiet at once, so the sign-in itself must be logged: an admin
// connects the two without waiting for a complaint.
func TestAccessSignInHoldingNoDeclaredGroupIsLogged(t *testing.T) {
	e := newAccessTestEnv(t)
	issuer := newFakeIssuer(t, "burrow-desktop")
	client, err := oidc.New(context.Background(), issuer.srv.URL, "burrow-desktop", "secret",
		e.srv.URL+"/auth/oidc/callback", true)
	if err != nil {
		t.Fatalf("oidc client against fake issuer: %v", err)
	}
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st, skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}), nil, client).Routes()
	if err := e.st.SetSkillPack(context.Background(), "family", []string{"code-review"}); err != nil {
		t.Fatalf("declare pack: %v", err)
	}
	var logs strings.Builder
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	issuer.groups = []any{"drifted"}
	noRedirect := newBrowser(t)
	start, err := noRedirect.Get(e.srv.URL + "/auth/oidc/start")
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	loc, err := url.Parse(start.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	cb, err := noRedirect.Get(approve(t, loc.String()))
	if err != nil {
		t.Fatal(err)
	}
	cb.Body.Close()
	if cb.StatusCode != http.StatusOK {
		t.Fatalf("callback: status %d", cb.StatusCode)
	}
	if !strings.Contains(logs.String(), "sign-in holds no declared group") {
		t.Fatalf("a sign-in the fail-closed default applies to must be logged: %s", logs.String())
	}
}

// The escape hatch: burrow requests the groups scope by default (ADR-0004 —
// row audiences and the admin surface are both group-based), but an issuer
// that rejects unknown scopes is served by turning the request off with
// BURROW_OIDC_GROUPS_SCOPE=false.
func TestAccessGroupsScopeIsOnUnlessDisabled(t *testing.T) {
	t.Run("default: requested", func(t *testing.T) {
		scope := signStartScope(t, true)
		if !strings.Contains(" "+scope+" ", " groups ") {
			t.Fatalf("sign-in must request the groups scope by default: %q", scope)
		}
	})
	t.Run("disabled: not requested", func(t *testing.T) {
		scope := signStartScope(t, false)
		if strings.Contains(scope, "groups") {
			t.Fatalf("the escape hatch must not request groups: %q", scope)
		}
	})
}

// signStartScope returns the scope sign-in asks for with the given
// BURROW_OIDC_GROUPS_SCOPE value.
func signStartScope(t *testing.T, requestGroups bool) string {
	t.Helper()
	e := newTestEnv(t)
	issuer := newFakeIssuer(t, "burrow-desktop")
	client, err := oidc.New(context.Background(), issuer.srv.URL, "burrow-desktop", "secret",
		e.srv.URL+"/auth/oidc/callback", requestGroups)
	if err != nil {
		t.Fatalf("oidc client against fake issuer: %v", err)
	}
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st, skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}), nil, client).Routes()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start, err := noRedirect.Get(e.srv.URL + "/auth/oidc/start")
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	loc, err := url.Parse(start.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc.Query().Get("scope")
}

package httpapi_test

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/httpapi"
	"gitea.stump.rocks/stump.wtf/burrow/internal/oidc"
	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// Governing: SPEC-0003 (org-access-control) — each member sees only the rows
// and models their OIDC groups allow, and /connect hands out only those keys.

// accessProviders is the deployment the spec was written for: one row per
// audience, each with its own key (the enforceable pattern), plus a row with
// one model narrowed below its provider for the model-level rule.
const accessProviders = `{
  "providers": [
    {"id": "lpr_local", "providerId": "litellm", "name": "Local", "baseURL": "https://gw.example/v1",
     "apiKeyEnv": "KEY_LOCAL", "groups": ["family", "friends"],
     "models": [{"id": "qwen-27b", "name": "Qwen 27B"}, {"id": "qwen-flash", "name": "Qwen Flash"}]},
    {"id": "lpr_family", "providerId": "litellm", "name": "Family", "baseURL": "https://gw.example/v1",
     "apiKeyEnv": "KEY_FAMILY", "groups": ["family"],
     "models": [{"id": "gpt-oss", "name": "gpt-oss"}, {"id": "glm", "name": "GLM"}]},
    {"id": "lpr_owner", "providerId": "litellm", "name": "Owner", "baseURL": "https://gw.example/v1",
     "apiKeyEnv": "KEY_OWNER", "groups": ["owner"],
     "models": [{"id": "gpt-5", "name": "GPT-5"}]},
    {"id": "lpr_mixed", "providerId": "litellm", "name": "Mixed", "baseURL": "https://gw.example/v1",
     "apiKeyEnv": "KEY_MIXED",
     "models": [{"id": "open", "name": "Open"}, {"id": "narrow", "name": "Narrow", "groups": ["owner"]}]}
  ]
}`

func newAccessTestEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(accessProviders), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := providers.Load(path)
	if err != nil {
		t.Fatalf("load providers fixture: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"KEY_LOCAL", "KEY_FAMILY", "KEY_OWNER", "KEY_MIXED"} {
		t.Setenv(env, "sk-"+strings.ToLower(env))
	}
	runtime := providers.NewRuntime(list, info.ModTime())
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st,
		skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}), runtime, nil).Routes()
	return e
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
		{"no groups: only open models", nil,
			[]string{"lpr_mixed:open"}},
		{"friends: local only", []string{"friends"},
			[]string{"lpr_local:qwen-27b", "lpr_local:qwen-flash", "lpr_mixed:open"}},
		{"family: local and family", []string{"family"},
			[]string{"lpr_family:glm", "lpr_family:gpt-oss", "lpr_local:qwen-27b", "lpr_local:qwen-flash", "lpr_mixed:open"}},
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
		if !strings.Contains(" "+loc.Query().Get("scope")+" ", " groups ") {
			t.Fatalf("sign-in must request the groups scope when groups are used: %q", loc.Query().Get("scope"))
		}
		cb, err := noRedirect.Get(e.srv.URL + "/auth/oidc/callback?code=c&state=" + url.QueryEscape(loc.Query().Get("state")))
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

// Compatibility: with no groups declared, the scope is not requested.
func TestAccessNoGroupsScopeWhenUnused(t *testing.T) {
	e := newTestEnv(t)
	issuer := newFakeIssuer(t, "burrow-desktop")
	client, err := oidc.New(context.Background(), issuer.srv.URL, "burrow-desktop", "secret",
		e.srv.URL+"/auth/oidc/callback", false)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Config.Handler = httpapi.New(e.cfg, e.st, skills.NewCatalog(skills.Source{Dir: e.cfg.SkillsDir}), nil, client).Routes()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start, err := noRedirect.Get(e.srv.URL + "/auth/oidc/start")
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	loc, _ := url.Parse(start.Header.Get("Location"))
	if strings.Contains(loc.Query().Get("scope"), "groups") {
		t.Fatalf("groups scope must not be requested when nothing uses groups: %q", loc.Query().Get("scope"))
	}
}

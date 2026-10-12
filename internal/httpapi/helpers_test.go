package httpapi_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/config"
	"gitea.stump.rocks/stump.wtf/burrow/internal/httpapi"
	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store/storetest"
)

// testEnv is one burrow server under test: a private Postgres schema, a temp
// skills directory with two skills, and a seeded session for alice.
type testEnv struct {
	t      *testing.T
	srv    *httptest.Server
	cfg    *config.Config
	st     *store.Store
	token  string
	apiURL string // <origin>/api/den
}

const (
	aliceSub   = "alice-sub-1"
	aliceEmail = "alice@example.com"
)

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	skillsDir := t.TempDir()
	writeSkillFixture(t, skillsDir, "code-review", "Code Review", "Review code changes carefully.")
	writeSkillFixture(t, skillsDir, "deploy-helper", "Deploy Helper", "Help deploy services.")

	cfg := &config.Config{
		BaseURL:     "", // derive from request
		OrgName:     "Stump Labs",
		OrgSlug:     "stump-labs",
		SkillsDir:   skillsDir,
		GrantTTL:    5 * time.Minute,
		SessionTTL:  24 * time.Hour,
		MCPTokenTTL: time.Hour,
		// Deployment takes these from config.FromEnv. A zero APITokenTTL
		// would mint an already-expired token, and a zero ModelsTTL would
		// ask the gateway on every request.
		ModelsTTL:    time.Minute,
		APITokenTTL:  90 * 24 * time.Hour,
		OIDCStateTTL: 10 * time.Minute,
	}

	st := storetest.Open(t)

	ctx := context.Background()
	if err := st.UpsertUser(ctx, store.User{ID: aliceSub, Email: aliceEmail, Name: "Alice"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	token, err := st.CreateSession(ctx, aliceSub, cfg.SessionTTL)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(cfg, st, skills.NewCatalog(skills.Source{Dir: skillsDir}), nil, nil).Routes())
	t.Cleanup(srv.Close)

	return &testEnv{
		t:      t,
		srv:    srv,
		cfg:    cfg,
		st:     st,
		token:  token,
		apiURL: srv.URL + "/api/den",
	}
}

func writeSkillFixture(t *testing.T, dir, name, title, description string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("---\ntitle: %s\ndescription: %s\n---\n\nFollow this skill for %s.\n", title, description, name)
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// getJSON performs an authenticated GET and decodes the JSON body.
func (e *testEnv) getJSON(t *testing.T, path string, out any) *http.Response {
	t.Helper()
	return e.doJSON(t, http.MethodGet, path, nil, out, true)
}

func (e *testEnv) doJSON(t *testing.T, method, path string, body any, out any, authed bool) *http.Response {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(encoded))
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, e.srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authed {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode response: %v", method, path, err)
		}
	}
	return resp
}

func mapOf(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON object, got %#v", v)
	}
	return m
}

func listOf(t *testing.T, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a JSON array, got %#v", v)
	}
	return l
}

func nonEmptyString(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok || strings.TrimSpace(v) == "" {
		t.Fatalf("field %q must be a non-empty string in %#v", key, m)
	}
	return v
}

// fakeIssuer is a minimal OIDC provider: discovery, JWKS, and a token
// endpoint that mints RS256 ID tokens with the claims burrow reads.
type fakeIssuer struct {
	srv       *httptest.Server
	key       *rsa.PrivateKey
	clientID  string
	claimsSub func() string
	// groups, when non-nil, is emitted as the ID token's groups claim.
	groups any
}

func newFakeIssuer(t *testing.T, clientID string) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fi := &fakeIssuer{key: key, clientID: clientID}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	fi.srv = srv
	t.Cleanup(srv.Close)

	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSONString(w, fmt.Sprintf(`{
			"issuer": %q,
			"authorization_endpoint": %q,
			"token_endpoint": %q,
			"jwks_uri": %q,
			"response_types_supported": ["code"],
			"subject_types_supported": ["public"],
			"id_token_signing_alg_values_supported": ["RS256"]
		}`, srv.URL, srv.URL+"/authorize", srv.URL+"/token", srv.URL+"/jwks"))
	})

	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		writeJSONString(w, fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":"k1","alg":"RS256","use":"sig","n":%q,"e":"AQAB"}]}`, base64.RawURLEncoding.EncodeToString(key.N.Bytes())))
	})

	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		idToken, err := fi.mintIDToken()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONString(w, fmt.Sprintf(`{"access_token":"at","token_type":"Bearer","expires_in":3600,"id_token":%q}`, idToken))
	})

	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		// The test drives the callback directly; the authorize page only
		// needs to exist for redirects to resolve.
		fmt.Fprint(w, "authorize")
	})

	return fi
}

func (fi *fakeIssuer) mintIDToken() (string, error) {
	now := time.Now()
	sub := "signer-sub-1"
	name := "Signer"
	if fi.claimsSub != nil {
		sub = fi.claimsSub()
	}
	header := `{"alg":"RS256","kid":"k1"}`
	claims := map[string]any{
		"iss":            fi.srv.URL,
		"sub":            sub,
		"aud":            fi.clientID,
		"exp":            now.Add(time.Hour).Unix(),
		"iat":            now.Unix(),
		"email":          "signer@example.com",
		"email_verified": true,
		"name":           name,
	}
	if fi.groups != nil {
		claims["groups"] = fi.groups
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString([]byte(header)) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, fi.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func writeJSONString(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, body)
}

// logCapture is the process log for one test. Handlers write it from the
// server's goroutines while the test reads it, hence the lock.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureLogs swaps the default logger for a buffer at Info — the level
// production runs at — and puts the old one back when the test ends.
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	logs := &logCapture{}
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(restore) })
	return logs
}

// wantLogged fails unless one captured line carries every fragment, and fails
// if any secret appears anywhere in the capture.
func wantLogged(t *testing.T, logs *logCapture, fragments []string, secrets ...string) {
	t.Helper()
	written := logs.String()
	found := false
	for _, line := range strings.Split(written, "\n") {
		all := true
		for _, fragment := range fragments {
			if !strings.Contains(line, fragment) {
				all = false
				break
			}
		}
		if all {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no log line carries %q; captured:\n%s", fragments, written)
	}
	for _, secret := range secrets {
		if strings.Contains(written, secret) {
			t.Fatalf("the log must never carry a credential value (%q found):\n%s", secret, written)
		}
	}
}

// errorCode reads the contract error code out of a JSON error body.
func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var payload any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return nonEmptyString(t, mapOf(t, payload), "error")
}

package skills_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
)

// hermeticGit keeps the developer's and the machine's git configuration out
// of the outcome: no credential helper, no insteadOf rewrite, no prompt on the
// terminal the tests happen to run in.
func hermeticGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

// upstream is a throwaway skills repository standing in for the org's remote.
type upstream struct {
	t   *testing.T
	dir string
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{t: t, dir: t.TempDir()}
	u.git("init", "--initial-branch=main")
	u.git("config", "user.email", "test@example.com")
	u.git("config", "user.name", "Test")
	return u
}

func (u *upstream) git(args ...string) {
	u.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", u.dir}, args...)...).CombinedOutput()
	if err != nil {
		u.t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// commit records the working tree as it stands; extra flags go to git commit.
func (u *upstream) commit(msg string, flags ...string) {
	u.t.Helper()
	u.git("add", "-A")
	u.git(append([]string{"commit", "-m", msg}, flags...)...)
}

// stallDeadline is the git deadline the stalled-remote tests run under: long
// enough for git to reach the transport on a loaded machine, short enough to
// wait out.
const stallDeadline = 2 * time.Second

// stall is the switch on a stallingRemote.
type stall struct {
	t         *testing.T
	marker    string
	heartbeat string
}

func (s *stall) on() {
	s.t.Helper()
	if err := os.WriteFile(s.marker, nil, 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *stall) off() {
	s.t.Helper()
	if err := os.Remove(s.marker); err != nil {
		s.t.Fatal(err)
	}
}

// reached reports whether a git command ever got as far as the stalled
// transport, so a test cannot pass by never exercising the stall.
func (s *stall) reached() bool {
	_, err := os.Stat(s.heartbeat)
	return err == nil
}

// running reports whether the stalled transport helper is still alive, by
// watching the counter it keeps writing. Asking the process table instead
// would count a zombie nobody reaped as alive.
func (s *stall) running() bool {
	before, _ := os.ReadFile(s.heartbeat)
	time.Sleep(500 * time.Millisecond)
	after, _ := os.ReadFile(s.heartbeat)
	return !bytes.Equal(before, after)
}

// stallingRemote returns a GitURL that reaches repo through git's ext
// transport, and a switch. While the switch is on, the transport helper never
// answers — the shape of a remote that accepts the connection and goes quiet —
// and, like git-remote-http, it is a grandchild of the git burrow started, so
// it survives that git being killed and keeps its output pipe open.
func stallingRemote(t *testing.T, repo string) (string, *stall) {
	t.Helper()
	dir := t.TempDir()
	s := &stall{t: t, marker: filepath.Join(dir, "stall"), heartbeat: filepath.Join(dir, "heartbeat")}
	// The loop is bounded so a helper orphaned by a failing run cannot outlive
	// the test binary by much.
	script := fmt.Sprintf(`#!/bin/sh
if [ -e '%s' ]; then
	i=0
	while [ $i -lt 100 ]; do
		echo $i > '%s'
		sleep 0.1
		i=$((i+1))
	done
	exit 1
fi
exec git "$1" "$2"
`, s.marker, s.heartbeat)
	helper := filepath.Join(dir, "transport.sh")
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// ext is off by default; allow it for this test's git only.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.ext.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")
	return "ext::" + helper + " %s " + repo, s
}

// wantSkills compares the loaded names, comma-joined in directory order, the
// way the audience tests spell a catalog.
func wantSkills(t *testing.T, got []skills.Skill, want string) {
	t.Helper()
	if names(got) != want {
		t.Fatalf("want skills %q, got %q", want, names(got))
	}
}

// A remote that accepts the connection and never answers must cost one
// bounded wait, not the Library: the refresh lock is held across the load, so
// on a cold start every Library and MCP request parks behind a hung git.
func TestGitRefreshGivesUpOnAnUnresponsiveRemote(t *testing.T) {
	hermeticGit(t)
	up := newUpstream(t)
	writeSkill(t, up.dir, "code-review", "description: Review code carefully.")
	up.commit("seed")
	url, stalled := stallingRemote(t, up.dir)
	stalled.on()

	src := &skills.Source{GitURL: url, CacheDir: t.TempDir(), GitTimeout: stallDeadline}
	done := make(chan error, 1)
	go func() {
		_, err := src.Load(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a clone that never completed must be an error")
		}
	case <-time.After(stallDeadline + 5*time.Second):
		t.Fatalf("Load still blocked 5s after its %s git deadline", stallDeadline)
	}
	if !stalled.reached() {
		t.Fatal("git never reached the stalled transport; the test proved nothing")
	}
	// Unblocking by abandoning the pipe is not enough: a helper left running
	// per timed-out refresh is a process leak against a remote that stays hung.
	if stalled.running() {
		t.Fatal("the stalled transport helper outlived the refresh that started it")
	}
}

// The refresh serves every member, so the one request that triggered it going
// away must not take it down.
func TestGitRefreshSurvivesACancelledRequest(t *testing.T) {
	hermeticGit(t)
	up := newUpstream(t)
	writeSkill(t, up.dir, "code-review", "description: Review code carefully.")
	up.commit("seed")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := (&skills.Source{GitURL: up.dir, CacheDir: t.TempDir()}).Load(ctx)
	if err != nil {
		t.Fatalf("load under a cancelled request: %v", err)
	}
	wantSkills(t, got, "code-review")
}

// A clone killed part-way leaves a .git directory that is not a checkout. The
// loader must not mistake it for one: the Library has to come up once the
// remote answers, and keep following upstream afterwards.
func TestGitRefreshDoesNotTrustAKilledClone(t *testing.T) {
	hermeticGit(t)
	up := newUpstream(t)
	writeSkill(t, up.dir, "code-review", "description: Review code carefully.")
	up.commit("seed")
	url, stalled := stallingRemote(t, up.dir)
	cache := t.TempDir()
	src := &skills.Source{GitURL: url, CacheDir: cache, GitTimeout: stallDeadline}

	stalled.on()
	if _, err := src.Load(context.Background()); err == nil {
		t.Fatal("a clone that never completed must be an error")
	}
	if !stalled.reached() {
		t.Fatal("git never reached the stalled transport; the test proved nothing")
	}
	left, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("a killed clone must leave nothing in the cache, found %v", left)
	}

	stalled.off()
	got, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("load once the remote answers: %v", err)
	}
	wantSkills(t, got, "code-review")

	writeSkill(t, up.dir, "code-review", "description: Review code twice.")
	writeSkill(t, up.dir, "deploy-helper", "description: Help deploy things.")
	up.commit("edit code-review, add deploy-helper")
	got, err = src.Load(context.Background())
	if err != nil {
		t.Fatalf("load after an upstream commit: %v", err)
	}
	wantSkills(t, got, "code-review,deploy-helper")
	if got[0].Description != "Review code twice." {
		t.Fatalf("the upstream edit never arrived: %q", got[0].Description)
	}
}

// A force-push upstream is how a bad skill gets retracted; the Library must
// follow it rather than freeze on the commit it can no longer fast-forward.
func TestGitRefreshFollowsARewrittenHistory(t *testing.T) {
	hermeticGit(t)
	up := newUpstream(t)
	writeSkill(t, up.dir, "code-review", "description: Review code carefully.")
	up.commit("seed")
	src := &skills.Source{GitURL: up.dir, CacheDir: t.TempDir()}

	got, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	wantSkills(t, got, "code-review")

	if err := os.RemoveAll(filepath.Join(up.dir, "code-review")); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, up.dir, "deploy-helper", "description: Help deploy things.")
	up.commit("rewritten", "--amend")

	got, err = src.Load(context.Background())
	if err != nil {
		t.Fatalf("load after the rewrite: %v", err)
	}
	wantSkills(t, got, "deploy-helper")
}

// An unreachable remote must not cost the Library it already has.
func TestGitRefreshFailureServesTheCachedClone(t *testing.T) {
	hermeticGit(t)
	up := newUpstream(t)
	writeSkill(t, up.dir, "code-review", "description: Review code carefully.")
	up.commit("seed")
	src := &skills.Source{GitURL: up.dir, CacheDir: t.TempDir()}

	if _, err := src.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := os.Rename(up.dir, up.dir+".gone"); err != nil {
		t.Fatal(err)
	}
	got, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("a failed refresh must serve the cached clone, got %v", err)
	}
	wantSkills(t, got, "code-review")
}

// Repointing BURROW_SKILLS_GIT_URL must repoint the Library, even though the
// cache directory still holds the old remote's clone.
func TestGitRefreshFollowsAChangedURL(t *testing.T) {
	hermeticGit(t)
	a := newUpstream(t)
	writeSkill(t, a.dir, "code-review", "description: Review code carefully.")
	a.commit("seed a")
	b := newUpstream(t)
	writeSkill(t, b.dir, "deploy-helper", "description: Help deploy things.")
	b.commit("seed b")
	cache := t.TempDir()

	got, err := (&skills.Source{GitURL: a.dir, CacheDir: cache}).Load(context.Background())
	if err != nil {
		t.Fatalf("load a: %v", err)
	}
	wantSkills(t, got, "code-review")

	got, err = (&skills.Source{GitURL: b.dir, CacheDir: cache}).Load(context.Background())
	if err != nil {
		t.Fatalf("load b: %v", err)
	}
	wantSkills(t, got, "deploy-helper")
}

// The configured URL may carry a credential, and git echoes it: a token in
// the username position comes back in "could not read Password for
// 'http://<token>@host'" when the remote answers 401. Neither the returned
// error nor a log line may repeat it.
func TestGitFailureKeepsTheURLCredentialOutOfErrorsAndLogs(t *testing.T) {
	hermeticGit(t)
	// A made-up value, spelled so the secret scanner does not take it for a
	// real one: it only has to be recognisable in an error or a log line.
	const userinfo = "skills-fixture-user"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="skills"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	// A proxy configured for the machine must not get between git and the
	// test server.
	t.Setenv("NO_PROXY", "127.0.0.1")
	t.Setenv("no_proxy", "127.0.0.1")
	url := strings.Replace(srv.URL, "http://", "http://"+userinfo+"@", 1) + "/skills.git"

	var logs bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	// Cold: the clone fails and the error goes back to the caller, who logs it.
	_, err := (&skills.Source{GitURL: url, CacheDir: t.TempDir()}).Load(context.Background())
	if err == nil {
		t.Fatal("a 401 from the remote must fail the clone")
	}
	if strings.Contains(err.Error(), userinfo) {
		t.Fatalf("the clone error repeats the URL credential: %v", err)
	}

	// Warm: the refresh fails and the loader logs git's output itself.
	up := newUpstream(t)
	writeSkill(t, up.dir, "code-review", "description: Review code carefully.")
	up.commit("seed")
	cache := t.TempDir()
	if _, err := (&skills.Source{GitURL: up.dir, CacheDir: cache}).Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := (&skills.Source{GitURL: url, CacheDir: cache}).Load(context.Background()); err != nil {
		t.Fatalf("a failed refresh must serve the cached clone, got %v", err)
	}
	if !strings.Contains(logs.String(), "serving the cached clone") {
		t.Fatalf("the failed refresh was not logged: %s", logs.String())
	}
	if strings.Contains(logs.String(), userinfo) {
		t.Fatalf("the refresh log repeats the URL credential: %s", logs.String())
	}
}

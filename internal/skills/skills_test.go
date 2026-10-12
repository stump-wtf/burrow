package skills_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
)

func writeSkill(t *testing.T, dir, name, frontmatter string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\n" + frontmatter + "\n---\n\nDo the thing.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDirParsesFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "code-review", "title: Code Review\ndescription: Review code carefully.")
	writeSkill(t, dir, "deploy-helper", "description: Help deploy things.")

	got, err := (&skills.Source{Dir: dir}).Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 skills, got %d: %+v", len(got), got)
	}
	byName := map[string]skills.Skill{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if byName["code-review"].Title != "Code Review" {
		t.Fatalf("title override not applied: %+v", byName["code-review"])
	}
	if byName["deploy-helper"].Title != "deploy-helper" {
		t.Fatalf("title must default to the directory name: %+v", byName["deploy-helper"])
	}
	if byName["code-review"].Description != "Review code carefully." {
		t.Fatalf("description not parsed: %+v", byName["code-review"])
	}
	if byName["code-review"].Body == "" {
		t.Fatalf("body must carry the full SKILL.md source")
	}
}

// A skill without a description, a directory that is not kebab-case, and
// frontmatter that is not YAML are skipped rather than served broken.
func TestLoadDirSkipsInvalidEntries(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "no-description", "title: Broken")
	writeSkill(t, dir, "Not_Kebab", "description: Bad name.")
	writeSkill(t, dir, "not-yaml", "description: [never closed")
	writeSkill(t, dir, "good-one", "description: Fine.")

	got, err := (&skills.Source{Dir: dir}).Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || got[0].Name != "good-one" {
		t.Fatalf("want only good-one, got %+v", got)
	}
}

// Frontmatter is YAML, and skill authors write it as YAML: a description
// folded over several lines, a metadata block with its own keys, Windows line
// endings. Each of these used to produce a wrong catalog entry — a description
// of ">-", a title taken from metadata — or drop the skill outright, and the
// catalog entry is what the agent is shown.
func TestLoadDirDecodesFrontmatterAsYAML(t *testing.T) {
	tests := []struct {
		name            string
		source          string
		wantTitle       string
		wantDescription string
	}{
		{
			name:            "plain",
			source:          "---\ntitle: Code Review\ndescription: Review code carefully.\n---\n\nDo the thing.\n",
			wantTitle:       "Code Review",
			wantDescription: "Review code carefully.",
		},
		{
			name:            "folded",
			source:          "---\ndescription: >-\n  Review code carefully,\n  then review it again.\n---\n\nDo the thing.\n",
			wantTitle:       "folded",
			wantDescription: "Review code carefully, then review it again.",
		},
		{
			name:            "literal",
			source:          "---\ndescription: |\n  Review code carefully.\n  Then review it again.\n---\n\nDo the thing.\n",
			wantTitle:       "literal",
			wantDescription: "Review code carefully.\nThen review it again.",
		},
		{
			name:            "nested",
			source:          "---\ntitle: Code Review\ndescription: Review code carefully.\nmetadata:\n  title: Internal\n  description: Not the skill's description.\n---\n\nDo the thing.\n",
			wantTitle:       "Code Review",
			wantDescription: "Review code carefully.",
		},
		{
			name:            "trailing-quote",
			source:          "---\ndescription: Use when asked to \"review this\"\n---\n\nDo the thing.\n",
			wantTitle:       "trailing-quote",
			wantDescription: `Use when asked to "review this"`,
		},
		{
			name:            "crlf",
			source:          "---\r\ntitle: Code Review\r\ndescription: Review code carefully.\r\n---\r\n\r\nDo the thing.\r\n",
			wantTitle:       "Code Review",
			wantDescription: "Review code carefully.",
		},
	}
	dir := t.TempDir()
	for _, tt := range tests {
		if err := os.MkdirAll(filepath.Join(dir, tt.name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, tt.name, "SKILL.md"), []byte(tt.source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := (&skills.Source{Dir: dir}).Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skill, ok := skills.Find(got, tt.name)
			if !ok {
				t.Fatal("skill was dropped from the catalog")
			}
			if skill.Title != tt.wantTitle {
				t.Errorf("title: want %q, got %q", tt.wantTitle, skill.Title)
			}
			if skill.Description != tt.wantDescription {
				t.Errorf("description: want %q, got %q", tt.wantDescription, skill.Description)
			}
			if skill.Body != tt.source {
				t.Errorf("body must stay the SKILL.md source verbatim, got %q", skill.Body)
			}
		})
	}
}

func TestSearchMatchesTermsCaseInsensitively(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "code-review", "description: Review code carefully.")
	writeSkill(t, dir, "deploy-helper", "description: Help deploy things.")

	cat := skills.NewCatalog(skills.Source{Dir: dir})
	list, err := cat.All(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	hits := skills.Search(list, "REVIEW code")
	if len(hits) != 1 || hits[0].Name != "code-review" {
		t.Fatalf("want code-review, got %+v", hits)
	}

	if all := skills.Search(list, ""); len(all) != 2 {
		t.Fatalf("empty query must return the whole list: %+v", all)
	}
}

func TestFindAndClamp(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "code-review", "description: short")

	cat := skills.NewCatalog(skills.Source{Dir: dir})
	list, err := cat.All(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s, ok := skills.Find(list, "code-review"); !ok || s.Name != "code-review" {
		t.Fatalf("find: %v %+v", ok, s)
	}
	if _, ok := skills.Find(list, "missing"); ok {
		t.Fatalf("missing skill must not be found")
	}
	if got := skills.ClampDescription(string(make([]byte, 2048))); len(got) != skills.MaxDescription {
		t.Fatalf("clamp: got %d", len(got))
	}
	// A byte cap must never split a multi-byte rune.
	multi := strings.Repeat("é", 700) + strings.Repeat("а", 400) // 1400 + 800 bytes
	if got := skills.ClampDescription(multi); !utf8.ValidString(got) || len(got) > skills.MaxDescription {
		t.Fatalf("clamp split a rune or exceeded the cap: valid=%v len=%d", utf8.ValidString(got), len(got))
	}
	_ = time.Now()
}

// The org's skills repo keeps its skills under skills/, with repo plumbing at
// the root. SubDir must point the loader at that directory; without it the
// root scan finds nothing.
func TestLoadFromGitSubDir(t *testing.T) {
	repo := t.TempDir()
	skillDir := filepath.Join(repo, "skills", "code-review")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\ntitle: Code Review\ndescription: Review code carefully.\n---\n\nDo the thing.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(repo, "README.md")
	if err := os.WriteFile(readme, []byte("# skills\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "--initial-branch=main")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test")
	git("add", "-A")
	git("commit", "-m", "seed")

	cache := t.TempDir()
	src := &skills.Source{GitURL: repo, CacheDir: cache, SubDir: "skills"}
	got, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || got[0].Name != "code-review" {
		t.Fatalf("want code-review from the skills/ subdir, got %+v", got)
	}

	// Without SubDir the root scan must not mistake the repo root for a
	// skills directory.
	root, err := (&skills.Source{GitURL: repo, CacheDir: cache}).Load(context.Background())
	if err != nil {
		t.Fatalf("root load: %v", err)
	}
	if len(root) != 0 {
		t.Fatalf("root scan must find no skills, got %+v", root)
	}
}

// A source with no valid skills is still a loaded catalog. The cache used to
// tell "never loaded" from "loaded" by the slice being non-nil, so an empty
// Library was never cached and every request inside the TTL ran git again.
func TestCatalogCachesAnEmptyCatalog(t *testing.T) {
	// The developer's git configuration must not change the outcome.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# skills\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"add", "-A"},
		{"commit", "-m", "seed"},
	} {
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	// From here on `git` is a wrapper that records each invocation before
	// handing over to the real one.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	wrapper := "#!/bin/sh\necho \"$1\" >> '" + calls + "'\nexec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	gitCalls := func() int {
		raw, err := os.ReadFile(calls)
		if err != nil {
			t.Fatalf("git was never run through the wrapper: %v", err)
		}
		return strings.Count(string(raw), "\n")
	}

	cat := skills.NewCatalog(skills.Source{GitURL: repo, CacheDir: t.TempDir()})
	list, err := cat.All(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("the seed repo has no skills, got %+v", list)
	}
	first := gitCalls()

	for range 4 {
		if _, err := cat.All(context.Background()); err != nil {
			t.Fatalf("cached load: %v", err)
		}
	}
	if again := gitCalls() - first; again != 0 {
		t.Fatalf("an empty catalog must be cached for the TTL, but 4 more requests ran git %d more times", again)
	}
}

// Package skills loads burrow's Library content: a directory of
// <name>/SKILL.md files, either a local path or a git repository refreshed on
// load. Each directory becomes one skill; the SKILL.md body is served
// verbatim as the skill's source. The catalog is global; which member sees
// which part of it is the audience filter in access.go.
//
// Governing: ADR-0001 (Library content from a git repository of SKILL.md
// skills), SPEC stage-a-control-plane REQ "Skills mapping", SPEC-0003
// REQ "Library audience" (the filter).
package skills

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Skill is one loadable Library entry.
type Skill struct {
	// Name is the kebab-case directory name; the client's discovery schema
	// enforces ^[a-z0-9]+(-[a-z0-9]+)*$ and 64 bytes.
	Name string
	// Title defaults to Name; SKILL.md frontmatter may override.
	Title string
	// Description comes from frontmatter and is required.
	Description string
	// Body is the full SKILL.md source, frontmatter included.
	Body string
	// UpdatedAt is the SKILL.md modification time.
	UpdatedAt time.Time
}

// Source locates the skills directory: Dir directly, or GitURL cloned and
// refreshed into CacheDir on every Load. SubDir names a directory inside the
// resolved source to load from instead of its root — the org's skills repo
// keeps its skills under skills/, and the repo root carries repo plumbing
// that is not a skill. GitTimeout bounds the git commands of one refresh; zero
// means DefaultGitTimeout.
type Source struct {
	Dir        string
	GitURL     string
	CacheDir   string
	SubDir     string
	GitTimeout time.Duration
}

// DefaultGitTimeout is how long one refresh may spend in git. A shallow clone
// of a skills repository is seconds of work, so a minute only ever ends a
// remote that has stopped answering.
const DefaultGitTimeout = time.Minute

// gitWaitDelay is how long a git that has exited or been killed may keep its
// output pipe open before the loader stops reading it.
const gitWaitDelay = 5 * time.Second

var nameRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Load returns the current catalog, refreshing the git clone when the source
// is a URL. Invalid entries are skipped with a log line, never fatal — one
// malformed skill must not take the org's Library down.
func (s *Source) Load(ctx context.Context) ([]Skill, error) {
	dir := s.Dir
	if s.GitURL != "" {
		var err error
		dir, err = s.refreshGit(ctx)
		if err != nil {
			return nil, err
		}
	}
	if s.SubDir != "" {
		dir = filepath.Join(dir, s.SubDir)
	}
	return loadDir(dir)
}

func (s *Source) refreshGit(ctx context.Context) (string, error) {
	// The refresh is shared: it runs under the refresh lock and its result
	// serves every member. So it takes its deadline from the loader, not from
	// the request that happened to trigger it — that member disconnecting must
	// not kill a clone part-way, and a request with no deadline of its own must
	// not let a silent remote hold the lock forever.
	timeout := s.GitTimeout
	if timeout <= 0 {
		timeout = DefaultGitTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	clone := filepath.Join(s.CacheDir, "repo")
	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		if err := os.MkdirAll(s.CacheDir, 0o755); err != nil {
			return "", fmt.Errorf("skills: cache dir: %w", err)
		}
		// A clone killed part-way leaves a .git directory behind, and the
		// check above would take it for a finished checkout from then on.
		// Whether refreshing such a repository ever recovers depends on where
		// git died, so the clone is made beside the cache and only a finished
		// one is renamed into place.
		tmp, err := os.MkdirTemp(s.CacheDir, "clone-")
		if err != nil {
			return "", fmt.Errorf("skills: cache dir: %w", err)
		}
		defer os.RemoveAll(tmp)
		out, err := s.git(ctx, "clone", "--depth", "1", s.GitURL, tmp)
		if err != nil {
			return "", fmt.Errorf("skills: git clone: %v: %s", err, out)
		}
		// Whatever sits at the path is not a checkout; it would block the rename.
		if err := os.RemoveAll(clone); err != nil {
			return "", fmt.Errorf("skills: cache dir: %w", err)
		}
		if err := os.Rename(tmp, clone); err != nil {
			return "", fmt.Errorf("skills: cache dir: %w", err)
		}
	} else {
		// The cache is a mirror of the remote's default branch, not a branch
		// of its own: fetch what the remote has now and move to it, whatever
		// the history between. A fast-forward-only pull freezes the Library
		// the first time upstream force-pushes, which is how a bad skill gets
		// retracted. origin is re-pointed first because the clone remembers
		// the URL it was made from: without that, changing
		// BURROW_SKILLS_GIT_URL is ignored for as long as the cache exists.
		for _, args := range [][]string{
			{"remote", "set-url", "origin", s.GitURL},
			{"fetch", "--depth", "1", "origin", "HEAD"},
			{"reset", "--hard", "FETCH_HEAD"},
		} {
			out, err := s.git(ctx, append([]string{"-C", clone}, args...)...)
			if err != nil {
				slog.Warn("skills: git refresh failed; serving the cached clone", "step", args[0], "error", err, "output", out)
				break
			}
		}
	}
	return clone, nil
}

// git runs one git command and returns its combined output. git reaches a
// remote through a helper process (git-remote-http, ssh) that inherits the
// output pipe and survives git itself being killed, so killing git alone
// leaves CombinedOutput reading a pipe the orphan still holds: the whole
// process group goes down with the deadline, and WaitDelay stops the read if
// something escapes that. The output comes back redacted, because it is only
// ever used in an error or a log line.
func (s *Source) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	killGroupOnCancel(cmd)
	cmd.WaitDelay = gitWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil && ctx.Err() != nil {
		// "signal: killed" says nothing; the deadline is why.
		err = ctx.Err()
	}
	return s.redact(strings.TrimSpace(string(out))), err
}

// redact takes the configured URL out of git's output. The URL may carry a
// credential, and git repeats it: a token in the username position comes back
// in "could not read Password for 'https://<token>@host'" when the remote
// answers 401. Current gits strip the userinfo from most other messages, so
// the URL is matched with and without it, and the userinfo on its own for the
// messages that print only part of the URL.
func (s *Source) redact(out string) string {
	const placeholder = "<BURROW_SKILLS_GIT_URL>"
	pairs := []string{s.GitURL, placeholder}
	if u, err := url.Parse(s.GitURL); err == nil && u.User != nil {
		userinfo := u.User.String()
		password, _ := u.User.Password()
		u.User = nil
		pairs = append(pairs, u.String(), placeholder, userinfo+"@", "***@")
		if password != "" {
			pairs = append(pairs, password, "***")
		}
	}
	return strings.NewReplacer(pairs...).Replace(out)
}

func loadDir(dir string) ([]Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("skills: read dir: %w", err)
	}
	var skills []Skill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !nameRE.MatchString(name) || len(name) > 64 {
			slog.Warn("skills: skipping directory with non-kebab-case name", "dir", name)
			continue
		}
		path := filepath.Join(dir, name, "SKILL.md")
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("skills: skipping unreadable SKILL.md", "path", path, "error", err)
			continue
		}
		skill, err := parse(name, string(raw), info.ModTime())
		if err != nil {
			slog.Warn("skills: skipping invalid SKILL.md", "path", path, "error", err)
			continue
		}
		skills = append(skills, skill)
	}
	return skills, nil
}

// frontmatter is what the loader reads out of a SKILL.md frontmatter block.
// Every other key is the agent's business and is ignored.
type frontmatter struct {
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
}

// parse reads the frontmatter of a SKILL.md: a leading `---`-delimited YAML
// block. description is required, title optional (defaults to the directory
// name). The block is decoded as YAML, not scanned for `key: value` lines: a
// line scan reads a folded `description: >-` as the description ">-" and
// takes a nested metadata block's keys for the skill's own, and what it gets
// wrong goes into the catalog the agent is shown.
func parse(name, raw string, updated time.Time) (Skill, error) {
	skill := Skill{Name: name, Title: name, Body: raw, UpdatedAt: updated}
	front, ok := splitFrontmatter(raw)
	if !ok {
		return skill, fmt.Errorf("no frontmatter block")
	}
	var meta frontmatter
	if err := yaml.Unmarshal([]byte(front), &meta); err != nil {
		return skill, fmt.Errorf("frontmatter is not valid YAML: %w", err)
	}
	if title := strings.TrimSpace(meta.Title); title != "" {
		skill.Title = title
	}
	skill.Description = strings.TrimSpace(meta.Description)
	if skill.Description == "" {
		return skill, fmt.Errorf("frontmatter has no description")
	}
	return skill, nil
}

// splitFrontmatter returns the text between the opening `---` line and the
// next one. A file saved with CRLF line endings opens with "---\r\n".
func splitFrontmatter(raw string) (string, bool) {
	rest, ok := strings.CutPrefix(raw, "---\n")
	if !ok {
		rest, ok = strings.CutPrefix(raw, "---\r\n")
	}
	if !ok {
		return "", false
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// Catalog is a caching view over a Source. The desktop app polls the Library
// endpoints; re-reading a handful of files on every poll is fine, but a git
// refresh is not, so the cache holds for CacheTTL.
type Catalog struct {
	Source   Source
	CacheTTL time.Duration

	mu        sync.Mutex // guards the cache fields
	refreshMu sync.Mutex // serializes refreshes; never held by cache hits
	skills    []Skill
	loadedAt  time.Time // zero until the first successful load
}

// DefaultCacheTTL balances freshness against clone traffic.
const DefaultCacheTTL = 30 * time.Second

// NewCatalog builds a caching catalog over source.
func NewCatalog(source Source) *Catalog {
	return &Catalog{Source: source, CacheTTL: DefaultCacheTTL}
}

// All returns the cached catalog, reloading when stale. The refresh lock is
// only held across Source.Load, so a slow git fetch never stalls cache hits;
// a reader arriving mid-refresh serves the stale catalog instead of waiting.
func (c *Catalog) All(ctx context.Context) ([]Skill, error) {
	c.mu.Lock()
	// "Loaded" is the timestamp, not a non-nil slice: a source with no valid
	// skills loads as an empty catalog, and an empty catalog that never counts
	// as cached runs git on every request.
	loaded := !c.loadedAt.IsZero()
	if loaded && time.Since(c.loadedAt) < c.CacheTTL {
		skills := c.skills
		c.mu.Unlock()
		return skills, nil
	}
	stale := c.skills
	c.mu.Unlock()

	if !c.refreshMu.TryLock() {
		if !loaded {
			// Cold start with a load already in flight: wait for it.
			c.refreshMu.Lock()
			c.refreshMu.Unlock()
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.skills, nil
	}
	defer c.refreshMu.Unlock()

	skills, err := c.Source.Load(ctx)
	if err != nil {
		// Serve the stale catalog rather than an error when a refresh fails.
		if loaded {
			slog.Warn("skills: reload failed; serving stale catalog", "error", err)
			return stale, nil
		}
		return nil, err
	}
	c.mu.Lock()
	c.skills = skills
	c.loadedAt = time.Now()
	c.mu.Unlock()
	return skills, nil
}

// All is the catalog's only accessor: search, lookup, and audience filtering
// are functions over the slice it returns (see access.go), so no surface can
// reach the catalog by a route that skips the filter.

// MaxDescription is the discovery schema's cap on a skill description.
const MaxDescription = 1024

// ClampDescription truncates over-long descriptions for the discovery index,
// never splitting a multi-byte rune at the byte cap.
func ClampDescription(d string) string {
	if len(d) <= MaxDescription {
		return d
	}
	cut := MaxDescription
	for cut > 0 && !utf8.RuneStart(d[cut]) {
		cut--
	}
	return d[:cut]
}

// TimestampMS renders a time the way the resource snapshot fields expect.
func TimestampMS(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

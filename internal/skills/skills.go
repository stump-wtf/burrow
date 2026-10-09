// Package skills loads burrow's Library content: a directory of
// <name>/SKILL.md files, either a local path or a git repository refreshed on
// load. Each directory becomes one skill; the SKILL.md body is served
// verbatim as the skill's source.
//
// Governing: ADR-0001 (Library content from a git repository of SKILL.md
// skills), SPEC stage-a-control-plane REQ "Skills mapping".
package skills

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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
// refreshed into CacheDir on every Load.
type Source struct {
	Dir      string
	GitURL   string
	CacheDir string
}

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
	return loadDir(dir)
}

func (s *Source) refreshGit(ctx context.Context) (string, error) {
	clone := filepath.Join(s.CacheDir, "repo")
	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		if err := os.MkdirAll(s.CacheDir, 0o755); err != nil {
			return "", fmt.Errorf("skills: cache dir: %w", err)
		}
		out, err := exec.CommandContext(ctx, "git", "clone", "--depth", "1", s.GitURL, clone).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("skills: git clone: %v: %s", err, strings.TrimSpace(string(out)))
		}
	} else {
		out, err := exec.CommandContext(ctx, "git", "-C", clone, "pull", "--ff-only").CombinedOutput()
		if err != nil {
			slog.Warn("skills: git pull failed; serving the cached clone", "error", strings.TrimSpace(string(out)))
		}
	}
	return clone, nil
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

// parse reads the frontmatter of a SKILL.md. Frontmatter is a leading
// `---`-delimited block of `key: value` lines; description is required,
// title optional (defaults to the directory name).
func parse(name, raw string, updated time.Time) (Skill, error) {
	skill := Skill{Name: name, Title: name, Body: raw, UpdatedAt: updated}
	front, ok := splitFrontmatter(raw)
	if !ok {
		return skill, fmt.Errorf("no frontmatter block")
	}
	for _, line := range front {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch key {
		case "description":
			skill.Description = value
		case "title", "name":
			if value != "" && key == "title" {
				skill.Title = value
			}
		}
	}
	if strings.TrimSpace(skill.Description) == "" {
		return skill, fmt.Errorf("frontmatter has no description")
	}
	return skill, nil
}

func splitFrontmatter(raw string) ([]string, bool) {
	if !strings.HasPrefix(raw, "---\n") {
		return nil, false
	}
	rest := raw[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, false
	}
	return strings.Split(rest[:end], "\n"), true
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
	loadedAt  time.Time
}

// DefaultCacheTTL balances freshness against clone traffic.
const DefaultCacheTTL = 30 * time.Second

// NewCatalog builds a caching catalog over source.
func NewCatalog(source Source) *Catalog {
	return &Catalog{Source: source, CacheTTL: DefaultCacheTTL}
}

// All returns the cached catalog, reloading when stale. The refresh lock is
// only held across Source.Load, so a slow git pull never stalls cache hits;
// a reader arriving mid-refresh serves the stale catalog instead of waiting.
func (c *Catalog) All(ctx context.Context) ([]Skill, error) {
	c.mu.Lock()
	if c.skills != nil && time.Since(c.loadedAt) < c.CacheTTL {
		skills := c.skills
		c.mu.Unlock()
		return skills, nil
	}
	stale := c.skills
	c.mu.Unlock()

	if !c.refreshMu.TryLock() {
		if stale == nil {
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
		if stale != nil {
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

// Find returns the skill with exactly name.
func (c *Catalog) Find(ctx context.Context, name string) (Skill, bool, error) {
	skills, err := c.All(ctx)
	if err != nil {
		return Skill{}, false, err
	}
	for _, s := range skills {
		if s.Name == name {
			return s, true, nil
		}
	}
	return Skill{}, false, nil
}

// Search returns skills whose name, title, or description contain every
// whitespace-separated term of query, case-insensitively. An empty query
// returns the whole catalog.
func (c *Catalog) Search(ctx context.Context, query string) ([]Skill, error) {
	skills, err := c.All(ctx)
	if err != nil {
		return nil, err
	}
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return skills, nil
	}
	var out []Skill
	for _, s := range skills {
		haystack := strings.ToLower(s.Name + " " + s.Title + " " + s.Description)
		matched := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matched = false
				break
			}
		}
		if matched {
			out = append(out, s)
		}
	}
	return out, nil
}

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

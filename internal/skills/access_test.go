package skills_test

import (
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
)

// Governing: SPEC-0003 REQ "Library audience", REQ "Compatibility"
// (stump.wtf/burrow#12). The catalog is one global list; this is the whole
// rule for which part of it a member sees.

var catalog = []skills.Skill{
	{Name: "code-review", Title: "Code Review"},
	{Name: "deploy-helper", Title: "Deploy Helper"},
	{Name: "private-notes", Title: "Private Notes"},
}

func names(list []skills.Skill) string {
	var out []string
	for _, s := range list {
		out = append(out, s.Name)
	}
	return strings.Join(out, ",")
}

func TestVisible(t *testing.T) {
	tests := []struct {
		name   string
		groups []string
		packs  skills.Packs
		want   string
	}{
		{
			name:   "no rules written is today's behavior",
			groups: []string{"family"},
			packs:  nil,
			want:   "code-review,deploy-helper,private-notes",
		},
		{
			name:   "a member no rule mentions keeps the full catalog",
			groups: []string{"stranger"},
			packs:  skills.Packs{"family": {"code-review"}},
			want:   "code-review,deploy-helper,private-notes",
		},
		{
			name:   "a member with no groups at all keeps the full catalog",
			groups: nil,
			packs:  skills.Packs{"family": {"code-review"}},
			want:   "code-review,deploy-helper,private-notes",
		},
		{
			name:   "a mapped group sees its union",
			groups: []string{"family"},
			packs:  skills.Packs{"family": {"code-review", "private-notes"}},
			want:   "code-review,private-notes",
		},
		{
			name:   "two mapped groups union, order follows the catalog",
			groups: []string{"ops", "family"},
			packs:  skills.Packs{"family": {"private-notes"}, "ops": {"deploy-helper"}},
			want:   "deploy-helper,private-notes",
		},
		{
			name:   "an unmapped group adds nothing to a mapped one",
			groups: []string{"family", "stranger"},
			packs:  skills.Packs{"family": {"code-review"}},
			want:   "code-review",
		},
		{
			name:   "a declared empty allowance shows nothing",
			groups: []string{"family"},
			packs:  skills.Packs{"family": {}},
			want:   "",
			// The store keeps a group row with no skill rows to say this;
			// it is not the same statement as having no rule at all.
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := names(skills.Visible(catalog, tt.groups, tt.packs)); got != tt.want {
				t.Fatalf("Visible(%v, %v) = %s, want %s", tt.groups, tt.packs, got, tt.want)
			}
		})
	}
}

// A search can only answer from what the member may see: the filtered slice is
// the only thing the search ever looks at.
func TestSearchSeesOnlyTheFilteredSlice(t *testing.T) {
	visible := skills.Visible(catalog, []string{"family"}, skills.Packs{"family": {"code-review"}})
	if got := names(skills.Search(visible, "deploy")); got != "" {
		t.Fatalf("a hidden skill must not match a search: %s", got)
	}
	if got := names(skills.Search(visible, "REVIEW")); got != "code-review" {
		t.Fatalf("search is case-insensitive over the visible slice: %s", got)
	}
}

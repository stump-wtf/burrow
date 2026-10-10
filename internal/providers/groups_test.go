package providers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Governing: SPEC-0003 REQ "Group declarations", "Key-scope warning",
// "Compatibility".

func loadJSON(t *testing.T, body string) ([]Provider, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

const row = `{"id":"lpr_a","providerId":"litellm","name":"A","baseURL":"https://gw.example/v1","apiKeyEnv":"K"`

func TestLoadRejectsEmptyGroupDeclarations(t *testing.T) {
	for name, body := range map[string]string{
		"empty provider list": `{"providers":[` + row + `,"groups":[],"models":[{"id":"m","name":"M"}]}]}`,
		"empty provider name": `{"providers":[` + row + `,"groups":[" "],"models":[{"id":"m","name":"M"}]}]}`,
		"empty model list":    `{"providers":[` + row + `,"models":[{"id":"m","name":"M","groups":[]}]}]}`,
	} {
		if _, err := loadJSON(t, body); err == nil || !strings.Contains(err.Error(), "groups") {
			t.Errorf("%s: want a groups validation error, got %v", name, err)
		}
	}
}

func TestUsesGroupsAndMixedAudience(t *testing.T) {
	plain, err := loadJSON(t, `{"providers":[`+row+`,"models":[{"id":"m","name":"M"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if NewRuntime(plain, timeZero()).UsesGroups() {
		t.Fatal("a file with no groups must not request the groups scope")
	}

	rowLevel, err := loadJSON(t, `{"providers":[`+row+`,"groups":["g"],"models":[{"id":"m","name":"M"},{"id":"n","name":"N"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(rowLevel, timeZero())
	if !rt.UsesGroups() {
		t.Fatal("row-level groups must request the groups scope")
	}
	if got := rt.MixedAudienceRows(); len(got) != 0 {
		t.Fatalf("one audience per row is the enforceable pattern; no warning expected, got %v", got)
	}

	mixed, err := loadJSON(t, `{"providers":[`+row+`,"models":[{"id":"m","name":"M"},{"id":"n","name":"N","groups":["owner"]}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := NewRuntime(mixed, timeZero()).MixedAudienceRows(); len(got) != 1 || got[0] != "lpr_a" {
		t.Fatalf("a row mixing audiences must be flagged, got %v", got)
	}
}

// A row with no models follows its provider's groups.
func TestRowWithoutModelsFollowsProviderGroups(t *testing.T) {
	list, err := loadJSON(t, `{"providers":[`+row+`,"groups":["g"],"models":[]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(list, timeZero())
	if got := rt.RowsFor(nil); len(got) != 0 {
		t.Fatalf("hidden from a member without g, got %v", got)
	}
	if got := rt.RowsFor([]string{"g"}); len(got) != 1 {
		t.Fatalf("visible to a member with g, got %v", got)
	}
}

func timeZero() time.Time { return time.Time{} }

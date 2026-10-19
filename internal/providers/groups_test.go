package providers_test

// Group-governed visibility for store rows (SPEC-0003, applied unchanged by
// SPEC-0004 REQ "Visibility unchanged"): the row's audience decides who sees
// the row; a model's audience is its row's (ADR-0004 retired model-level
// groups).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store/storetest"
)

func TestLoadRejectsEmptyGroupDeclarations(t *testing.T) {
	row := `{"id":"lpr_a","providerId":"litellm","name":"A","baseURL":"https://gw.example/v1","apiKeyEnv":"K"`
	for name, body := range map[string]string{
		"empty provider list": `{"providers":[` + row + `,"groups":[],"models":[{"id":"m","name":"M"}]}]}`,
		"empty provider name": `{"providers":[` + row + `,"groups":[" "],"models":[{"id":"m","name":"M"}]}]}`,
	} {
		path := filepath.Join(t.TempDir(), "providers.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := providers.Load(path); err == nil || !strings.Contains(err.Error(), "groups") {
			t.Errorf("%s: want a groups validation error, got %v", name, err)
		}
	}
}

// ValidateStoreRow applies the same group rules to a write: an empty list is
// a mistake, not "visible to nobody".
func TestValidateStoreRowGroups(t *testing.T) {
	base := store.ProviderRow{ID: "lpr_a", ProviderID: "p", Name: "A",
		BaseURL: "https://gw.example/v1", APIKeyEnv: "K"}

	withGroups := base
	withGroups.Groups = []string{"family"}
	if err := providers.ValidateStoreRow(withGroups, providers.Unconstrained()); err != nil {
		t.Fatalf("a row-level audience is valid: %v", err)
	}

	empty := base
	empty.Groups = []string{}
	if err := providers.ValidateStoreRow(empty, providers.Unconstrained()); err == nil || !strings.Contains(err.Error(), "groups") {
		t.Fatalf("an empty groups list must be rejected, got %v", err)
	}

	badOverride := base
	badOverride.NameOverrides = map[string]string{"gpt-5": " "}
	if err := providers.ValidateStoreRow(badOverride, providers.Unconstrained()); err == nil {
		t.Fatal("an empty display name must be rejected")
	}

	badID := base
	badID.ID = "nope"
	if err := providers.ValidateStoreRow(badID, providers.Unconstrained()); err == nil {
		t.Fatal("an id without the lpr_ prefix must be rejected")
	}
}

func TestRowVisibilityFollowsRowGroups(t *testing.T) {
	st := storetest.Open(t)
	gw := newFakeGateway(t)
	gw.add("m")
	t.Setenv("K", "sk-k")

	open := store.ProviderRow{ID: "lpr_open", ProviderID: "litellm", Name: "Open",
		BaseURL: gw.URL() + "/v1", APIKeyEnv: "K"}
	gated := store.ProviderRow{ID: "lpr_gated", ProviderID: "litellm", Name: "Gated",
		BaseURL: gw.URL() + "/v1", APIKeyEnv: "K", Groups: []string{"family"}}
	for _, p := range []store.ProviderRow{open, gated} {
		seedRow(t, st, p)
	}
	runtime := providers.NewRuntime(st, time.Minute, providers.Unconstrained())
	ctx := context.Background()

	ids := func(groups []string) string {
		t.Helper()
		rows, err := runtime.RowsFor(ctx, groups)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range rows {
			got = append(got, r.ID)
		}
		sort.Strings(got)
		return strings.Join(got, ",")
	}
	if got := ids(nil); got != "lpr_open" {
		t.Fatalf("a member without family sees only the open row, got %s", got)
	}
	if got := ids([]string{"family"}); got != "lpr_gated,lpr_open" {
		t.Fatalf("a member with family sees both, got %s", got)
	}

	// Connect follows visibility: the gated row answers like an unknown id.
	if _, _, ok, err := runtime.ConnectFor(ctx, "lpr_gated", nil); ok || err != nil {
		t.Fatalf("hidden row must not connect: %v %v", ok, err)
	}
	if _, _, ok, err := runtime.ConnectFor(ctx, "lpr_gated", []string{"family"}); !ok || err != nil {
		t.Fatalf("visible row must connect: %v %v", ok, err)
	}

	// The snapshot lists only visible rows.
	snap, err := runtime.SnapshotFor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 1 {
		if raw, marshalErr := json.Marshal(snap); marshalErr == nil {
			t.Fatalf("snapshot must list only visible rows, got %s", raw)
		}
	}
	if _, present := snap["lpr_gated"]; present {
		t.Fatal("hidden row must not appear in the snapshot")
	}
}

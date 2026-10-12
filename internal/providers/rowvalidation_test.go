package providers_test

// Row validation (SPEC-0004 REQ "Rows in the store"): what the seed file, a
// POST and a PUT may put in a row. Every one of these fields is later used as
// something else — a URL path segment, a request target carrying the row's
// key, a Postgres TEXT value, a group to match — so a value that only looks
// like a row must be refused before it is stored.

import (
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

func validStoreRow() store.ProviderRow {
	return store.ProviderRow{
		ID: "lpr_a", ProviderID: "litellm", Name: "A",
		BaseURL: "https://gw.example/v1", APIKeyEnv: "K",
	}
}

// A row id is a path segment on /connect and in the console's links, so the
// prefix alone is not enough: "lpr_a/b" cannot be addressed without
// percent-encoding, and "lpr_a/delete" names another row's delete page.
func TestValidateStoreRowIDCharset(t *testing.T) {
	bad := map[string]string{
		"slash":                 "lpr_a/b",
		"bare prefix":           "lpr_",
		"space and newline":     "lpr_x y\n",
		"dot segments":          "lpr_../admin",
		"console action suffix": "lpr_a/delete",
		"percent-encoded slash": "lpr_a%2Fb",
		"NUL":                   "lpr_a\x00",
		"trailing newline":      "lpr_a\n",
		// (?i) folds the Kelvin sign onto k; the charset is ASCII.
		"non-ASCII case fold": "lpr_K",
		"65 after the prefix": "lpr_" + strings.Repeat("a", 65),
		"100 KB":              "lpr_" + strings.Repeat("a", 100<<10),
	}
	for name, id := range bad {
		t.Run(name, func(t *testing.T) {
			row := validStoreRow()
			row.ID = id
			err := providers.ValidateStoreRow(row)
			if err == nil {
				t.Fatalf("id %.40q must be rejected", id)
			}
			if len(err.Error()) > 200 {
				t.Fatalf("the refusal must not echo an oversized id back, got %d bytes", len(err.Error()))
			}
		})
	}

	// The contract's prefix is case-insensitive (/^lpr_/i) and stays so.
	for _, id := range []string{"lpr_a", "lpr_litellm", "LPR_Family-2", "lpr__x", "lpr_" + strings.Repeat("a", 64)} {
		row := validStoreRow()
		row.ID = id
		if err := providers.ValidateStoreRow(row); err != nil {
			t.Fatalf("id %q must be accepted: %v", id, err)
		}
	}
}

// baseURL is both where burrow sends the row's key (GET {baseURL}/models)
// and what every member's client is told to call. A query or fragment lets
// the writer decide where "/models" lands; userinfo is a credential stored in
// a row and served to every member in providerConfig.options.baseURL.
func TestValidateStoreRowBaseURLShape(t *testing.T) {
	bad := map[string]string{
		"query":                  "https://gw.example/v1?x=",
		"bare question mark":     "https://gw.example/v1?",
		"fragment":               "https://gw.example/v1#frag",
		"bare hash":              "https://gw.example/v1#",
		"userinfo":               "https://user@gw.example/v1",
		"userinfo with password": "https://user:hunter2@gw.example/v1",
	}
	for name, baseURL := range bad {
		t.Run(name, func(t *testing.T) {
			row := validStoreRow()
			row.BaseURL = baseURL
			err := providers.ValidateStoreRow(row)
			if err == nil {
				t.Fatalf("baseURL %q must be rejected", baseURL)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("the refusal must not repeat a password: %v", err)
			}
		})
	}

	for _, baseURL := range []string{
		"https://gw.example/v1", "https://gw.example/v1/", "https://gw.example",
		"http://litellm.internal:4000", "http://[::1]:4000/v1",
	} {
		row := validStoreRow()
		row.BaseURL = baseURL
		if err := providers.ValidateStoreRow(row); err != nil {
			t.Fatalf("baseURL %q must be accepted: %v", baseURL, err)
		}
	}
}

// Postgres TEXT cannot hold a NUL, so a name carrying one passed validation
// and then failed in the store — a 500 on the admin API, and a half-applied
// seed at boot. No row field a human reads has a use for the other control
// characters either; they only make two rows or two groups look alike.
func TestValidateStoreRowRejectsControlCharacters(t *testing.T) {
	cases := map[string]func(*store.ProviderRow){
		"NUL in providerId":     func(p *store.ProviderRow) { p.ProviderID = "lite\x00llm" },
		"NUL in name":           func(p *store.ProviderRow) { p.Name = "A\x00B" },
		"newline in name":       func(p *store.ProviderRow) { p.Name = "A\nB" },
		"tab in name":           func(p *store.ProviderRow) { p.Name = "A\tB" },
		"DEL in name":           func(p *store.ProviderRow) { p.Name = "A\x7fB" },
		"C1 control in name":    func(p *store.ProviderRow) { p.Name = "A\u0085B" },
		"NUL in a group":        func(p *store.ProviderRow) { p.Groups = []string{"fam\x00ily"} },
		"newline in a group":    func(p *store.ProviderRow) { p.Groups = []string{"family", "eng\nineering"} },
		"escape in providerId":  func(p *store.ProviderRow) { p.ProviderID = "lite\x1bllm" },
		"carriage return in id": func(p *store.ProviderRow) { p.ID = "lpr_a\r" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			row := validStoreRow()
			mutate(&row)
			if err := providers.ValidateStoreRow(row); err == nil {
				t.Fatalf("must be rejected: %+q", row)
			}
		})
	}

	// Ordinary text in any script is not a control character.
	row := validStoreRow()
	row.Name, row.Groups = "Stump LiteLLM — família 家族", []string{"openwork-family", "Équipe A"}
	if err := providers.ValidateStoreRow(row); err != nil {
		t.Fatalf("printable non-ASCII must be accepted: %v", err)
	}
}

// A row's groups are compared exactly with the member's OIDC groups, so
// " family" is an audience nobody holds: the row would be stored, look right
// in a listing, and be served to no one. Refused rather than trimmed — the
// writer should see that what they sent is not what would have matched.
func TestValidateStoreRowRejectsUntrimmedGroups(t *testing.T) {
	for name, group := range map[string]string{
		"leading space":        " family",
		"trailing space":       "family ",
		"leading no-break":     " family",
		"trailing ideographic": "family　",
	} {
		t.Run(name, func(t *testing.T) {
			row := validStoreRow()
			row.Groups = []string{"friends", group}
			if err := providers.ValidateStoreRow(row); err == nil || !strings.Contains(err.Error(), "groups") {
				t.Fatalf("group %q must be rejected with a groups error, got %v", group, err)
			}
		})
	}

	// Space inside a name is part of the name.
	row := validStoreRow()
	row.Groups = []string{"family and friends"}
	if err := providers.ValidateStoreRow(row); err != nil {
		t.Fatalf("an inner space must be accepted: %v", err)
	}

	// The seed file goes through the same gate.
	file := `{"providers":[{"id":"lpr_a","providerId":"litellm","name":"A","baseURL":"https://gw.example/v1","apiKeyEnv":"K","groups":[" family"]}]}`
	if _, err := providers.Load(writeProvidersFile(t, file)); err == nil || !strings.Contains(err.Error(), "groups") {
		t.Fatalf("the seed file must refuse an untrimmed group, got %v", err)
	}
}

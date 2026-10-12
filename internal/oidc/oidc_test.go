package oidc

import (
	"strings"
	"testing"
)

// Governing: SPEC-0003 REQ "Groups claim" — an array of strings or a single
// string; anything else and empty entries yield nothing, never nil. Names are
// trimmed and listed once.
func TestGroupsFromClaim(t *testing.T) {
	cases := []struct {
		claim any
		want  string
	}{
		{[]any{"family", "friends"}, "family,friends"},
		{"owner", "owner"},
		{nil, ""},
		{"", ""},
		{[]any{"a", "", 3, "b"}, "a,b"},
		{map[string]any{"x": 1}, ""},
		// One spelling per group: every later comparison is exact, and the
		// rules it is compared with are trimmed when an admin saves them.
		{"contractors ", "contractors"},
		{"  ", ""},
		{[]any{" family", "friends\t", "family ", "   "}, "family,friends"},
		{[]any{"a", "b", "a"}, "a,b"},
	}
	for _, tc := range cases {
		got := GroupsFromClaim(tc.claim)
		if got == nil {
			t.Fatalf("%#v: result must never be nil", tc.claim)
		}
		if strings.Join(got, ",") != tc.want {
			t.Errorf("%#v: got %v, want %q", tc.claim, got, tc.want)
		}
	}
}

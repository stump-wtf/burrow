package oidc

import (
	"strings"
	"testing"
)

// Governing: SPEC-0003 REQ "Groups claim" — an array of strings or a single
// string; anything else and empty entries yield nothing, never nil.
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

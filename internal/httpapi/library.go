package httpapi

// The Library's single access path: every surface that lists or reads a skill
// — the control-plane JSON, the marketplace and plugin resolutions, the MCP
// tools, the discovery index — answers from a slice produced here, so a skill
// a member may not see is invisible on all of them at once. Hiding it in only
// one of these would leak it through the others, and the client's Library and
// the agent's catalog would disagree about what exists.
//
// A filtered-out skill is answered exactly like an absent one (SPEC-0003
// design: "invisible looks like absent"), so the Library never tells a member
// what someone else has. An MCP token resolves to the member who minted it, so
// the agent's view is that member's view — a token is not a second class of
// caller with its own rules.
//
// Governing: SPEC-0003 REQ "Library audience", REQ "Compatibility"
// (stump.wtf/burrow#12).

import (
	"net/http"

	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
)

// library returns the Library as the requesting member sees it.
func (s *Server) library(r *http.Request) ([]skills.Skill, error) {
	groups, err := s.memberGroups(r)
	if err != nil {
		return nil, err
	}
	return s.libraryFor(r, groups)
}

// libraryFor is library for groups already in hand, so a handler that needs
// both the skill slice and a group-filtered provider view reads the member
// once.
func (s *Server) libraryFor(r *http.Request, groups []string) ([]skills.Skill, error) {
	packs, err := s.store.ListSkillPacks(r.Context())
	if err != nil {
		return nil, err
	}
	list, err := s.skills.All(r.Context())
	if err != nil {
		return nil, err
	}
	return skills.Visible(list, groups, packs), nil
}

// skillByName resolves one skill inside the member's slice.
func (s *Server) skillByName(r *http.Request, name string) (skills.Skill, bool, error) {
	list, err := s.library(r)
	if err != nil {
		return skills.Skill{}, false, err
	}
	skill, ok := skills.Find(list, name)
	return skill, ok, nil
}

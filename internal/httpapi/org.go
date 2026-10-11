package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/skills"
)

func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// catalogUpdatedAt is the freshest skill mtime, or the epoch when the
// Library is empty.
func catalogUpdatedAt(list []skills.Skill) time.Time {
	var newest time.Time
	for _, s := range list {
		if s.UpdatedAt.After(newest) {
			newest = s.UpdatedAt
		}
	}
	return newest
}

func isoOrEpoch(t time.Time) string {
	if t.IsZero() {
		t = time.Unix(0, 0)
	}
	return t.UTC().Format(time.RFC3339)
}

// handleOrg answers GET /v1/org: the member directory. teams must be present
// (an empty list is fine).
func (s *Server) handleOrg(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not list members")
		return
	}
	members := make([]map[string]any, 0, len(users))
	for _, u := range users {
		members = append(members, map[string]any{
			"id":   memberID(u.ID),
			"user": map[string]any{"name": u.Name, "email": u.Email},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"currentMember": map[string]any{"id": memberID(userID(r))},
		"members":       members,
		"teams":         []map[string]any{},
	})
}

// handleResources answers GET /v1/resources: the resource snapshot the client
// diffs to notice Library changes.
func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	groups, err := s.memberGroups(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not load the member")
		return
	}
	list, err := s.skills.All(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not load the Library")
		return
	}
	plugins := make([]map[string]any, 0, len(list))
	for _, skill := range list {
		plugins = append(plugins, map[string]any{
			"pluginId":      pluginID(skill.Name),
			"lastUpdatedAt": isoOrEpoch(skill.UpdatedAt),
			"configItems": []map[string]any{{
				"configItemId":  configObjectID(skill.Name),
				"lastUpdatedAt": isoOrEpoch(skill.UpdatedAt),
			}},
		})
	}
	llmProviders, err := s.providers.SnapshotFor(r.Context(), groups)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not snapshot the providers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"organizationId": s.cfg.OrgID(),
		"orgMemberId":    memberID(userID(r)),
		"teamIds":        []string{},
		"resources": map[string]any{
			"llmProviders": llmProviders,
			"marketplaces": map[string]any{
				marketplaceID(s.cfg): map[string]any{
					"lastUpdatedAt": isoOrEpoch(catalogUpdatedAt(list)),
					"plugins":       plugins,
				},
			},
		},
	})
}

// handleMarketplaceCapabilities answers GET /v1/resources/marketplace-capabilities.
func (s *Server) handleMarketplaceCapabilities(w http.ResponseWriter, r *http.Request) {
	list, err := s.skills.All(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not load the Library")
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, skill := range list {
		items = append(items, map[string]any{
			"configObjectId": configObjectID(skill.Name),
			"marketplaceId":  marketplaceID(s.cfg),
			"objectType":     "skill",
			"pluginId":       pluginID(skill.Name),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleMarketplaces answers GET /v1/marketplaces.
func (s *Server) handleMarketplaces(w http.ResponseWriter, r *http.Request) {
	list, err := s.skills.All(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not load the Library")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": []map[string]any{{
			"id":          marketplaceID(s.cfg),
			"name":        s.cfg.OrgName + " Library",
			"description": "Skills served by burrow from " + s.skillsSourceLabel() + ".",
			"status":      "active",
			"pluginCount": len(list),
			"updatedAt":   isoOrEpoch(catalogUpdatedAt(list)),
		}},
	})
}

// handleMarketplaceResolved answers GET /v1/marketplaces/:id/resolved.
func (s *Server) handleMarketplaceResolved(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != marketplaceID(s.cfg) {
		writeError(w, http.StatusNotFound, "not_found", "no such marketplace")
		return
	}
	list, err := s.skills.All(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not load the Library")
		return
	}
	plugins := make([]map[string]any, 0, len(list))
	for _, skill := range list {
		plugins = append(plugins, orgPlugin(skill))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"item": map[string]any{
			"marketplace": map[string]any{
				"id":          marketplaceID(s.cfg),
				"name":        s.cfg.OrgName + " Library",
				"description": "Skills served by burrow from " + s.skillsSourceLabel() + ".",
				"status":      "active",
				"pluginCount": len(list),
				"updatedAt":   isoOrEpoch(catalogUpdatedAt(list)),
			},
			"plugins": plugins,
		},
	})
}

// orgPlugin is the plugin row inside a resolved marketplace.
func orgPlugin(skill skills.Skill) map[string]any {
	return map[string]any{
		"id":          pluginID(skill.Name),
		"name":        skill.Title,
		"description": skill.Description,
		"status":      "active",
		"memberCount": 0,
		"updatedAt":   isoOrEpoch(skill.UpdatedAt),
		"componentCounts": map[string]int{
			"skill": 1,
		},
	}
}

// configObject is the full config object for one skill: the SKILL.md body is
// the latest version's rawSourceText.
func configObject(skill skills.Skill) map[string]any {
	updated := isoOrEpoch(skill.UpdatedAt)
	return map[string]any{
		"id":                   configObjectID(skill.Name),
		"objectType":           "skill",
		"title":                skill.Title,
		"description":          skill.Description,
		"currentFileName":      "SKILL.md",
		"currentFileExtension": "md",
		"currentRelativePath":  skill.Name + "/SKILL.md",
		"status":               "active",
		"updatedAt":            updated,
		"latestVersion": map[string]any{
			"id":            "cfv_" + skill.Name,
			"rawSourceText": skill.Body,
			"createdAt":     updated,
		},
	}
}

// handlePluginResolved answers GET /v1/plugins/:id/resolved: the client reads
// payload.items as plugin memberships.
func (s *Server) handlePluginResolved(w http.ResponseWriter, r *http.Request) {
	name, ok := s.pluginByPluginID(r, w, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": []map[string]any{{
			"id":             membershipID(name.Name),
			"pluginId":       pluginID(name.Name),
			"configObjectId": configObjectID(name.Name),
			"configObject":   configObject(name),
		}},
	})
}

// handlePluginConfigObjects answers GET /v1/plugins/:id/config-objects.
func (s *Server) handlePluginConfigObjects(w http.ResponseWriter, r *http.Request) {
	skill, ok := s.pluginByPluginID(r, w, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": []map[string]any{{
			"configObjectId": configObjectID(skill.Name),
			"configObject":   configObject(skill),
		}},
	})
}

// handleConfigObjectLatest answers GET /v1/config-objects/:id/versions/latest.
func (s *Server) handleConfigObjectLatest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	list, err := s.skills.All(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not load the Library")
		return
	}
	for _, skill := range list {
		if configObjectID(skill.Name) == id {
			writeJSON(w, http.StatusOK, map[string]any{
				"item": map[string]any{
					"id":            id,
					"rawSourceText": skill.Body,
				},
			})
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "no such config object")
}

// pluginByPluginID resolves a plg_<name> identifier to its skill, writing the
// 404 itself when it does not exist.
func (s *Server) pluginByPluginID(r *http.Request, w http.ResponseWriter, id string) (skills.Skill, bool) {
	list, err := s.skills.All(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not load the Library")
		return skills.Skill{}, false
	}
	for _, skill := range list {
		if pluginID(skill.Name) == id {
			return skill, true
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "no such plugin")
	return skills.Skill{}, false
}

// handleLibrary answers GET /v1/me/library: every skill as a plugin item.
func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	list, err := s.skills.All(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not load the Library")
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, skill := range list {
		items = append(items, map[string]any{
			"type":           "plugin",
			"id":             pluginID(skill.Name),
			"name":           skill.Title,
			"description":    skill.Description,
			"componentKinds": []string{"skill"},
			"componentCount": 1,
			"role":           "owner",
			"edges":          []map[string]any{{"kind": "org_wide"}},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// skillsSourceLabel describes where Library content comes from, for the
// marketplace description rows.
func (s *Server) skillsSourceLabel() string {
	if s.cfg.SkillsGitURL != "" {
		return "a git repository"
	}
	return "the configured skills directory"
}

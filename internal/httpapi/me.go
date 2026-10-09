package httpapi

import (
	"net/http"
)

// handleMe answers GET /v1/me: {user:{id,email,name}}.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, err := s.store.GetUser(r.Context(), userID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not load the user")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"name":  user.Name,
		},
	})
}

// handleMyOrgs answers GET /v1/me/orgs. Every row must carry all four of
// id, name, slug and role or the client drops it (contract, Stage A table).
// Stage A has one org from config and every member is an owner.
func (s *Server) handleMyOrgs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"orgs": []map[string]any{{
			"id":   s.cfg.OrgID(),
			"name": s.cfg.OrgName,
			"slug": s.cfg.OrgSlug,
			"role": "owner",
		}},
		"activeOrgId":   s.cfg.OrgID(),
		"activeOrgSlug": s.cfg.OrgSlug,
	})
}

// handleSetActiveOrg answers POST /v1/me/active-organization. The response is
// ignored; with a single org the only valid choice is the configured one.
func (s *Server) handleSetActiveOrg(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OrganizationID   *string `json:"organizationId"`
		OrganizationSlug *string `json:"organizationSlug"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be JSON")
		return
	}
	id := ""
	if body.OrganizationID != nil {
		id = *body.OrganizationID
	} else if body.OrganizationSlug != nil {
		id = "org_" + *body.OrganizationSlug
	}
	if id != "" && id != s.cfg.OrgID() {
		writeError(w, http.StatusNotFound, "organization_not_found", "no such organization")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// handleDesktopConfig answers GET /v1/me/desktop-config. connectEnabled must
// be true or the agent gets no remote skills at all (contract, Stage A).
func (s *Server) handleDesktopConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"connectEnabled":     true,
		"automationsEnabled": false,
		"dashboardEnabled":   false,
	})
}

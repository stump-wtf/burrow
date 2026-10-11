package httpapi

// Admin console, shard 1 (SPEC-0004 REQ "Admin console surface", ADR-0004):
// the server-rendered /admin shell. The gate is the admin API's own decision
// — a session whose recorded OIDC groups intersect BURROW_ADMIN_GROUPS (or
// an llm-admin API token, as the write API admits) — with the same
// closed-by-default posture. The index lists the store's provider rows
// read-only; every mutation goes through the admin API, and no key value is
// ever rendered (the light is the key's presence, not its value).

import (
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// adminGate is the gate's outcome: a page state instead of the API's error
// code.
type adminGate int

const (
	adminGateAllow  adminGate = iota // the console
	adminGateDenied                  // signed in (or a token), not an admin
	adminGateSignin                  // no usable credential: the sign-in page
)

// adminGateVerdict is the web twin of requireAdmin's decision: the same
// lookups, the same intersection test, the same closed-by-default posture —
// only the answer changes from an error code to a page state, so the console
// and the API never gate the same credential differently. An unknown or
// expired credential answers "sign in" (the page has no token vocabulary to
// speak with); a verified-but-unprivileged one answers "not for you".
func (s *Server) adminGateVerdict(r *http.Request) (adminGate, error) {
	token := bearerToken(r)
	if strings.HasPrefix(token, store.AdminTokenPrefix) {
		tok, err := s.store.LookupAPIToken(r.Context(), token)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrExpired) {
				return adminGateSignin, nil
			}
			return adminGateDenied, err
		}
		if !hasScope(tok.Scopes, ScopeLLMAdmin) {
			return adminGateDenied, nil
		}
		return adminGateAllow, nil
	}
	userID, err := s.store.LookupSession(r.Context(), token)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrExpired) {
			return adminGateSignin, nil
		}
		return adminGateDenied, err
	}
	user, err := s.store.GetUser(r.Context(), userID)
	if err != nil {
		return adminGateDenied, err
	}
	if !holdsAny(user.Groups, s.cfg.AdminGroups) {
		return adminGateDenied, nil
	}
	return adminGateAllow, nil
}

// adminRowView is one row of the /admin index: the stored fields the console
// may show. It carries the key's presence only — the environment variable's
// value has no path into a page.
type adminRowView struct {
	Name    string
	ID      string
	BaseURL string
	HasKey  bool
	Groups  string // the row's groups, or "org-wide" when null
	Models  string // the derived menu's model count, or "deriving…"
}

type adminPageData struct {
	Title string
	Note  string
	Rows  []adminRowView
}

// adminPageTmpl is the /admin shell. It embeds pageCSS (the design-language
// token block, docs/design-language.md) and adds only the list's table rules;
// state comes from the gray ramp and the key light, never from hue.
var adminPageTmpl = template.Must(template.New("admin").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · burrow</title>
<style>` + pageCSS + `
/* the console list outgrows the auth surfaces' 400px card cap */
.card-admin{width:min(100%,720px)}
.rows{width:100%;border-collapse:collapse;margin-top:18px;font-size:13.5px}
.rows th{
  text-align:left;padding:0 8px 8px 0;color:var(--muted);
  border-bottom:1px solid var(--border);font-size:12.5px;font-weight:600;
}
.rows td{
  padding:10px 8px 10px 0;color:var(--body);vertical-align:top;
  border-bottom:1px solid var(--border);
}
.rows tr:last-child td{border-bottom:none}
.mono{
  font:12.5px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
  word-break:break-all;
}
.key-on{color:var(--ink)}
.key-off{color:var(--faint)}
</style></head><body>
<main class="shell">
<div class="wordmark">burrow</div>
<section class="card card-admin">
<h1>{{.Title}}</h1>
<p class="note">{{.Note}}</p>
{{if .Rows}}
<table class="rows">
<thead><tr><th>Name</th><th>Id</th><th>Base URL</th><th>Key</th><th>Groups</th><th>Models</th></tr></thead>
<tbody>
{{range .Rows}}
<tr>
<td>{{.Name}}</td>
<td class="mono">{{.ID}}</td>
<td class="mono">{{.BaseURL}}</td>
<td><span class="{{if .HasKey}}key-on{{else}}key-off{{end}}" role="img" aria-label="{{if .HasKey}}API key provisioned{{else}}API key not provisioned{{end}}">{{if .HasKey}}●{{else}}○{{end}}</span></td>
<td>{{.Groups}}</td>
<td>{{.Models}}</td>
</tr>
{{end}}
</tbody>
</table>
{{end}}
</section>
</main>
</body></html>`))

// renderAdminPage answers the console with one of its three pages: the list,
// the closed-surface notice, or the refusal. The status travels with the
// page (200 open/closed, 403 not-for-you, 500 the store could not answer),
// never a 404 — the surface exists and says what it knows.
func (s *Server) renderAdminPage(w http.ResponseWriter, status int, title, note string, rows []adminRowView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = adminPageTmpl.Execute(w, adminPageData{Title: title, Note: note, Rows: rows})
}

// handleAdminConsole serves the /admin shell. With no admin groups the
// surface is closed and answers a plain "not enabled" page; a session (or
// llm-admin token) the gate admits sees the provider list; everything else
// gets the sign-in page or the refusal.
func (s *Server) handleAdminConsole(w http.ResponseWriter, r *http.Request) {
	if len(s.cfg.AdminGroups) == 0 {
		s.renderAdminPage(w, http.StatusOK, "Admin console not enabled",
			"burrow has no admin groups configured (BURROW_ADMIN_GROUPS is unset or empty), so the admin console — and the provider write API with it — is closed. An operator opens it by setting the variable.",
			nil)
		return
	}
	if bearerToken(r) == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	verdict, err := s.adminGateVerdict(r)
	if err != nil {
		s.renderAdminPage(w, http.StatusInternalServerError, "Admin console unavailable",
			"burrow could not check your credential against the member store. Try again.", nil)
		return
	}
	switch verdict {
	case adminGateSignin:
		http.Redirect(w, r, "/", http.StatusFound)
	case adminGateDenied:
		s.renderAdminPage(w, http.StatusForbidden, "This surface isn't for you",
			"You are signed in, but you are not a member of an admin group — or your credential lacks the llm-admin scope the admin surface requires.", nil)
		return
	}
	rows, err := s.store.ListProviders(r.Context())
	if err != nil {
		s.renderAdminPage(w, http.StatusInternalServerError, "Admin console unavailable",
			"burrow could not read the provider rows. Try again.", nil)
		return
	}
	views := make([]adminRowView, 0, len(rows))
	for _, p := range rows {
		views = append(views, adminRowView{
			Name:    p.Name,
			ID:      p.ID,
			BaseURL: p.BaseURL,
			HasKey:  providers.KeyProvisioned(p.APIKeyEnv),
			Groups:  groupsOrOrgWide(p.Groups),
			Models:  s.modelsCell(p),
		})
	}
	s.renderAdminPage(w, http.StatusOK, "Providers",
		"The organization's provider rows, read from the store. Edits go through the admin API; this page shows each key only as a presence light.", views)
}

// groupsOrOrgWide renders the row's audience: its groups, or org-wide when
// the row is open to every member (a null audience).
func groupsOrOrgWide(groups []string) string {
	if groups == nil {
		return "org-wide"
	}
	return strings.Join(groups, ", ")
}

// modelsCell is the derived menu's model count from the cache alone: the
// count once the gateway has answered for the row, "deriving…" until then —
// the console never blocks a page on a live gateway call.
func (s *Server) modelsCell(p store.ProviderRow) string {
	n, ok := s.providers.CachedMenuCount(p)
	if !ok {
		return "deriving…"
	}
	unit := "models"
	if n == 1 {
		unit = "model"
	}
	return strconv.Itoa(n) + " " + unit
}

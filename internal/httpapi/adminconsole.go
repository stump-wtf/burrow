package httpapi

// The /admin console: server-rendered pages over the provider write API
// (SPEC-0004). The console is a CLIENT of that API, never a second
// management path: every mutation is driven through the API's own handlers
// and middleware in-process (consoleCall), so validation, etag semantics,
// and the audit trail are the API's, exactly. Reads use the same store
// functions the API's handlers call. Pages are gated by requireAdminPage
// with the API's own BURROW_ADMIN_GROUPS check, and the browser session is
// the cookie transport for the session token the desktop client holds as a
// bearer.
//
// Governing: SPEC-0004 REQ "Admin console", ADR-0004.

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// sessionCookieName carries the session token for browser surfaces (the
// desktop client sends the same token as a bearer instead).
const sessionCookieName = "burrow_session"

// adminSessionCookie returns the browser session token, or "".
func adminSessionCookie(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// requireAdminPage gates the console the way the API is gated: a session
// whose member holds any of BURROW_ADMIN_GROUPS. With no groups configured
// the console is closed to everyone (holdsAny on an empty allow list
// admits nobody). Unauthenticated browsers are sent to sign-in; a signed-in
// member who is not an admin gets the same 403 the API would answer.
func (s *Server) requireAdminPage(next func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := adminSessionCookie(r)
		if token == "" {
			http.Redirect(w, r, "/?mode=sign-in", http.StatusFound)
			return
		}
		userID, err := s.store.LookupSession(r.Context(), token)
		if err != nil {
			if errors.Is(err, store.ErrExpired) || errors.Is(err, store.ErrNotFound) {
				http.Redirect(w, r, "/?mode=sign-in", http.StatusFound)
				return
			}
			s.renderConsoleError(w, http.StatusInternalServerError, "the session store is unavailable")
			return
		}
		user, err := s.store.GetUser(r.Context(), userID)
		if err != nil {
			s.renderConsoleError(w, http.StatusInternalServerError, "could not load the member")
			return
		}
		if !holdsAny(user.Groups, s.cfg.AdminGroups) {
			s.renderConsoleError(w, http.StatusForbidden, "admin_required: this member is not in an admin group (BURROW_ADMIN_GROUPS)")
			return
		}
		next(w, r)
	})
}

// consoleCall drives the write API in-process: the request carries the
// member's session token as its bearer, so the API's own requireAdmin
// middleware re-checks the gate and wires the audit actor, and the API's
// own handlers validate, write, and audit. The console never writes rows
// any other way (SPEC-0004: one management path).
func (s *Server) consoleCall(r *http.Request, method, target string, body []byte, ifMatch string) (*httptest.ResponseRecorder, map[string]any) {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req = req.WithContext(r.Context())
	req.Header.Set("Authorization", "Bearer "+adminSessionCookie(r))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	s.adminAPI.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// consoleModel is one derived model in a console view: its id and the
// display name an override would give it.
type consoleModel struct {
	ID   string
	Name string
}

// consoleRow is one provider row as the console renders it. Key VALUES
// never appear here — only the environment variable's name and whether it
// is set (SPEC-0004: burrow never holds a key).
type consoleRow struct {
	ID              string
	ProviderID      string
	Name            string
	BaseURL         string
	APIKeyEnv       string
	HasAPIKey       bool
	Groups          []string
	GroupsNull      bool
	Models          []consoleModel
	ModelCount      int
	UpdatedAt       string
	HiddenOverrides []consoleModel
	IsNewRow        bool
}

// auditLine is one audit entry in the console's audit view. Detail carries
// the recorded before/after rows, pretty-printed for <pre> display.
type auditLine struct {
	When       string
	Actor      string
	Action     string
	ProviderID string
	Detail     string
}

// consolePage is the data for every console page (one template; sections
// render when their fields are set).
type consolePage struct {
	Title        string
	Flash        string
	Errors       []string
	Rows         []consoleRow
	IsPreview    bool
	PreviewGroup string
	GroupChoices []string
	Row          *consoleRow
	IsNew        bool
	IsConfirm    bool
	Etag         string
	Audit        []auditLine
	Matrix       *skillMatrix
}

const consoleCSS = `
body.console{display:block;place-items:start;max-width:980px;margin:0 auto;padding:32px 24px}
.console-head{display:flex;justify-content:space-between;align-items:baseline;margin-bottom:20px}
.console-head nav a{color:var(--muted);text-decoration:none;font-size:14px}
.console-head nav a:hover{color:var(--ink)}
table{width:100%;border-collapse:separate;border-spacing:0;background:var(--card);
  border:1px solid var(--border);border-radius:var(--radius-card);box-shadow:var(--card-shadow)}
th,td{text-align:left;padding:10px 12px;border-bottom:1px solid var(--border);font-size:14px;vertical-align:top}
tr:last-child td{border-bottom:none}
th{color:var(--muted);font-weight:600;font-size:12px;text-transform:uppercase;letter-spacing:.05em}
td .sub{color:var(--muted);font-size:12.5px}
.badge{display:inline-block;padding:1px 8px;border:1px solid var(--border);border-radius:999px;
  font-size:12px;color:var(--muted);white-space:nowrap}
.badge.warn{color:var(--ink);border-color:var(--muted)}
.flash,.error{margin:0 0 16px;padding:10px 14px;border-radius:var(--radius-control);font-size:14px}
.flash{background:var(--surface);border:1px solid var(--border);color:var(--body)}
.error{color:var(--ink);border:1px solid var(--muted)}
section.card{width:100%;margin-top:20px}
section.card.wide{max-width:none}
label{display:block;margin:14px 0 4px;font-size:13px;font-weight:500;color:var(--body)}
input[type=text]{width:100%;padding:8px 10px;border:1px solid var(--border);
  border-radius:var(--radius-control);font:14px/1.4 system-ui,sans-serif;color:var(--ink);
  background:var(--card)}
input[type=text]:focus-visible{outline:2px solid var(--ink);outline-offset:1px}
.hint{margin:4px 0 0;font-size:12.5px;color:var(--muted)}
.actions{display:flex;gap:12px;align-items:center;margin-top:20px}
.actions form{margin:0}
.button.inline{display:inline-block;width:auto;margin-top:0;padding:8px 14px}
.button.danger{background:var(--card);color:var(--ink);border:1px solid var(--muted)}
.button.danger:hover{background:var(--hover)}
.modelrow{display:grid;grid-template-columns:minmax(120px,1fr) minmax(120px,2fr);gap:8px;margin:6px 0}
.modelrow code{font:12.5px/1.6 ui-monospace,Menlo,Consolas,monospace;color:var(--muted);
  align-self:center;word-break:break-all}
details{margin:4px 0}
details pre{margin:6px 0;padding:10px 12px;background:var(--surface);border:1px solid var(--border);
  border-radius:var(--radius-control);font:12px/1.5 ui-monospace,Menlo,Consolas,monospace;
  overflow-x:auto;white-space:pre-wrap;word-break:break-all}
.keys-light{font-size:12.5px}
td.c,th.c{text-align:center}
input[type=checkbox]{width:16px;height:16px;accent-color:var(--ink);vertical-align:-3px}
th label.pack{display:inline;margin:0;font:600 12px system-ui,sans-serif;color:var(--muted);
  text-transform:uppercase;letter-spacing:.05em;white-space:nowrap}
a.plain{color:var(--ink);text-decoration:none}
a.plain:hover{text-decoration:underline}
`

var consoleTmpl = template.Must(template.New("console").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · burrow admin</title>
<style>` + pageCSS + consoleCSS + `</style></head>
<body class="console">
<main class="shell" style="width:100%">
<header class="console-head">
<div class="wordmark">burrow · admin</div>
<nav><a href="/admin">Providers</a> &middot; <a href="/admin/skills">Skill packs</a> &middot; <a href="/admin/audit">Audit</a></nav>
</header>
{{if .Flash}}<p class="flash">{{.Flash}}</p>{{end}}
{{range .Errors}}<p class="error">{{.}}</p>{{end}}

{{if .Rows}}
{{if .IsPreview}}
<h1 style="font-size:19px;font-weight:600;margin:0 0 16px">Visibility preview</h1>
<form method="get" action="/admin/preview" class="card" style="padding:20px">
<label for="group">A member of group</label>
<select id="group" name="group" style="padding:8px 10px;border:1px solid var(--border);border-radius:var(--radius-control);font:14px system-ui,sans-serif;color:var(--ink);background:var(--card)">
{{range .GroupChoices}}<option value="{{.}}"{{if eq $.PreviewGroup .}} selected{{end}}>{{.}}</option>{{end}}
</select>
<button class="button inline" type="submit" style="margin-left:12px">Preview</button>
</form>
<p class="note">Rows a member of <strong>{{.PreviewGroup}}</strong> would get — the same group match the runtime applies (SPEC-0003). Rows with a null audience are visible to every member.</p>
{{else}}
<h1 style="font-size:19px;font-weight:600;margin:0 0 16px">LLM providers</h1>
<p class="note" style="margin:0 0 16px"><a class="plain" href="/admin/providers/new">Add a provider</a> &middot; <a class="plain" href="/admin/preview">Visibility preview</a> &middot; audiences take effect at each member's next sign-in</p>
{{end}}
<table>
<thead><tr><th>Provider</th><th>Base URL</th><th>Key</th><th>Audience</th><th>Models</th><th></th></tr></thead>
<tbody>
{{range .Rows}}
<tr>
<td><a class="plain" href="/admin/providers/{{.ID}}">{{.Name}}</a><div class="sub">{{.ID}}</div></td>
<td>{{.BaseURL}}</td>
<td>{{if .HasAPIKey}}<span class="badge">{{.APIKeyEnv}} set</span>{{else}}<span class="badge warn">{{.APIKeyEnv}} unset</span>{{end}}</td>
<td>{{if .GroupsNull}}<span class="badge warn">everyone (org-wide)</span>{{else}}{{range .Groups}}<span class="badge">{{.}}</span> {{end}}{{end}}</td>
<td>{{.ModelCount}}</td>
<td><a class="plain" href="/admin/providers/{{.ID}}">edit</a></td>
</tr>
{{end}}
</tbody></table>
{{end}}

{{if .IsConfirm}}
<h1 style="font-size:19px;font-weight:600;margin:0">Delete {{.Row.Name}}?</h1>
<section class="card">
<p class="note">This deletes <strong>{{.Row.Name}}</strong> (<code>{{.Row.ID}}</code>) and its {{.Row.ModelCount}} derived model{{if ne .Row.ModelCount 1}}s{{end}} — menus come from the row's key, so members lose them at their next sign-in.{{if .Row.GroupsNull}} This row is visible org-wide (audience is null).{{end}}</p>
<form method="post" action="/admin/providers/{{.Row.ID}}/delete">
<input type="hidden" name="etag" value="{{.Etag}}">
<button class="button inline danger" type="submit">Delete provider</button>
<a class="button inline" href="/admin/providers/{{.Row.ID}}">Cancel</a>
</form>
</section>
{{end}}

{{if and .Row (not .IsConfirm)}}
<h1 style="font-size:19px;font-weight:600;margin:0">{{if .IsNew}}New provider{{else}}Edit {{.Row.Name}}{{end}}</h1>
<section class="card">
<form method="post" action="{{if .IsNew}}/admin/providers{{else}}/admin/providers/{{.Row.ID}}{{end}}">
{{if not .IsNew}}<input type="hidden" name="etag" value="{{.Etag}}">{{end}}
{{if .IsNew}}
<label for="id">Row id (lpr_&hellip;)</label>
<input type="text" id="id" name="id" value="{{.Row.ID}}">
<p class="hint">Unique; must match /^lpr_/i.</p>
{{end}}
<label for="providerId">Gateway provider id</label>
<input type="text" id="providerId" name="providerId" value="{{.Row.ProviderID}}">
<label for="name">Name</label>
<input type="text" id="name" name="name" value="{{.Row.Name}}">
<label for="baseURL">Base URL</label>
<input type="text" id="baseURL" name="baseURL" value="{{.Row.BaseURL}}">
<label for="apiKeyEnv">API key environment variable</label>
<input type="text" id="apiKeyEnv" name="apiKeyEnv" value="{{.Row.APIKeyEnv}}">
<p class="hint">The variable's NAME only — key values never enter burrow. Rotation stays an OpenBao / LiteLLM operation.
{{if not .IsNew}}{{if .Row.HasAPIKey}}Currently set.{{else}}Currently unset — the row serves no models.{{end}}{{end}}</p>
<label for="groups">Audience (groups)</label>
<input type="text" id="groups" name="groups" value="{{range .Row.Groups}}{{.}}, {{end}}" placeholder="empty = every member">
<p class="hint">Comma-separated OIDC groups. {{if .Row.GroupsNull}}This row currently has a null audience — it is visible org-wide.{{end}} Changes take effect at each member's next sign-in.</p>

{{if .Row.Models}}
<label>Models (derived from the row's key — display names are editable)</label>
{{range .Row.Models}}
<div class="modelrow">
<code>{{.ID}}</code>
<input type="text" name="override:{{.ID}}" value="{{.Name}}" placeholder="{{.ID}}">
</div>
{{end}}
{{else if not .IsNew}}
<p class="note">No models derived yet{{if not .Row.HasAPIKey}} — the key variable is unset{{end}}. The menu is the key's scope, fetched from the gateway and cached.</p>
{{end}}
{{range .Row.HiddenOverrides}}<input type="hidden" name="override:{{.ID}}" value="{{.Name}}">{{end}}

<div class="actions">
<button class="button inline" type="submit">Save</button>
{{if not .IsNew}}
<a class="button inline" href="/admin">Cancel</a>
</div>
</form>
<a class="button inline danger" style="margin-top:12px" href="/admin/providers/{{.Row.ID}}/delete">Delete provider</a>
{{else}}
</div>
</form>
{{end}}
</section>
{{end}}

{{if .Audit}}
<h1 style="font-size:19px;font-weight:600;margin:0 0 16px">Audit — recent provider changes</h1>
<table>
<thead><tr><th>When</th><th>Actor</th><th>Action</th><th>Provider</th><th>Detail</th></tr></thead>
<tbody>
{{range .Audit}}
<tr>
<td class="sub">{{.When}}</td>
<td>{{.Actor}}</td>
<td>{{.Action}}</td>
<td>{{.ProviderID}}</td>
<td>
{{if .Detail}}<details><summary>detail</summary><pre>{{.Detail}}</pre></details>{{end}}
</td>
</tr>
{{end}}
</tbody></table>
{{end}}

{{if .Matrix}}
<h1 style="font-size:19px;font-weight:600;margin:0 0 16px">Skill packs</h1>
{{if .Matrix.NoRules}}<p class="note">Nothing configured yet, so every member sees the whole catalog. Switch a group on and check the skills its members may see; nobody else changes.</p>{{else}}<p class="note">A group sees the skills checked in its column. A column switched off carries no rule, so its members keep the whole catalog; switched on with nothing checked, they see no skills.</p>{{end}}
{{if .Matrix.UntaggedGroups}}<p class="note">A rule names {{range .Matrix.UntaggedGroups}}<span class="badge">{{.}}</span> {{end}}but no member has signed in with that group yet — check the spelling in the identity provider.</p>{{end}}
<form method="post" action="/admin/skills">
<input type="hidden" name="groupCount" value="{{len .Matrix.Groups}}">
{{range $column, $group := .Matrix.Groups}}<input type="hidden" name="group.{{$column}}" value="{{$group}}">
{{end}}{{range $row, $skill := .Matrix.Rows}}<input type="hidden" name="skill.{{$row}}" value="{{$skill.Name}}">
{{end}}<table class="matrix">
<thead><tr><th>Skill</th>{{range $column, $group := .Matrix.Groups}}<th class="c"><label class="pack"><input type="checkbox" name="declare.{{$column}}" value="1"{{if index $.Matrix.Declared $column}} checked{{end}}> {{$group}}</label></th>{{end}}</tr></thead>
<tbody>
{{range $row, $skill := .Matrix.Rows}}
<tr>
<td><strong>{{$skill.Title}}</strong><div class="sub">{{$skill.Name}}</div></td>
{{range $column, $checked := $skill.Cells}}<td class="c"><input type="checkbox" name="cell.{{$column}}.{{$row}}" value="1"{{if $checked}} checked{{end}} aria-label="{{$skill.Name}} for {{index $.Matrix.Groups $column}}"></td>{{end}}
</tr>
{{end}}
</tbody></table>
<div class="actions"><button class="button inline" type="submit">Save</button></div>
</form>
{{if not .Matrix.Groups}}<p class="note">No group has reached burrow yet. A group's column appears after one of its members signs in, or once a rule names it.</p>{{end}}
{{end}}
</main>
</body></html>`))

// renderConsole executes the console template.
func renderConsole(w http.ResponseWriter, status int, data consolePage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = consoleTmpl.Execute(w, data)
}

func (s *Server) renderConsoleError(w http.ResponseWriter, status int, msg string) {
	renderConsole(w, status, consolePage{Title: "admin", Errors: []string{msg}})
}

// consoleRowOf builds the console view of one stored row, deriving its menu
// and key presence through the runtime (the same cache the served rows use).
func (s *Server) consoleRowOf(r *http.Request, p store.ProviderRow) consoleRow {
	ids, hasKey := s.providers.MenuFor(r.Context(), p)
	row := consoleRow{
		ID:         p.ID,
		ProviderID: p.ProviderID,
		Name:       p.Name,
		BaseURL:    p.BaseURL,
		APIKeyEnv:  p.APIKeyEnv,
		HasAPIKey:  hasKey,
		Groups:     p.Groups,
		GroupsNull: p.Groups == nil,
		ModelCount: len(ids),
		UpdatedAt:  quoteETag(p.UpdatedAt),
	}
	sort.Strings(ids)
	for _, id := range ids {
		name := id
		if o, ok := p.NameOverrides[id]; ok && o != "" {
			name = o
		}
		row.Models = append(row.Models, consoleModel{ID: id, Name: name})
	}
	// Overrides for ids the current menu does not carry are kept in the form
	// as hidden inputs: a PUT is a full replace, and losing them would drop
	// the display names the gateway's next scope change would need.
	for _, id := range sortedOverrideIDs(p.NameOverrides) {
		if !menuContains(ids, id) {
			row.HiddenOverrides = append(row.HiddenOverrides, consoleModel{ID: id, Name: p.NameOverrides[id]})
		}
	}
	return row
}

func menuContains(ids []string, id string) bool {
	for _, have := range ids {
		if have == id {
			return true
		}
	}
	return false
}

func sortedOverrideIDs(overrides map[string]string) []string {
	ids := make([]string, 0, len(overrides))
	for id := range overrides {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// handleConsoleList answers GET /admin: every row with name, baseURL, key
// status, audience, and model count.
func (s *Server) handleConsoleList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListProviders(r.Context())
	if err != nil {
		s.renderConsoleError(w, http.StatusInternalServerError, "could not load the providers")
		return
	}
	page := consolePage{Title: "LLM providers"}
	for _, p := range rows {
		page.Rows = append(page.Rows, s.consoleRowOf(r, p))
	}
	switch r.URL.Query().Get("saved") {
	case "1":
		page.Flash = "Saved. Audience changes reach each member at their next sign-in; model menus follow the row's key."
	case "deleted":
		page.Flash = "Provider deleted."
	}
	renderConsole(w, http.StatusOK, page)
}

// handleConsoleNew answers GET /admin/providers/new: a blank editor.
func (s *Server) handleConsoleNew(w http.ResponseWriter, r *http.Request) {
	renderConsole(w, http.StatusOK, consolePage{Title: "New provider", IsNew: true, Row: &consoleRow{}})
}

// handleConsoleEdit answers GET /admin/providers/{id}: the editor for one
// row, seeded from the store and carrying the row's etag for the save.
func (s *Server) handleConsoleEdit(w http.ResponseWriter, r *http.Request) {
	s.renderConsoleEditor(w, r, r.PathValue("id"), nil, nil)
}

// renderConsoleEditor renders the editor for id. conflict (the 412 current
// row) and formErr (a row rebuilt from submitted form values) re-seed the
// form so nothing the member typed is lost.
func (s *Server) renderConsoleEditor(w http.ResponseWriter, r *http.Request, id string, conflict *store.ProviderRow, formRow *consoleRow) {
	p, err := s.store.GetProvider(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.renderConsoleError(w, http.StatusNotFound, "no such provider: "+id)
			return
		}
		s.renderConsoleError(w, http.StatusInternalServerError, "could not load the provider")
		return
	}
	page := consolePage{Title: "Edit " + p.Name, Row: ptr(s.consoleRowOf(r, p)), Etag: quoteETag(p.UpdatedAt)}
	if conflict != nil {
		// 412: the API's body carries the current row; show it.
		page.Row = ptr(s.consoleRowOf(r, *conflict))
		page.Etag = quoteETag(conflict.UpdatedAt)
		page.Errors = append(page.Errors,
			"This row changed while you were editing (the If-Match etag was stale). The form now shows the CURRENT row — review it and save again.")
	}
	if formRow != nil {
		page.Row = formRow
	}
	renderConsole(w, http.StatusOK, page)
}

func ptr[T any](v T) *T { return &v }

// handleConsoleCreate answers POST /admin/providers: the form becomes a
// POST to the write API; errors come back as form errors.
func (s *Server) handleConsoleCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderConsoleError(w, http.StatusBadRequest, "could not read the form")
		return
	}
	formRow, body := consoleFormRow(r, "", r.PostForm)
	rec, out := s.consoleCall(r, http.MethodPost, "/v1/admin/llm-providers", body, "")
	switch rec.Code {
	case http.StatusCreated:
		http.Redirect(w, r, "/admin?saved=1", http.StatusSeeOther)
	case http.StatusConflict:
		s.renderConsoleForm(w, r, "", "A provider with that id already exists (duplicate_id).", formRow)
	case http.StatusBadRequest:
		s.renderConsoleForm(w, r, "", consoleAPIError(out, "invalid_row"), formRow)
	default:
		s.renderConsoleError(w, http.StatusInternalServerError, "the write API refused the change: "+strconv.Itoa(rec.Code))
	}
}

// handleConsoleUpdate answers POST /admin/providers/{id}: fetch → edit →
// send the fetched etag. A stale etag is a 412 whose body carries the
// current row, shown in the form for a re-save (SPEC-0004 REQ "Admin
// surface").
func (s *Server) handleConsoleUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderConsoleError(w, http.StatusBadRequest, "could not read the form")
		return
	}
	id := r.PathValue("id")
	etag := r.PostForm.Get("etag")
	formRow, body := consoleFormRow(r, id, r.PostForm)
	if body == nil {
		s.renderConsoleError(w, http.StatusInternalServerError, "could not encode the row")
		return
	}
	rec, out := s.consoleCall(r, http.MethodPut, "/v1/admin/llm-providers/"+url.PathEscape(id), body, etag)
	switch rec.Code {
	case http.StatusOK:
		http.Redirect(w, r, "/admin?saved=1", http.StatusSeeOther)
	case http.StatusPreconditionFailed:
		current := consoleAPIRow(out)
		if current != nil {
			s.renderConsoleEditor(w, r, id, current, nil)
			return
		}
		s.renderConsoleForm(w, r, etag, "The row changed while you were editing; reload the editor and save again.", formRow)
	case http.StatusBadRequest:
		s.renderConsoleForm(w, r, etag, consoleAPIError(out, "invalid_row"), formRow)
	case http.StatusNotFound:
		s.renderConsoleError(w, http.StatusNotFound, "no such provider: "+id)
	default:
		s.renderConsoleError(w, http.StatusInternalServerError, "the write API refused the change: "+strconv.Itoa(rec.Code))
	}
}

// handleConsoleConfirmDelete answers GET /admin/providers/{id}/delete: the
// confirm step before a delete. It names the row's display name and model
// count and carries the row's current etag, so the confirm POST states the
// exact version the admin agreed to delete (SPEC-0004: no silent clobbers).
func (s *Server) handleConsoleConfirmDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := s.store.GetProvider(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.renderConsoleError(w, http.StatusNotFound, "no such provider: "+id)
			return
		}
		s.renderConsoleError(w, http.StatusInternalServerError, "could not load the provider")
		return
	}
	page := consolePage{Title: "Delete " + p.Name, IsConfirm: true,
		Row: ptr(s.consoleRowOf(r, p)), Etag: quoteETag(p.UpdatedAt)}
	renderConsole(w, http.StatusOK, page)
}

// handleConsoleDelete answers POST /admin/providers/{id}/delete: the yes on
// the confirm step. The etag the confirm page carried rides to the API as
// If-Match, so a row that changed since the confirm step is not deleted
// blind; the API's own handler, store path, and audit do the work.
func (s *Server) handleConsoleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_ = r.ParseForm()
	rec, _ := s.consoleCall(r, http.MethodDelete, "/v1/admin/llm-providers/"+url.PathEscape(id), nil, r.FormValue("etag"))
	switch rec.Code {
	case http.StatusNoContent:
		http.Redirect(w, r, "/admin?saved=deleted", http.StatusSeeOther)
	case http.StatusNotFound:
		s.renderConsoleError(w, http.StatusNotFound, "no such provider: "+id)
	case http.StatusPreconditionFailed:
		s.renderConsoleError(w, http.StatusPreconditionFailed, "the row changed since the confirm step; open it again and delete from there")
	default:
		s.renderConsoleError(w, http.StatusInternalServerError, "the write API refused the delete: "+strconv.Itoa(rec.Code))
	}
}

// handleConsolePreview answers GET /admin/preview?group=: exactly the rows a
// member of that group would get, computed by the runtime's own group
// matching (SPEC-0003), so the console's answer cannot drift from the
// served one.
func (s *Server) handleConsolePreview(w http.ResponseWriter, r *http.Request) {
	group := strings.TrimSpace(r.URL.Query().Get("group"))
	page := consolePage{Title: "Visibility preview", IsPreview: true, PreviewGroup: group}

	all, err := s.store.ListProviders(r.Context())
	if err != nil {
		s.renderConsoleError(w, http.StatusInternalServerError, "could not load the providers")
		return
	}
	page.GroupChoices = distinctGroups(all)
	if group != "" {
		rows, err := s.providers.RowsFor(r.Context(), []string{group})
		if err != nil {
			s.renderConsoleError(w, http.StatusInternalServerError, "could not load the preview")
			return
		}
		for _, row := range rows {
			page.Rows = append(page.Rows, consoleRow{
				ID:         row.ID,
				ProviderID: row.ProviderID,
				Name:       row.Name,
				BaseURL:    baseURLOf(row),
				APIKeyEnv:  apiKeyEnvOf(row),
				HasAPIKey:  row.HasAPIKey,
				ModelCount: len(row.Models),
				Models:     modelsOf(row),
			})
		}
	}
	renderConsole(w, http.StatusOK, page)
}

// handleConsoleAudit answers GET /admin/audit: the store audit records the
// write API writes, newest first, with actor and before/after detail.
func (s *Server) handleConsoleAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.store.ListAudit(r.Context(), adminAuditDefaultLimit)
	if err != nil {
		s.renderConsoleError(w, http.StatusInternalServerError, "could not load the audit trail")
		return
	}
	page := consolePage{Title: "Audit"}
	for _, e := range entries {
		page.Audit = append(page.Audit, auditLine{
			When:       e.CreatedAt.UTC().Format(time.RFC3339),
			Actor:      e.Actor,
			Action:     e.Action,
			ProviderID: e.ProviderID,
			Detail:     auditDetail(e.Before, e.After),
		})
	}
	renderConsole(w, http.StatusOK, page)
}

// auditDetail renders the before/after rows the write API recorded. Secret
// values never occur in them: rows name environment variables only.
func auditDetail(before, after json.RawMessage) string {
	var b strings.Builder
	if len(before) > 0 {
		b.WriteString("before: " + prettyJSON(before))
	}
	if len(after) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("after: " + prettyJSON(after))
	}
	return b.String()
}

func prettyJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// renderConsoleForm re-renders the editor with the submitted values and an
// error banner (create and update failures). The etag rides along so a
// retry after a fixable error keeps its precondition.
func (s *Server) renderConsoleForm(w http.ResponseWriter, r *http.Request, etag, msg string, row *consoleRow) {
	page := consolePage{Title: "New provider", IsNew: row.IsNewRow, Row: row, Etag: etag}
	if !row.IsNewRow {
		page.Title = "Edit provider"
	}
	if msg != "" {
		page.Errors = []string{msg}
	}
	renderConsole(w, http.StatusBadRequest, page)
}

// consoleFormRow builds both the re-renderable form row and the write API
// JSON body from submitted form values. A nil body means the form could not
// be read and the caller already answered.
func consoleFormRow(r *http.Request, id string, form url.Values) (*consoleRow, []byte) {
	row := &consoleRow{
		ID:         strings.TrimSpace(form.Get("id")),
		ProviderID: strings.TrimSpace(form.Get("providerId")),
		Name:       strings.TrimSpace(form.Get("name")),
		BaseURL:    strings.TrimSpace(form.Get("baseURL")),
		APIKeyEnv:  strings.TrimSpace(form.Get("apiKeyEnv")),
		Groups:     parseGroups(form.Get("groups")),
		IsNewRow:   id == "",
	}
	if id != "" {
		row.ID = id
	}
	overrides := map[string]string{}
	for key, values := range form {
		if !strings.HasPrefix(key, "override:") {
			continue
		}
		modelID := strings.TrimPrefix(key, "override:")
		if modelID == "" || len(values) == 0 {
			continue
		}
		name := strings.TrimSpace(values[0])
		if name != "" && name != modelID {
			overrides[modelID] = name
		}
		row.Models = append(row.Models, consoleModel{ID: modelID, Name: name})
	}
	sort.Slice(row.Models, func(i, j int) bool { return row.Models[i].ID < row.Models[j].ID })
	body, err := json.Marshal(adminProvider{
		ID:            row.ID,
		ProviderID:    row.ProviderID,
		Name:          row.Name,
		BaseURL:       row.BaseURL,
		APIKeyEnv:     row.APIKeyEnv,
		Groups:        row.Groups,
		NameOverrides: overrides,
	})
	if err != nil {
		return row, nil
	}
	return row, body
}

// parseGroups turns the comma-separated audience input into the row's
// groups: blank means null (visible to every member), never an empty array
// (SPEC-0004 rejects one).
func parseGroups(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var groups []string
	for _, part := range strings.Split(raw, ",") {
		if g := strings.TrimSpace(part); g != "" {
			groups = append(groups, g)
		}
	}
	if len(groups) == 0 {
		return nil
	}
	return groups
}

// consoleAPIError extracts the API's message for a form error banner.
func consoleAPIError(out map[string]any, fallbackCode string) string {
	if out == nil {
		return fallbackCode + ": the write API rejected the row"
	}
	code, _ := out["error"].(string)
	message, _ := out["message"].(string)
	if code == "" {
		code = fallbackCode
	}
	if message == "" {
		return code
	}
	return code + ": " + message
}

// consoleAPIRow extracts the current row a 412 body carries.
func consoleAPIRow(out map[string]any) *store.ProviderRow {
	if out == nil {
		return nil
	}
	raw, err := json.Marshal(out["llmProvider"])
	if err != nil {
		return nil
	}
	var wire adminProvider
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil
	}
	etag, err := time.Parse(time.RFC3339Nano, unquoteETag(wire.Etag))
	if err != nil {
		return nil
	}
	row := wire.toStoreRow()
	row.UpdatedAt = etag
	return &row
}

// distinctGroups lists every group named by any row's audience, for the
// preview picker.
func distinctGroups(rows []store.ProviderRow) []string {
	seen := map[string]bool{}
	var groups []string
	for _, p := range rows {
		for _, g := range p.Groups {
			if !seen[g] {
				seen[g] = true
				groups = append(groups, g)
			}
		}
	}
	sort.Strings(groups)
	return groups
}

// apiKeyEnvOf pulls the row's key variable name back out of the client-facing
// row shape (preview rows come from the runtime, not the store).
func apiKeyEnvOf(row providers.Row) string {
	if envs, ok := row.ProviderConfig["env"].([]string); ok && len(envs) > 0 {
		return envs[0]
	}
	return ""
}

// baseURLOf mirrors apiKeyEnvOf for the base URL.
func baseURLOf(row providers.Row) string {
	options, _ := row.ProviderConfig["options"].(map[string]any)
	baseURL, _ := options["baseURL"].(string)
	return baseURL
}

func modelsOf(row providers.Row) []consoleModel {
	out := make([]consoleModel, 0, len(row.Models))
	for _, m := range row.Models {
		out = append(out, consoleModel{ID: m.ID, Name: m.Name})
	}
	return out
}

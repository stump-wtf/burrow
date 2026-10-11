package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// bearerToken extracts the Bearer token from the Authorization header.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

type ctxKey int

const userIDKey ctxKey = 0

// requireSession wraps a handler that needs an authenticated session. The
// error codes are exactly the ones the client contract lists as
// session-terminating; anything the client might meet elsewhere must not
// collide with them (docs/client-contract.md, Authentication §3).
func (s *Server) requireSession(next func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		userID, err := s.store.LookupSession(r.Context(), token)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrExpired):
				writeError(w, http.StatusUnauthorized, "session_expired", "session has expired")
			case errors.Is(err, store.ErrNotFound):
				writeError(w, http.StatusUnauthorized, "invalid_session", "session not found")
			default:
				// A transient store failure is not a session problem. Any
				// error code outside the session-terminating list makes the
				// client treat burrow as unavailable and keep its token
				// (docs/client-contract.md, Authentication §3).
				slog.Error("session lookup failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal", "session store unavailable")
			}
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userIDKey, userID)))
	})
}

func userID(r *http.Request) string {
	id, _ := r.Context().Value(userIDKey).(string)
	return id
}

// memberGroups returns the session member's OIDC groups as of their last
// sign-in (SPEC-0003 REQ "Groups claim").
func (s *Server) memberGroups(r *http.Request) ([]string, error) {
	user, err := s.store.GetUser(r.Context(), userID(r))
	if err != nil {
		return nil, err
	}
	return user.Groups, nil
}

func randomState() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		panic("httpapi: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func randomVerifier() string {
	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		panic("httpapi: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>burrow</title>
<style>` + pageCSS + `</style></head><body>
<main class="shell">
<div class="wordmark">burrow</div>
<section class="card">
{{if .SignInURL}}<h1>Sign in to {{.OrgName}}</h1>{{else}}<h1>One-time grant</h1>{{end}}
{{range .Notes}}<p class="note">{{.}}</p>{{end}}
{{if .Grant}}
<p class="note">Copy this grant into the OpenWork desktop app's manual sign-in, or let the button below hand it over.</p>
<code id="grant">{{.Grant}}</code>
{{if .DeepLink}}<a class="button" href="{{.DeepLink}}">Open OpenWork</a>
<p class="alt">or open <a href="{{.DeepLink}}">{{.DeepLink}}</a></p>{{end}}
{{end}}
{{if .SignInURL}}<a class="button href-signin" href="{{.SignInURL}}">Sign in with {{.OrgName}}</a>{{end}}
{{if .AdminLink}}<a class="button" href="/admin">Open the admin console</a>{{end}}
</section>
</main>
<script>if ({{.AutoRedirect}}) { try { location.replace({{.DeepLink}}) } catch (e) {} }</script>
</body></html>`))

type pageData struct {
	Notes        []string
	Grant        string
	DeepLink     template.URL
	AutoRedirect bool
	SignInURL    string
	OrgName      string
	AdminLink    bool
}

// handleRoot serves burrow's sign-in page. The desktop app opens it with
// ?mode=sign-in&desktopAuth=1&desktopScheme=openwork; the page is burrow's
// own design (contract, Authentication §1).
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	desktop := r.URL.Query().Get("desktopAuth") == "1"
	data := pageData{OrgName: s.cfg.OrgName}
	if desktop {
		data.Notes = []string{"Sign in to hand a one-time grant to the OpenWork desktop app."}
	}
	data.SignInURL = "/auth/oidc/start"
	if desktop {
		data.SignInURL += "?desktop=1"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTmpl.Execute(w, data)
}

// handleOIDCStart begins the authorization-code-with-PKCE flow.
func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	state := randomState()
	verifier := randomVerifier()
	desktop := r.URL.Query().Get("desktop") == "1"
	if err := s.store.SaveOIDCState(r.Context(), state, verifier, desktop, s.cfg.OIDCStateTTL); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not start sign-in")
		return
	}
	http.Redirect(w, r, s.oidc.AuthCodeURL(state, verifier), http.StatusFound)
}

// handleOIDCCallback finishes the flow: exchange, verify, upsert the user,
// mint a single-use grant, and hand it to the desktop app.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if state == "" || code == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "missing state or code")
		return
	}
	verifier, desktop, err := s.store.TakeOIDCState(r.Context(), state)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_state", "sign-in attempt expired or unknown; start again")
		return
	}
	rawIDToken, err := s.oidc.Exchange(r.Context(), code, verifier)
	if err != nil {
		writeError(w, http.StatusBadGateway, "oidc_exchange_failed", "the identity provider rejected the sign-in")
		return
	}
	claims, err := s.oidc.Verify(r.Context(), rawIDToken)
	if err != nil {
		slog.Error("oidc verify failed", "error", err)
		writeError(w, http.StatusUnauthorized, "oidc_verification_failed", "the identity token did not verify")
		return
	}
	// Groups are recorded at every sign-in and read from the user record on
	// later requests (SPEC-0003 REQ "Groups claim").
	user := store.User{ID: claims.Subject, Email: claims.Email, Name: claims.Name, Groups: claims.Groups}
	if err := s.store.UpsertUser(r.Context(), user); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not record the user")
		return
	}
	grant, err := s.store.MintGrant(r.Context(), user.ID, s.cfg.GrantTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not mint a grant")
		return
	}

	origin := s.origin(r)
	data := pageData{
		Grant: grant,
		Notes: []string{"Signed in as " + firstNonEmpty(user.Name, user.Email) + "." +
			" The grant works once and expires in " + s.cfg.GrantTTL.String() + "."},
	}
	// A web (non-desktop) sign-in also establishes a browser session for
	// the /admin console: the cookie carries the same session token the
	// desktop client holds as a bearer, so the console reuses the session
	// store, its TTL, and sign-out, and the admin gate stays the API's own
	// BURROW_ADMIN_GROUPS check (SPEC-0004 REQ "Admin console"). HttpOnly +
	// SameSite=Lax keeps it out of scripts and off cross-site form posts,
	// so the console's POST-only mutations are not CSRF-able.
	if !desktop {
		token, err := s.store.CreateSession(r.Context(), user.ID, s.cfg.SessionTTL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", "could not create a session")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   strings.HasPrefix(origin, "https://"),
			MaxAge:   int(s.cfg.SessionTTL.Seconds()),
		})
		if holdsAny(user.Groups, s.cfg.AdminGroups) {
			data.AdminLink = true
		}
	}
	if desktop {
		// Always include denBaseUrl, URL-encoded, per the client contract.
		data.DeepLink = template.URL((&url.URL{
			Scheme:   "openwork",
			Host:     "den-auth",
			RawQuery: url.Values{"grant": {grant}, "denBaseUrl": {origin}}.Encode(),
		}).String())
		data.AutoRedirect = true
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTmpl.Execute(w, data)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// handleMintHandoff implements the optional pre-provisioning mint
// (contract, Authentication §5). burrow requires an authenticated session:
// an unauthenticated mint would be an auth bypass. The grant is for the
// calling member.
func (s *Server) handleMintHandoff(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DesktopScheme string `json:"desktopScheme"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.DesktopScheme != "" && body.DesktopScheme != "openwork" {
		writeError(w, http.StatusBadRequest, "invalid_request", "unsupported desktopScheme")
		return
	}
	grant, err := s.store.MintGrant(r.Context(), userID(r), s.cfg.GrantTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not mint a grant")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"grant": grant})
}

// handleExchange burns a grant for a session token (contract,
// Authentication §2). No Authorization header is sent.
func (s *Server) handleExchange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Grant string `json:"grant"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be JSON with a grant")
		return
	}
	if strings.TrimSpace(body.Grant) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "missing grant")
		return
	}
	uid, err := s.store.ConsumeGrant(r.Context(), body.Grant)
	if err != nil {
		// invalid_grant is deliberately not a session-terminating code.
		writeError(w, http.StatusUnauthorized, "invalid_grant", "grant is unknown, used, or expired")
		return
	}
	user, err := s.store.GetUser(r.Context(), uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "grant pointed at a missing user")
		return
	}
	token, err := s.store.CreateSession(r.Context(), user.ID, s.cfg.SessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not create a session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token,
		"user": map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"name":  user.Name,
		},
		"organization": map[string]any{
			"id":   s.cfg.OrgID(),
			"slug": s.cfg.OrgSlug,
			"name": s.cfg.OrgName,
		},
		"connectEnabled": true,
	})
}

// handleSignOut revokes the calling session. The client posts best-effort to
// <api base>/api/auth/sign-out.
func (s *Server) handleSignOut(w http.ResponseWriter, r *http.Request) {
	if token := bearerToken(r); token != "" {
		if err := s.store.DeleteSession(r.Context(), token); err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", "could not sign out")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

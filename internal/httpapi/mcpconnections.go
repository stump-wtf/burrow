// Shared MCP connections on the wire (Stage C, stump.wtf/burrow#4).
//
// One org sees one list of MCP connections, and the agent reaches them through
// burrow rather than as local client entries — so this file answers two
// questions and never both at once:
//
//   - "What does this member see?" The listing, and the connect/disconnect
//     routes, which are the desktop client's contract.
//   - "What may burrow present upstream?" The credential behind one connection
//     for one member, which mcpbridge.go asks when it proxies a tool call.
//
// Two rules hold everywhere here. A connection is addressed by its audience in
// SPEC-0003's shape, so a member who holds none of its groups is answered
// exactly as they would be for a connection that does not exist. And a
// credential leaves this file in one direction only, upstream: nothing an
// endpoint returns carries a key or a token, and nothing a credential is logged
// with names its value.
//
// Governing: ADR-0010 (where a credential lives, and the OAuth dance), ADR-0005
// (what a row may name), SPEC-0003 REQ "Visibility", docs/client-contract.md
// ("Stage C").
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/providers"
	"gitea.stump.rocks/stump.wtf/burrow/internal/seal"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
	"gitea.stump.rocks/stump.wtf/burrow/internal/upstream"
)

// mcpRefreshSkew is how early burrow refreshes an access token it holds, so an
// agent's tool call does not arrive on the last second of its life.
const mcpRefreshSkew = 60 * time.Second

// maxMCPKey caps what a member may hand burrow as their own key. A real API key
// is a few hundred characters; anything longer is a paste of something else.
const maxMCPKey = 4096

// mcpConnOf builds one connection's listing entry. The field names are the
// client's (contract, "Stage C"); the credential behind the row is never among
// them, only whether burrow can use it and for whom.
func mcpConnOf(c store.MCPConnection, connected, connectedForMe bool, connectedAt time.Time) map[string]any {
	entry := map[string]any{
		"id":             c.ID,
		"name":           c.Name,
		"url":            c.URL,
		"authType":       c.Auth,
		"credentialMode": c.Cred,
		"exposeDirectly": c.Expose,
		"connected":      connected,
		"connectedForMe": connectedForMe,
	}
	if !connectedAt.IsZero() {
		entry["connectedAt"] = providers.FormatUpdatedAt(connectedAt)
	}
	return entry
}

// handleMCPConnections answers GET /v1/mcp-connections?scope=usable|manageable.
// scope=usable (the default) is the connections whose audience the member holds;
// scope=manageable is the connections they may edit, which is the admin surface
// the console reads. A member outside a connection's audience does not learn it
// exists.
func (s *Server) handleMCPConnections(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "usable"
	}
	if scope != "usable" && scope != "manageable" {
		writeError(w, http.StatusBadRequest, "invalid_request", `scope must be "usable" or "manageable"`)
		return
	}
	groups, err := s.memberGroups(r)
	if err != nil {
		slog.Error("mcp: group lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "member store unavailable")
		return
	}
	connections, err := s.store.ListMCPConnections(r.Context())
	if err != nil {
		slog.Error("mcp: list connections failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not list MCP connections")
		return
	}
	manageable := holdsAny(groups, s.cfg.AdminGroups)
	out := make([]map[string]any, 0, len(connections))
	for _, c := range connections {
		usable := mcpAudienceAllows(c.Groups, groups)
		if scope == "manageable" {
			if !manageable {
				continue
			}
		} else if !usable {
			continue
		}
		connected, connectedForMe, connectedAt := s.mcpConnState(r.Context(), c, userID(r), usable)
		out = append(out, mcpConnOf(c, connected, connectedForMe, connectedAt))
	}
	noStore(w)
	writeJSON(w, http.StatusOK, map[string]any{"connections": out})
}

// handleMCPConnectStart answers GET /v1/mcp-connections/:id/connect/start: either
// the connection is ready and the client has nothing to do, or it hands back the
// URL to open. A member's own API key needs no browser, so that case answers
// needs_auth with an empty authorizeUrl and the client prompts for the key
// (contract, "Stage C").
func (s *Server) handleMCPConnectStart(w http.ResponseWriter, r *http.Request) {
	c, ok := s.mcpConnUsable(w, r)
	if !ok {
		return
	}
	access, err := s.mcpAccess(r.Context(), c, userID(r))
	if err != nil {
		slog.Warn("mcp: connect start could not resolve the credential", "connection", c.ID, "error", err)
	}
	if access.Connected {
		writeJSON(w, http.StatusOK, map[string]any{"status": "connected"})
		return
	}
	if !c.UsesOAuth() {
		writeJSON(w, http.StatusOK, map[string]any{"status": "needs_auth", "authorizeUrl": ""})
		return
	}
	authURL, err := s.mcpAuthorizeURL(r, c)
	if err != nil {
		if errors.Is(err, upstream.ErrHostNotAllowed) {
			writeError(w, http.StatusConflict, "mcp_connection_misconfigured",
				"this connection's authorize URL is outside BURROW_MCP_URL_HOSTS")
			return
		}
		slog.Error("mcp: could not build an authorize URL", "connection", c.ID, "error", err)
		writeError(w, http.StatusConflict, "mcp_connection_misconfigured",
			"this OAuth connection is missing its authorization server details")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "needs_auth", "authorizeUrl": authURL})
}

// mcpAuthorizeURL mints the single-use state this connect is keyed to and builds
// the URL. The state is the only thing the browser carries back, so finishing a
// connect never needs the member's session to survive the round trip.
func (s *Server) mcpAuthorizeURL(r *http.Request, c store.MCPConnection) (string, error) {
	verifier, err := upstream.PKCEVerifier()
	if err != nil {
		return "", err
	}
	state := randomState()
	redirectURI := s.origin(r) + "/mcp-connections/" + url.PathEscape(c.ID) + "/oauth/callback"
	if err := s.store.SaveMCPOAuthState(r.Context(), state, c.ID, userID(r), verifier, s.cfg.MCPOAuthStateTTL); err != nil {
		return "", err
	}
	authURL, err := upstream.AuthorizeURL(s.mcpPolicy, c.OAuthAuthURL, c.OAuthClientID, redirectURI, state, verifier, c.OAuthScopes)
	if err != nil {
		// The state outlives nothing here: it is single-use and short, and the
		// purge removes it. Leaving it is cheaper than a second round trip to
		// delete it on a path that failed for a reason the operator must fix.
		return "", err
	}
	return authURL, nil
}

// handleMCPOAuthCallback is where the browser comes back. It is not a session
// route: the state is the whole of its proof, it is consumed on the first read,
// and an unknown or expired one is answered the way a forged one is.
func (s *Server) handleMCPOAuthCallback(w http.ResponseWriter, r *http.Request) {
	connectionID, memberID, verifier, err := s.store.TakeMCPOAuthState(r.Context(), r.URL.Query().Get("state"))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrExpired) {
			slog.Error("mcp: oauth state lookup failed", "error", err)
			s.renderMCPNotice(w, http.StatusInternalServerError, "burrow could not check that sign-in link. Start it again.")
			return
		}
		slog.Warn("mcp: oauth callback with an unknown, used or expired state")
		s.renderMCPNotice(w, http.StatusBadRequest, "That sign-in link has been used or has expired. Start the connection again.")
		return
	}
	if denied := r.URL.Query().Get("error"); denied != "" {
		slog.Info("mcp: connect declined at the authorization server", "connection", connectionID, "error", denied)
		s.renderMCPNotice(w, http.StatusOK, "The authorization server said no, so nothing was connected.")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.renderMCPNotice(w, http.StatusBadRequest, "That sign-in link carried no code. Start the connection again.")
		return
	}
	c, err := s.store.GetMCPConnection(r.Context(), connectionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.renderMCPNotice(w, http.StatusNotFound, "That connection no longer exists, so nothing was connected.")
			return
		}
		slog.Error("mcp: oauth callback could not read the connection", "error", err)
		s.renderMCPNotice(w, http.StatusInternalServerError, "burrow could not read that connection. Try again.")
		return
	}
	if _, err := s.mcpSealer(); err != nil {
		s.renderMCPNotice(w, http.StatusServiceUnavailable, "burrow cannot store a credential yet, so nothing was connected.")
		return
	}
	token, err := upstream.ExchangeCode(r.Context(), s.mcpUpstream.HTTP(), s.mcpPolicy, c.OAuthTokenURL, code, verifier,
		c.OAuthClientID, s.origin(r)+"/mcp-connections/"+url.PathEscape(c.ID)+"/oauth/callback")
	if err != nil {
		slog.Warn("mcp: code exchange failed", "connection", c.ID, "error", err)
		s.renderMCPNotice(w, http.StatusBadGateway, "The token exchange failed, so nothing was connected.")
		return
	}
	if err := s.saveMCPToken(r.Context(), c, memberID, token); err != nil {
		slog.Error("mcp: could not store the connected credential", "connection", c.ID, "error", err)
		s.renderMCPNotice(w, http.StatusInternalServerError, "burrow could not store that credential. Try again.")
		return
	}
	slog.Info("mcp: connected", "connection", c.ID, "member", memberID, "mode", c.Cred)
	s.renderMCPNotice(w, http.StatusOK, "Connected. You can close this page.")
}

// handleMCPPutMyCredential answers PUT /v1/mcp-connections/:id/my-credential: the
// member's own key for a per-member connection. It is sealed before it is
// stored, so a database read never hands out a usable secret, and a server
// without BURROW_MCP_CREDENTIAL_KEY refuses rather than writing one in the clear.
func (s *Server) handleMCPPutMyCredential(w http.ResponseWriter, r *http.Request) {
	c, ok := s.mcpConnUsable(w, r)
	if !ok {
		return
	}
	if !c.PerMember() {
		writeError(w, http.StatusConflict, "shared_credential",
			"this connection's key belongs to the org, not to a member")
		return
	}
	if c.UsesOAuth() {
		writeError(w, http.StatusConflict, "oauth_connection",
			"this connection is connected by signing in, not by a key you hold")
		return
	}
	var body struct {
		APIKey string `json:"apiKey"`
	}
	if err := decodeStrict(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be {apiKey}")
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "apiKey is empty")
		return
	}
	if len(key) > maxMCPKey {
		writeError(w, http.StatusBadRequest, "invalid_request", "apiKey is too long")
		return
	}
	sealer, err := s.mcpSealer()
	if err != nil {
		s.mcpSealingUnavailable(w)
		return
	}
	sealed, err := sealer.Seal(key)
	if err != nil {
		slog.Error("mcp: could not seal a member credential", "connection", c.ID, "error", err)
		s.mcpSealingUnavailable(w)
		return
	}
	if err := s.store.PutMCPCredential(r.Context(), store.MCPCredential{
		ConnectionID: c.ID, MemberID: userID(r), Kind: store.MCPAuthAPIKey,
		Hash: seal.Fingerprint(key), Sealed: sealed,
	}); err != nil {
		slog.Error("mcp: store member credential failed", "connection", c.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not store your credential")
		return
	}
	slog.Info("mcp: member credential stored", "connection", c.ID, "member", userID(r), "fingerprint", seal.Fingerprint(key)[:12])
	noStore(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMCPDisconnectMyAccount answers POST /v1/mcp-connections/:id/disconnect-my-account.
// It removes the caller's own credential and nothing else: on a per-member
// connection that is exactly "sign this connection out for me", and on a shared
// one it would sign everyone else out too, so it is refused with the route that
// does mean that.
func (s *Server) handleMCPDisconnectMyAccount(w http.ResponseWriter, r *http.Request) {
	c, ok := s.mcpConnUsable(w, r)
	if !ok {
		return
	}
	if !c.PerMember() {
		writeError(w, http.StatusConflict, "shared_credential",
			"this connection's credential is the org's; disconnect it from the admin surface")
		return
	}
	if err := s.store.DeleteMCPCredential(r.Context(), c.ID, userID(r)); err != nil {
		slog.Error("mcp: disconnect-my-account failed", "connection", c.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not disconnect")
		return
	}
	slog.Info("mcp: member disconnected", "connection", c.ID, "member", userID(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleOAuthProviderDisconnect answers POST /v1/oauth-providers/:id/disconnect:
// the org-wide disconnect of a shared credential. It is the admin's move, so the
// route is under the same gate as the admin surface, and it refuses a per-member
// connection because the answer there is a member's own disconnect route.
func (s *Server) handleOAuthProviderDisconnect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := s.store.GetMCPConnection(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such MCP connection")
			return
		}
		slog.Error("mcp: read connection for disconnect failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read that connection")
		return
	}
	if !c.UsesOAuth() || !c.Shared() {
		writeError(w, http.StatusConflict, "not_shared_oauth",
			"only a shared OAuth connection is disconnected here")
		return
	}
	if err := s.store.DeleteMCPCredential(r.Context(), c.ID, store.SharedSlot); err != nil {
		slog.Error("mcp: shared disconnect failed", "connection", c.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not disconnect")
		return
	}
	slog.Info("mcp: shared credential disconnected", "connection", c.ID, "actor", adminActor(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// The admin surface for connections, outside the client contract and under the
// same gate as the provider rows (SPEC-0004 REQ "Admin authorization"). A
// connection is a row that makes burrow speak to a host with a credential, so
// both of its choices are held to the allowlists before they are stored, and
// again every time they are used (ADR-0005's rule, applied to Stage C).

func (s *Server) handleAdminListMCPConnections(w http.ResponseWriter, r *http.Request) {
	connections, err := s.store.ListMCPConnections(r.Context())
	if err != nil {
		slog.Error("mcp: admin list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not list MCP connections")
		return
	}
	out := make([]map[string]any, 0, len(connections))
	for _, c := range connections {
		out = append(out, adminMCPConnOf(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": out})
}

func (s *Server) handleAdminCreateMCPConnection(w http.ResponseWriter, r *http.Request) {
	c, _, err := s.decodeMCPConn(w, r)
	if err != nil {
		return
	}
	if c.ID == "" {
		c.ID = store.NewMCPConnectionID()
	}
	at, err := s.store.CreateMCPConnection(r.Context(), c)
	if err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			writeError(w, http.StatusConflict, "duplicate_id", "an MCP connection with that id already exists")
			return
		}
		slog.Error("mcp: admin create failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not create the MCP connection")
		return
	}
	c.UpdatedAt = at
	slog.Info("mcp: connection created", "connection", c.ID, "actor", adminActor(r), "authType", c.Auth, "mode", c.Cred)
	s.mcpCache.Forget(c.ID)
	writeJSON(w, http.StatusCreated, adminMCPConnOf(c))
}

func (s *Server) handleAdminUpdateMCPConnection(w http.ResponseWriter, r *http.Request) {
	c, updatedAt, err := s.decodeMCPConn(w, r)
	if err != nil {
		return
	}
	c.ID = r.PathValue("id")
	// The timestamp is the one the editor read: an admin working from a stale
	// listing is refused instead of overwriting a change someone else saved.
	expect, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(updatedAt))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "updatedAt is required, the RFC 3339 timestamp you read")
		return
	}
	at, err := s.store.UpdateMCPConnection(r.Context(), c, expect)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrStale):
			writeError(w, http.StatusConflict, "stale", "that connection changed since you read it; read it again")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "no such MCP connection")
		default:
			slog.Error("mcp: admin update failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not update the MCP connection")
		}
		return
	}
	c.UpdatedAt = at
	slog.Info("mcp: connection updated", "connection", c.ID, "actor", adminActor(r))
	s.mcpCache.Forget(c.ID)
	writeJSON(w, http.StatusOK, adminMCPConnOf(c))
}

func (s *Server) handleAdminDeleteMCPConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteMCPConnection(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such MCP connection")
			return
		}
		slog.Error("mcp: admin delete failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not delete the MCP connection")
		return
	}
	slog.Info("mcp: connection deleted", "connection", id, "actor", adminActor(r))
	s.mcpCache.Forget(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// decodeMCPConn reads a connection the admin surface writes and checks every
// choice burrow will act on: the URL it will talk to, the environment variable a
// shared key may be named by, and an auth/credential mode pair that means
// something. It answers the row to store and the updatedAt the body carried,
// which only the update route compares against the row.
func (s *Server) decodeMCPConn(w http.ResponseWriter, r *http.Request) (store.MCPConnection, string, error) {
	var body struct {
		ID             string   `json:"id"`
		Name           string   `json:"name"`
		URL            string   `json:"url"`
		AuthType       string   `json:"authType"`
		CredentialMode string   `json:"credentialMode"`
		ExposeDirectly *bool    `json:"exposeDirectly"`
		APIKeyEnv      string   `json:"apiKeyEnv"`
		Groups         []string `json:"groups"`
		OAuthIssuer    string   `json:"oauthIssuer"`
		OAuthAuthURL   string   `json:"oauthAuthorizeUrl"`
		OAuthTokenURL  string   `json:"oauthTokenUrl"`
		OAuthClientID  string   `json:"oauthClientId"`
		OAuthScopes    string   `json:"oauthScopes"`
		UpdatedAt      string   `json:"updatedAt"`
	}
	if err := decodeStrict(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be one MCP connection object")
		return store.MCPConnection{}, "", err
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_request", "name is required and must be under 200 characters")
		return store.MCPConnection{}, "", errors.New("name")
	}
	auth := strings.ToLower(strings.TrimSpace(body.AuthType))
	if auth == "" {
		auth = store.MCPAuthNone
	}
	mode := strings.ToLower(strings.TrimSpace(body.CredentialMode))
	if mode == "" {
		mode = store.MCPCredShared
	}
	switch auth {
	case store.MCPAuthNone, store.MCPAuthAPIKey, store.MCPAuthOAuth:
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "authType must be oauth, apikey or none")
		return store.MCPConnection{}, "", errors.New("authType")
	}
	switch mode {
	case store.MCPCredShared, store.MCPCredPerUser:
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "credentialMode must be shared or per_member")
		return store.MCPConnection{}, "", errors.New("credentialMode")
	}
	if auth == store.MCPAuthNone && mode == store.MCPCredPerUser {
		writeError(w, http.StatusBadRequest, "invalid_request", "a connection with no auth has no per-member credential")
		return store.MCPConnection{}, "", errors.New("credentialMode")
	}
	endpoint, err := s.mcpPolicy.CheckURL(body.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_url", err.Error())
		return store.MCPConnection{}, "", err
	}
	c := store.MCPConnection{
		ID: strings.TrimSpace(body.ID), Name: name, URL: endpoint,
		Auth: auth, Cred: mode, Expose: body.ExposeDirectly != nil && *body.ExposeDirectly,
		APIKeyEnv:    strings.ToUpper(strings.TrimSpace(body.APIKeyEnv)),
		Groups:       body.Groups,
		OAuthIssuer:  strings.TrimSpace(body.OAuthIssuer),
		OAuthAuthURL: strings.TrimSpace(body.OAuthAuthURL), OAuthTokenURL: strings.TrimSpace(body.OAuthTokenURL),
		OAuthClientID: strings.TrimSpace(body.OAuthClientID), OAuthScopes: strings.TrimSpace(body.OAuthScopes),
	}
	if auth == store.MCPAuthAPIKey && mode == store.MCPCredShared {
		if !s.mcpKeyEnvAllowed(c.APIKeyEnv) {
			writeError(w, http.StatusBadRequest, "invalid_api_key_env",
				fmt.Sprintf("apiKeyEnv must start with one of %s", strings.Join(s.cfg.MCPKeyEnvPrefixes, ", ")))
			return store.MCPConnection{}, "", errors.New("apiKeyEnv")
		}
	}
	if auth == store.MCPAuthOAuth {
		if err := s.checkMCPOAuth(w, c); err != nil {
			return store.MCPConnection{}, "", err
		}
	}
	return c, body.UpdatedAt, nil
}

// checkMCPOAuth holds what an OAuth connection needs before a member can be sent
// to it: an authorization endpoint burrow is allowed to name, a token endpoint it
// is allowed to talk to, and a client id. burrow is a public PKCE client, so
// there is no secret to name and no secret in any row (ADR-0010).
func (s *Server) checkMCPOAuth(w http.ResponseWriter, c store.MCPConnection) error {
	if strings.TrimSpace(c.OAuthAuthURL) == "" || strings.TrimSpace(c.OAuthClientID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"an oauth connection needs an authorize URL and a client id")
		return errors.New("oauth")
	}
	if _, err := s.mcpPolicy.CheckURL(c.OAuthTokenURL); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_url", err.Error())
		return err
	}
	return nil
}

// mcpConnState answers the listing's three state fields for one member.
//
//   - connected is what the org has: an authType none connection is always
//     connected, a shared one is connected when burrow can present its shared
//     credential, and a per-member one is connected when somebody is.
//   - connectedForMe is whether this member can use it right now.
//   - connectedAt is when their own credential landed, falling back to the org's.
func (s *Server) mcpConnState(ctx context.Context, c store.MCPConnection, memberID string, usable bool) (connected, connectedForMe bool, connectedAt time.Time) {
	access, err := s.mcpAccess(ctx, c, memberID)
	if err != nil {
		slog.Debug("mcp: credential could not be resolved for the listing", "connection", c.ID, "error", err)
	}
	connectedForMe = usable && access.Connected
	if access.ConnectedAt.IsZero() {
		connectedAt = c.CreatedAt
	} else {
		connectedAt = access.ConnectedAt
	}
	switch {
	case !c.UsesOAuth() && !c.UsesAPIKey():
		connected = true
	case c.Shared():
		connected = access.Connected
	default:
		if creds, err := s.store.ListMCPConnectionCredentials(ctx, c.ID); err == nil {
			for _, cred := range creds {
				if !cred.ExpiresAt.IsZero() && time.Now().After(cred.ExpiresAt) {
					continue
				}
				connected = true
				connectedAt = cred.ConnectedAt
				break
			}
		}
	}
	return connected, connectedForMe, connectedAt
}

// mcpAccess is what burrow would present upstream for one connection and one
// member right now, and why it cannot if it cannot.
type mcpAccess struct {
	Connected   bool
	Present     upstream.Headers
	ConnectedAt time.Time
	// Fingerprint identifies the credential in the tool cache key; it is a hash,
	// never the value.
	Fingerprint string
}

func (s *Server) mcpAccess(ctx context.Context, c store.MCPConnection, memberID string) (mcpAccess, error) {
	switch {
	case c.UsesOAuth():
		return s.mcpOAuthAccess(ctx, c, memberID)
	case c.UsesAPIKey():
		if c.Shared() {
			key, ok := s.mcpSharedKey(c)
			if !ok {
				return mcpAccess{}, nil
			}
			return mcpAccess{Connected: true, Present: upstream.Headers{"Authorization": "Bearer " + key},
				ConnectedAt: c.UpdatedAt, Fingerprint: seal.Fingerprint(key)}, nil
		}
		cred, err := s.store.GetMCPCredential(ctx, c.ID, memberID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return mcpAccess{}, nil
			}
			return mcpAccess{}, err
		}
		key, err := s.openMCPCredential(cred)
		if err != nil {
			return mcpAccess{}, err
		}
		return mcpAccess{Connected: true, Present: upstream.Headers{"Authorization": "Bearer " + key},
			ConnectedAt: cred.ConnectedAt, Fingerprint: cred.Hash}, nil
	default:
		return mcpAccess{Connected: true, ConnectedAt: c.CreatedAt}, nil
	}
}

// mcpOAuthAccess reads the OAuth token for the right slot and refreshes it when
// it has come due. A refresh that fails leaves the row alone: the connection is
// simply not connected until someone signs in again, which is what the listing
// then says.
func (s *Server) mcpOAuthAccess(ctx context.Context, c store.MCPConnection, memberID string) (mcpAccess, error) {
	cred, err := s.store.GetMCPCredential(ctx, c.ID, c.Slot(memberID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return mcpAccess{}, nil
		}
		return mcpAccess{}, err
	}
	sealer, err := s.mcpSealer()
	if err != nil {
		return mcpAccess{}, err
	}
	access, err := sealer.Open(cred.Sealed)
	if err != nil {
		return mcpAccess{}, fmt.Errorf("mcp: credential for %s cannot be read: %w", c.ID, err)
	}
	if !cred.ExpiresAt.IsZero() && time.Now().Add(mcpRefreshSkew).After(cred.ExpiresAt) {
		refreshed, err := s.refreshMCPToken(ctx, c, memberID, cred)
		if err != nil {
			slog.Warn("mcp: refresh failed, treating the connection as not connected",
				"connection", c.ID, "member", memberID, "error", err)
			return mcpAccess{ConnectedAt: cred.ConnectedAt}, nil
		}
		return refreshed, nil
	}
	return mcpAccess{Connected: true, Present: upstream.Headers{"Authorization": bearerValue(access, cred.Kind)},
		ConnectedAt: cred.ConnectedAt, Fingerprint: cred.Hash}, nil
}

// refreshMCPToken swaps the stored refresh token for a fresh access token and
// keeps whatever the server rotated with it.
func (s *Server) refreshMCPToken(ctx context.Context, c store.MCPConnection, memberID string, cred store.MCPCredential) (mcpAccess, error) {
	sealer, err := s.mcpSealer()
	if err != nil {
		return mcpAccess{}, err
	}
	refresh, err := sealer.Open(cred.RefreshSealed)
	if err != nil {
		return mcpAccess{}, err
	}
	if refresh == "" {
		return mcpAccess{}, errors.New("mcp: the credential is expired and carries no refresh token")
	}
	token, err := upstream.Refresh(ctx, s.mcpUpstream.HTTP(), s.mcpPolicy, c.OAuthTokenURL, refresh, c.OAuthClientID)
	if err != nil {
		return mcpAccess{}, err
	}
	if err := s.saveMCPToken(ctx, c, c.Slot(memberID), token); err != nil {
		return mcpAccess{}, err
	}
	return mcpAccess{Connected: true, Present: upstream.Headers{"Authorization": "Bearer " + token.AccessToken},
		ConnectedAt: cred.ConnectedAt, Fingerprint: seal.Fingerprint(token.AccessToken)}, nil
}

// saveMCPToken seals and stores one OAuth token in the slot the connection's
// credential mode points at.
func (s *Server) saveMCPToken(ctx context.Context, c store.MCPConnection, memberID string, token upstream.Token) error {
	sealer, err := s.mcpSealer()
	if err != nil {
		return err
	}
	sealed, err := sealer.Seal(token.AccessToken)
	if err != nil {
		return err
	}
	refreshSealed, err := sealer.Seal(token.RefreshToken)
	if err != nil {
		return err
	}
	cred := store.MCPCredential{
		ConnectionID: c.ID, MemberID: c.Slot(memberID), Kind: store.MCPAuthOAuth,
		Hash: seal.Fingerprint(token.AccessToken), Sealed: sealed, RefreshSealed: refreshSealed,
	}
	if token.ExpiresIn > 0 {
		cred.ExpiresAt = time.Now().UTC().Add(token.ExpiresIn)
	}
	return s.store.PutMCPCredential(ctx, cred)
}

// openMCPCredential unseals a member's stored API key.
func (s *Server) openMCPCredential(cred store.MCPCredential) (string, error) {
	sealer, err := s.mcpSealer()
	if err != nil {
		return "", err
	}
	return sealer.Open(cred.Sealed)
}

// mcpSharedKey is the connection's org-wide key, read from the environment only
// when the row names a variable the deployment allows (ADR-0005).
func (s *Server) mcpSharedKey(c store.MCPConnection) (string, bool) {
	if !s.mcpKeyEnvAllowed(c.APIKeyEnv) {
		return "", false
	}
	value, ok := os.LookupEnv(c.APIKeyEnv)
	if !ok || value == "" {
		return "", false
	}
	return value, true
}

func (s *Server) mcpKeyEnvAllowed(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	for _, prefix := range s.cfg.MCPKeyEnvPrefixes {
		if strings.HasPrefix(name, strings.ToUpper(strings.TrimSpace(prefix))) {
			return true
		}
	}
	return false
}

// mcpSealer gives the sealer this process can use. Without
// BURROW_MCP_CREDENTIAL_KEY there is none, and storing or reading a member
// credential refuses instead of falling back to plaintext.
func (s *Server) mcpSealer() (*seal.Sealer, error) {
	if !s.mcpSeal.Configured() {
		return nil, seal.ErrNotConfigured
	}
	return s.mcpSeal, nil
}

func (s *Server) mcpSealingUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "mcp_credentials_unavailable",
		"burrow cannot store MCP credentials until BURROW_MCP_CREDENTIAL_KEY is set")
}

// mcpAudienceAllows is SPEC-0003's rule for a connection: a row that declares no
// audience is every member's, and one that does is held by whoever is in it.
func mcpAudienceAllows(audience, memberGroups []string) bool {
	if len(audience) == 0 {
		return true
	}
	return holdsAny(memberGroups, audience)
}

// mcpConnUsable loads the connection a member-facing route names and answers it
// as unknown when this member is outside its audience. The admin routes read the
// row under the admin gate instead: managing a connection is not being allowed
// to connect it or run its tools, so the audience — not the admin right — is
// what decides here (ADR-0010, SPEC-0003 REQ "Visibility").
func (s *Server) mcpConnUsable(w http.ResponseWriter, r *http.Request) (store.MCPConnection, bool) {
	c, err := s.store.GetMCPConnection(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such MCP connection")
			return c, false
		}
		slog.Error("mcp: read connection failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read that connection")
		return c, false
	}
	groups, err := s.memberGroups(r)
	if err != nil {
		slog.Error("mcp: group lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "member store unavailable")
		return c, false
	}
	if !mcpAudienceAllows(c.Groups, groups) {
		writeError(w, http.StatusNotFound, "not_found", "no such MCP connection")
		return c, false
	}
	return c, true
}

// renderMCPNotice is the page a browser gets back from an OAuth callback: one
// sentence, no assets, and nothing that says whether the member exists.
func (s *Server) renderMCPNotice(w http.ResponseWriter, status int, message string) {
	privatePage(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>burrow</title>"+
		"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"></head>"+
		"<body style=\"font-family:system-ui,sans-serif;margin:3rem auto;max-width:34rem\">"+
		"<h1>burrow</h1><p>%s</p></body></html>\n", htmlEscape(message))
}

func htmlEscape(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return replacer.Replace(s)
}

// adminMCPConnOf is the admin view of a row: the listing's fields plus everything
// an editor needs to see and change, and still no credential value.
func adminMCPConnOf(c store.MCPConnection) map[string]any {
	return map[string]any{
		"id": c.ID, "name": c.Name, "url": c.URL,
		"authType": c.Auth, "credentialMode": c.Cred, "exposeDirectly": c.Expose,
		"apiKeyEnv": c.APIKeyEnv, "groups": c.Groups,
		"oauthIssuer": c.OAuthIssuer, "oauthAuthorizeUrl": c.OAuthAuthURL, "oauthTokenUrl": c.OAuthTokenURL,
		"oauthClientId": c.OAuthClientID, "oauthScopes": c.OAuthScopes,
		"updatedAt": providers.FormatUpdatedAt(c.UpdatedAt),
	}
}

// bearerValue renders the Authorization header for a stored credential. Both
// kinds burrow stores are bearer values; the kind is carried so a caller can tell
// them apart without decrypting anything.
func bearerValue(value, kind string) string {
	if kind == store.MCPAuthOAuth || kind == store.MCPAuthAPIKey {
		return "Bearer " + value
	}
	return value
}

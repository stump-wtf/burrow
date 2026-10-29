// Shared MCP connections (Stage C, stump.wtf/burrow#4): the org's upstream MCP
// servers, the credentials they are connected with, and the OAuth connects in
// flight.
//
// A connection row carries no secret. A shared API key is named by its
// environment variable and read from the environment where it is already kept
// (ADR-0005's rule for a provider row, for the same reason: a writable row that
// names an arbitrary variable turns a write into a read of anything the process
// holds); an OAuth token is stored, sealed, in mcp_credentials. A member's own
// key is stored the same way, so it is the only kind of credential a member can
// write.
//
// Governing: ADR-0010 (shared MCP connections), ADR-0005 (what a row may name),
// SPEC-0003 REQ "Visibility" (the audience shape mcp_groups follows).
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Auth and credential modes are stored as the client contract spells them, so a
// listing needs no translation and a row reads the same in the database as it
// answers on the wire.
const (
	MCPAuthOAuth   = "oauth"
	MCPAuthAPIKey  = "apikey"
	MCPAuthNone    = "none"
	MCPCredShared  = "shared"
	MCPCredPerUser = "per_member"
)

// SharedSlot is the member id of a connection's org-wide credential row. A
// per-member credential is stored under the member's own id.
const SharedSlot = ""

// MCPConnection is one shared MCP connection as stored. Groups follows the
// provider rows: nil or empty means every member, otherwise the members holding
// one of the listed groups.
type MCPConnection struct {
	ID     string
	Name   string
	URL    string
	Auth   string // MCPAuthOAuth, MCPAuthAPIKey or MCPAuthNone
	Cred   string // MCPCredShared or MCPCredPerUser
	Expose bool   // the contract's exposeDirectly
	// APIKeyEnv names the environment variable holding this connection's shared
	// key; only meaningful for an apikey connection in shared mode, and only
	// when it is on the allowlist the write gate applies.
	APIKeyEnv string
	Groups    []string

	// OAuth settings, for an oauth connection. burrow is a public PKCE client
	// and carries no client secret for any of them (ADR-0010).
	OAuthIssuer   string
	OAuthAuthURL  string
	OAuthTokenURL string
	OAuthClientID string
	OAuthScopes   string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// UsesOAuth and UsesAPIKey keep the mode tests out of the handlers.
func (c MCPConnection) UsesOAuth() bool  { return c.Auth == MCPAuthOAuth }
func (c MCPConnection) UsesAPIKey() bool { return c.Auth == MCPAuthAPIKey }
func (c MCPConnection) Shared() bool     { return c.Cred == MCPCredShared }
func (c MCPConnection) PerMember() bool  { return c.Cred == MCPCredPerUser }

// Slot is the credential row this member's use of this connection reads: their
// own row for a per-member connection, the org's shared row otherwise.
func (c MCPConnection) Slot(memberID string) string {
	if c.PerMember() {
		return memberID
	}
	return SharedSlot
}

// MCPCredential is one stored credential. Sealed and RefreshSealed are opaque to
// the store: whoever wrote them sealed them, and only that code can read them
// back. Hash is the credential's SHA-256, which identifies it in a log line
// without carrying it.
type MCPCredential struct {
	ConnectionID  string
	MemberID      string
	Kind          string // MCPAuthAPIKey or MCPAuthOAuth
	Hash          string
	Sealed        string
	RefreshSealed string
	ExpiresAt     time.Time // zero means the credential does not expire
	ConnectedAt   time.Time
	UpdatedAt     time.Time
}

const mcpConnectionColumns = `id, name, url, auth_type, credential_mode, expose_directly, api_key_env,
	mcp_groups, oauth_issuer, oauth_auth_url, oauth_token_url, oauth_client_id, oauth_scopes,
	created_at, updated_at`

func scanMCPConnection(row rowScanner) (MCPConnection, error) {
	var (
		c      MCPConnection
		groups string
	)
	err := row.Scan(&c.ID, &c.Name, &c.URL, &c.Auth, &c.Cred, &c.Expose, &c.APIKeyEnv,
		&groups, &c.OAuthIssuer, &c.OAuthAuthURL, &c.OAuthTokenURL, &c.OAuthClientID, &c.OAuthScopes,
		&c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return MCPConnection{}, err
	}
	if c.Groups, err = decodeGroups(groups); err != nil {
		return MCPConnection{}, err
	}
	return c, nil
}

// NewMCPConnectionID mints an id for a connection the console or the admin API
// creates. The client treats these as opaque (ADR-0010); the mcc_ prefix is
// there so an id in a log line says what kind of row it is.
func NewMCPConnectionID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		panic("store: crypto/rand unavailable: " + err.Error())
	}
	return "mcc_" + hex.EncodeToString(buf)
}

// CreateMCPConnection inserts a connection row and returns when it landed. An
// id already taken is ErrDuplicate, never a silent overwrite: a connection
// carries credentials, and a create that replaced one would orphan them.
func (s *Store) CreateMCPConnection(ctx context.Context, c MCPConnection) (time.Time, error) {
	if c.ID == "" {
		c.ID = NewMCPConnectionID()
	}
	groups, err := encodeGroups(c.Groups)
	if err != nil {
		return time.Time{}, err
	}
	at := now()
	tag, err := s.db.ExecContext(ctx, `
INSERT INTO mcp_connections (`+mcpConnectionColumns+`)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)
ON CONFLICT (id) DO NOTHING`,
		c.ID, c.Name, c.URL, c.Auth, c.Cred, c.Expose, c.APIKeyEnv,
		groups, c.OAuthIssuer, c.OAuthAuthURL, c.OAuthTokenURL, c.OAuthClientID, c.OAuthScopes, at)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: create mcp connection: %w", err)
	}
	if n, err := tag.RowsAffected(); err != nil {
		return time.Time{}, fmt.Errorf("store: create mcp connection rows: %w", err)
	} else if n == 0 {
		return time.Time{}, fmt.Errorf("%w: mcp connection %s", ErrDuplicate, c.ID)
	}
	return at, nil
}

// UpdateMCPConnection writes a connection row whole, and refuses when the row
// changed since expectUpdatedAt: an editor working from a stale listing would
// otherwise drop a credential change someone made in between.
func (s *Store) UpdateMCPConnection(ctx context.Context, c MCPConnection, expectUpdatedAt time.Time) (time.Time, error) {
	groups, err := encodeGroups(c.Groups)
	if err != nil {
		return time.Time{}, err
	}
	at := now()
	tag, err := s.db.ExecContext(ctx, `
UPDATE mcp_connections
SET name = $2, url = $3, auth_type = $4, credential_mode = $5, expose_directly = $6,
	api_key_env = $7, mcp_groups = $8, oauth_issuer = $9, oauth_auth_url = $10,
	oauth_token_url = $11, oauth_client_id = $12, oauth_scopes = $13, updated_at = $14
WHERE id = $1 AND updated_at = $15`,
		c.ID, c.Name, c.URL, c.Auth, c.Cred, c.Expose, c.APIKeyEnv,
		groups, c.OAuthIssuer, c.OAuthAuthURL, c.OAuthTokenURL, c.OAuthClientID, c.OAuthScopes, at, expectUpdatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: update mcp connection: %w", err)
	}
	if n, err := tag.RowsAffected(); err != nil {
		return time.Time{}, fmt.Errorf("store: update mcp connection rows: %w", err)
	} else if n == 0 {
		return time.Time{}, ErrStale
	}
	return at, nil
}

// GetMCPConnection fetches one connection by id.
func (s *Store) GetMCPConnection(ctx context.Context, id string) (MCPConnection, error) {
	c, err := scanMCPConnection(s.db.QueryRowContext(ctx,
		`SELECT `+mcpConnectionColumns+` FROM mcp_connections WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MCPConnection{}, ErrNotFound
		}
		return MCPConnection{}, fmt.Errorf("store: get mcp connection: %w", err)
	}
	return c, nil
}

// ListMCPConnections returns every connection, oldest first. Who may see one is
// decided by the caller's audience (SPEC-0003 REQ "Visibility"), not here.
func (s *Store) ListMCPConnections(ctx context.Context) ([]MCPConnection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mcpConnectionColumns+` FROM mcp_connections ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list mcp connections: %w", err)
	}
	defer rows.Close()
	var out []MCPConnection
	for rows.Next() {
		c, err := scanMCPConnection(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan mcp connection: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteMCPConnection removes a connection and, through the schema's cascade,
// every credential and pending connect that referenced it.
func (s *Store) DeleteMCPConnection(ctx context.Context, id string) error {
	tag, err := s.db.ExecContext(ctx, `DELETE FROM mcp_connections WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete mcp connection: %w", err)
	}
	if n, err := tag.RowsAffected(); err != nil {
		return fmt.Errorf("store: delete mcp connection rows: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PutMCPCredential stores (or replaces) one credential. The caller sealed it:
// this table never holds a credential in the clear, and a server that cannot
// seal never calls this.
func (s *Store) PutMCPCredential(ctx context.Context, cred MCPCredential) error {
	at := now()
	var expires any
	if !cred.ExpiresAt.IsZero() {
		expires = cred.ExpiresAt
	}
	if cred.ConnectedAt.IsZero() {
		cred.ConnectedAt = at
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO mcp_credentials (connection_id, member_id, kind, credential_hash, credential_sealed,
	refresh_sealed, expires_at, connected_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
ON CONFLICT (connection_id, member_id) DO UPDATE SET kind = EXCLUDED.kind,
	credential_hash = EXCLUDED.credential_hash, credential_sealed = EXCLUDED.credential_sealed,
	refresh_sealed = EXCLUDED.refresh_sealed, expires_at = EXCLUDED.expires_at, updated_at = EXCLUDED.updated_at`,
		cred.ConnectionID, cred.MemberID, cred.Kind, cred.Hash, cred.Sealed, cred.RefreshSealed, expires, cred.ConnectedAt)
	if err != nil {
		return fmt.Errorf("store: put mcp credential: %w", err)
	}
	return nil
}

// GetMCPCredential reads one credential row. slot is what MCPConnection.Slot
// answers, so the caller does not repeat the mode test. ErrNotFound covers both
// a connection nobody has connected and a member who has not connected themself.
func (s *Store) GetMCPCredential(ctx context.Context, connectionID, slot string) (MCPCredential, error) {
	var (
		cred    MCPCredential
		expires sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `
SELECT connection_id, member_id, kind, credential_hash, credential_sealed, refresh_sealed,
	expires_at, connected_at, updated_at
FROM mcp_credentials WHERE connection_id = $1 AND member_id = $2`, connectionID, slot).
		Scan(&cred.ConnectionID, &cred.MemberID, &cred.Kind, &cred.Hash, &cred.Sealed, &cred.RefreshSealed,
			&expires, &cred.ConnectedAt, &cred.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MCPCredential{}, ErrNotFound
		}
		return MCPCredential{}, fmt.Errorf("store: get mcp credential: %w", err)
	}
	if expires.Valid {
		cred.ExpiresAt = expires.Time
	}
	return cred, nil
}

// ListMCPConnectionCredentials returns every credential of one connection, which
// is what asks whether a per-member connection is connected for anybody.
func (s *Store) ListMCPConnectionCredentials(ctx context.Context, connectionID string) ([]MCPCredential, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT connection_id, member_id, kind, credential_hash, credential_sealed, refresh_sealed,
	expires_at, connected_at, updated_at
FROM mcp_credentials WHERE connection_id = $1 ORDER BY member_id`, connectionID)
	if err != nil {
		return nil, fmt.Errorf("store: list mcp credentials: %w", err)
	}
	defer rows.Close()
	var out []MCPCredential
	for rows.Next() {
		var (
			cred    MCPCredential
			expires sql.NullTime
		)
		if err := rows.Scan(&cred.ConnectionID, &cred.MemberID, &cred.Kind, &cred.Hash, &cred.Sealed,
			&cred.RefreshSealed, &expires, &cred.ConnectedAt, &cred.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan mcp credential: %w", err)
		}
		if expires.Valid {
			cred.ExpiresAt = expires.Time
		}
		out = append(out, cred)
	}
	return out, rows.Err()
}

// DeleteMCPCredential drops one credential row: a member disconnecting their own
// account, or an admin dropping the org's shared one. Dropping what is not
// there is a no-op, so a double disconnect is not an error the client has to
// understand.
func (s *Store) DeleteMCPCredential(ctx context.Context, connectionID, slot string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM mcp_credentials WHERE connection_id = $1 AND member_id = $2`, connectionID, slot); err != nil {
		return fmt.Errorf("store: delete mcp credential: %w", err)
	}
	return nil
}

// SaveMCPOAuthState records a connect in flight. The state is the whole of the
// browser's proof that it belongs to this member's connect, so it is random and
// single-use like every other handoff burrow mints.
func (s *Store) SaveMCPOAuthState(ctx context.Context, state, connectionID, memberID, verifier string, ttl time.Duration) error {
	at := now()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO mcp_oauth_states (state, connection_id, member_id, code_verifier, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`,
		state, connectionID, memberID, verifier, at.Add(ttl), at)
	if err != nil {
		return fmt.Errorf("store: save mcp oauth state: %w", err)
	}
	return nil
}

// TakeMCPOAuthState consumes a state: one read, and it is gone. An unknown,
// used or expired state is ErrNotFound, which the callback answers the same way
// it would answer a forged one.
func (s *Store) TakeMCPOAuthState(ctx context.Context, state string) (connectionID, memberID, verifier string, err error) {
	var expires time.Time
	if err := s.db.QueryRowContext(ctx, `
SELECT connection_id, member_id, code_verifier, expires_at FROM mcp_oauth_states WHERE state = $1`, state).
		Scan(&connectionID, &memberID, &verifier, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", "", ErrNotFound
		}
		return "", "", "", fmt.Errorf("store: take mcp oauth state: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM mcp_oauth_states WHERE state = $1`, state); err != nil {
		return "", "", "", fmt.Errorf("store: consume mcp oauth state: %w", err)
	}
	if !expires.After(now()) {
		return "", "", "", ErrExpired
	}
	return connectionID, memberID, verifier, nil
}

// Package store is burrow's Postgres persistence layer.
//
// Governing: ADR-0002 (Postgres on the shared cluster, superseding ADR-0001's
// SQLite choice), ADR-0001 (what the control plane must remember), ADR-0004
// (provider rows live here; model menus are derived from the gateway). Everything
// the control plane must remember across restarts lives here: users, sessions,
// sign-in grants, OIDC flow states, MCP tokens, LLM provider rows with their
// audit trail, admin API tokens, and the Library's group-to-skill mappings.
// Tokens and grants are stored only as SHA-256 hashes; the raw values exist
// once, in the response that carries them to the client.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// User is an identity established through OIDC sign-in.
type User struct {
	ID    string // the OIDC subject; opaque to the client
	Email string
	Name  string
	// Groups is the member's OIDC groups claim as of their last sign-in
	// (SPEC-0003 REQ "Groups claim"). Never nil once read from the store.
	Groups []string
}

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// Store wraps the Postgres pool.
type Store struct {
	db *sql.DB
}

// Open connects to the Postgres database named by dsn and applies the schema.
// The schema statements are idempotent, so every start converges it; burrow is
// one service with one schema and carries no migration history.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// The cluster is shared with the rest of the edge stack, so burrow keeps a
	// small pool and hands idle connections back rather than holding them.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the pool.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS users (
	id         TEXT PRIMARY KEY,
	email      TEXT NOT NULL UNIQUE,
	name       TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS grants (
	token_hash TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS oidc_states (
	state      TEXT PRIMARY KEY,
	verifier   TEXT NOT NULL,
	desktop    BOOLEAN NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL
);
-- SPEC-0003 REQ "Groups claim": the member's groups as of their last sign-in,
-- a JSON array. Named oidc_groups because GROUPS is a reserved word.
ALTER TABLE users ADD COLUMN IF NOT EXISTS oidc_groups TEXT NOT NULL DEFAULT '[]';
CREATE TABLE IF NOT EXISTS mcp_tokens (
	token_hash TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	org_id     TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL
);
-- SPEC-0004 REQ "Rows in the store": provider rows, source of truth for the
-- desktop-facing set. provider_groups is NULL for every member and a JSON
-- array otherwise (an empty array is invalid, so it never lands). Named
-- provider_groups because GROUPS is a reserved word. name_overrides is a JSON
-- object mapping derived model ids to display names.
CREATE TABLE IF NOT EXISTS llm_providers (
	id              TEXT PRIMARY KEY,
	provider_id     TEXT NOT NULL,
	name            TEXT NOT NULL,
	base_url        TEXT NOT NULL,
	api_key_env     TEXT NOT NULL,
	provider_groups TEXT,
	name_overrides  TEXT NOT NULL DEFAULT '{}',
	created_at      TIMESTAMPTZ NOT NULL,
	updated_at      TIMESTAMPTZ NOT NULL
);
-- SPEC-0004 REQ "Audit": one row per create/update/delete, written in the
-- same transaction as the change. before/after carry the full row as JSON;
-- rows name environment variables, never secret values.
CREATE TABLE IF NOT EXISTS llm_provider_audit (
	id          BIGSERIAL PRIMARY KEY,
	created_at  TIMESTAMPTZ NOT NULL,
	actor       TEXT NOT NULL,
	action      TEXT NOT NULL,
	provider_id TEXT NOT NULL,
	before      TEXT,
	after       TEXT
);
CREATE INDEX IF NOT EXISTS llm_provider_audit_provider_idx ON llm_provider_audit (provider_id);
-- SPEC-0004 REQ "Admin authorization": machine tokens for automation, stored
-- only as SHA-256 hashes like every other credential burrow carries.
CREATE TABLE IF NOT EXISTS api_tokens (
	token_hash   TEXT PRIMARY KEY,
	user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	scopes       TEXT NOT NULL,
	created_at   TIMESTAMPTZ NOT NULL,
	expires_at   TIMESTAMPTZ NOT NULL,
	last_used_at TIMESTAMPTZ
);
-- SPEC-0003 REQ "Library audience": group to skill mappings. A row in
-- skill_packs is a group that has declared a rule; its allowance is the rows in
-- skill_pack_skills. A group with no row in skill_packs has no rule, which is
-- exactly the behavior before this feature — every member sees the whole
-- catalog. Named pack_group because GROUP is a reserved word.
CREATE TABLE IF NOT EXISTS skill_packs (
	pack_group TEXT PRIMARY KEY,
	created_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS skill_pack_skills (
	pack_group TEXT NOT NULL REFERENCES skill_packs(pack_group) ON DELETE CASCADE,
	skill      TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (pack_group, skill)
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

func now() time.Time { return time.Now().UTC() }

// randomToken returns a fresh 43-character base64url secret (the client
// accepts pasted grants of 12+ characters; 43 keeps every grant well clear).
func randomToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic("store: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// UpsertUser records a user identified by their OIDC subject, refreshing the
// email, name and groups claims on every sign-in. Groups are replaced, not
// merged: the claim is the whole truth as of this sign-in (SPEC-0003).
func (s *Store) UpsertUser(ctx context.Context, u User) error {
	groups, err := encodeGroups(u.Groups)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO users (id, email, name, oidc_groups, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $5)
ON CONFLICT (id) DO UPDATE SET email = EXCLUDED.email, name = EXCLUDED.name,
	oidc_groups = EXCLUDED.oidc_groups, updated_at = EXCLUDED.updated_at`,
		u.ID, u.Email, u.Name, groups, now())
	if err != nil {
		return fmt.Errorf("store: upsert user: %w", err)
	}
	return nil
}

// GetUser fetches one user by id.
func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	var u User
	var groups string
	err := s.db.QueryRowContext(ctx, `SELECT id, email, name, oidc_groups FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.Email, &u.Name, &groups)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("store: get user: %w", err)
	}
	if u.Groups, err = decodeGroups(groups); err != nil {
		return User{}, err
	}
	return u, nil
}

// ListUsers returns every user, oldest first (the /v1/org member directory).
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, email, name, oidc_groups FROM users ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		var groups string
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &groups); err != nil {
			return nil, fmt.Errorf("store: scan user: %w", err)
		}
		if u.Groups, err = decodeGroups(groups); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// encodeGroups stores groups as a JSON array; nil and empty both store "[]".
func encodeGroups(groups []string) (string, error) {
	if groups == nil {
		groups = []string{}
	}
	raw, err := json.Marshal(groups)
	if err != nil {
		return "", fmt.Errorf("store: encode groups: %w", err)
	}
	return string(raw), nil
}

// decodeGroups reads the JSON array back, never returning nil.
func decodeGroups(raw string) ([]string, error) {
	groups := []string{}
	if err := json.Unmarshal([]byte(raw), &groups); err != nil {
		return nil, fmt.Errorf("store: decode groups: %w", err)
	}
	return groups, nil
}

// MintGrant issues a single-use sign-in grant for user, valid for ttl.
// It returns the raw grant value — the only time it exists in the clear.
func (s *Store) MintGrant(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	grant := randomToken()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO grants (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
		hashToken(grant), userID, now(), now().Add(ttl))
	if err != nil {
		return "", fmt.Errorf("store: mint grant: %w", err)
	}
	return grant, nil
}

// ErrGrantInvalid marks a grant that is unknown, already used, or expired.
var ErrGrantInvalid = errors.New("store: grant is invalid, used, or expired")

// ConsumeGrant validates and burns a grant in one statement, returning the
// user it was minted for. The DELETE is the read: a second call with the same
// grant finds no row, so no interleaving of two requests can spend a grant
// twice. An expired row is deleted and answered the same way a reused one is.
func (s *Store) ConsumeGrant(ctx context.Context, grant string) (string, error) {
	var userID string
	var expiresAt time.Time
	err := s.db.QueryRowContext(ctx,
		`DELETE FROM grants WHERE token_hash = $1 RETURNING user_id, expires_at`, hashToken(grant)).
		Scan(&userID, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrGrantInvalid
		}
		return "", fmt.Errorf("store: consume grant: %w", err)
	}
	if now().After(expiresAt) {
		return "", ErrGrantInvalid
	}
	return userID, nil
}

// CreateSession issues a session token for user, valid for ttl, returning the
// raw token value.
func (s *Store) CreateSession(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	token := randomToken()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
		hashToken(token), userID, now(), now().Add(ttl))
	if err != nil {
		return "", fmt.Errorf("store: create session: %w", err)
	}
	return token, nil
}

// ErrExpired marks a token that exists but is no longer valid.
var ErrExpired = errors.New("store: token expired")

// LookupSession resolves a session token to its user, distinguishing an
// unknown token from an expired one so the API can answer with the exact
// error code the desktop client expects.
func (s *Store) LookupSession(ctx context.Context, token string) (userID string, err error) {
	var expiresAt time.Time
	err = s.db.QueryRowContext(ctx, `SELECT user_id, expires_at FROM sessions WHERE token_hash = $1`, hashToken(token)).
		Scan(&userID, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("store: lookup session: %w", err)
	}
	if now().After(expiresAt) {
		return "", ErrExpired
	}
	return userID, nil
}

// DeleteSession removes a session (sign-out).
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hashToken(token))
	if err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// SaveOIDCState records the PKCE verifier for one in-flight sign-in.
func (s *Store) SaveOIDCState(ctx context.Context, state, verifier string, desktop bool, ttl time.Duration) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO oidc_states (state, verifier, desktop, created_at, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		state, verifier, desktop, now(), now().Add(ttl))
	if err != nil {
		return fmt.Errorf("store: save oidc state: %w", err)
	}
	return nil
}

// TakeOIDCState atomically reads and removes one sign-in state: the DELETE
// returns the row it removed, so a replayed callback finds nothing.
func (s *Store) TakeOIDCState(ctx context.Context, state string) (verifier string, desktop bool, err error) {
	var expiresAt time.Time
	err = s.db.QueryRowContext(ctx,
		`DELETE FROM oidc_states WHERE state = $1 RETURNING verifier, desktop, expires_at`, state).
		Scan(&verifier, &desktop, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, ErrNotFound
		}
		return "", false, fmt.Errorf("store: take oidc state: %w", err)
	}
	if now().After(expiresAt) {
		return "", false, ErrNotFound
	}
	return verifier, desktop, nil
}

// MintMCPToken issues an org-scoped MCP token valid for ttl.
func (s *Store) MintMCPToken(ctx context.Context, userID, orgID string, ttl time.Duration) (string, time.Time, error) {
	token := randomToken()
	expiresAt := now().Add(ttl)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mcp_tokens (token_hash, user_id, org_id, created_at, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		hashToken(token), userID, orgID, now(), expiresAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("store: mint mcp token: %w", err)
	}
	return token, expiresAt, nil
}

// LookupMCPToken resolves an MCP token to its user and org.
func (s *Store) LookupMCPToken(ctx context.Context, token string) (userID, orgID string, err error) {
	var expiresAt time.Time
	err = s.db.QueryRowContext(ctx, `SELECT user_id, org_id, expires_at FROM mcp_tokens WHERE token_hash = $1`, hashToken(token)).
		Scan(&userID, &orgID, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrNotFound
		}
		return "", "", fmt.Errorf("store: lookup mcp token: %w", err)
	}
	if now().After(expiresAt) {
		return "", "", ErrExpired
	}
	return userID, orgID, nil
}

// ProviderRow is one LLM provider row as stored (SPEC-0004 REQ "Rows in the
// store"). It carries what burrow owns — identity, gateway location, the key's
// environment variable, the audience, and model display-name overrides. Model
// menus themselves are derived from the gateway at request time (ADR-0004)
// and never stored.
type ProviderRow struct {
	ID            string
	ProviderID    string
	Name          string
	BaseURL       string
	APIKeyEnv     string
	Groups        []string // nil means every member (SPEC-0003 REQ "Visibility")
	NameOverrides map[string]string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// AuditEntry is one llm_provider_audit row (SPEC-0004 REQ "Audit"). Before
// and After carry the full provider row as JSON, or nil for create/delete
// ends; secrets never appear because rows only name environment variables.
type AuditEntry struct {
	ID         int64
	CreatedAt  time.Time
	Actor      string
	Action     string
	ProviderID string
	Before     json.RawMessage
	After      json.RawMessage
}

// ErrDuplicate marks a create whose id already exists.
var ErrDuplicate = errors.New("store: row already exists")

// ErrStale marks an update whose precondition failed: the row changed since
// the caller last read it, so its expected updated_at no longer matches. The
// admin surface answers this 412 with the current row (SPEC-0004 design:
// "ETag = quoted updated_at").
var ErrStale = errors.New("store: row changed since it was read")

const providerColumns = `id, provider_id, name, base_url, api_key_env, provider_groups, name_overrides, created_at, updated_at`

func scanProvider(row rowScanner) (ProviderRow, error) {
	var (
		p      ProviderRow
		groups sql.NullString
		overs  string
	)
	if err := row.Scan(&p.ID, &p.ProviderID, &p.Name, &p.BaseURL, &p.APIKeyEnv,
		&groups, &overs, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return ProviderRow{}, err
	}
	if groups.Valid {
		if err := json.Unmarshal([]byte(groups.String), &p.Groups); err != nil {
			return ProviderRow{}, fmt.Errorf("store: decode provider groups: %w", err)
		}
	}
	p.NameOverrides = map[string]string{}
	if err := json.Unmarshal([]byte(overs), &p.NameOverrides); err != nil {
		return ProviderRow{}, fmt.Errorf("store: decode name overrides: %w", err)
	}
	return p, nil
}

// rowScanner is shared by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func encodeProviderGroups(groups []string) any {
	if groups == nil {
		return nil // NULL: every member
	}
	raw, err := json.Marshal(groups)
	if err != nil {
		// []string never fails to marshal.
		return "[]"
	}
	return string(raw)
}

func encodeNameOverrides(overs map[string]string) string {
	if overs == nil {
		overs = map[string]string{}
	}
	raw, err := json.Marshal(overs)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// providerJSON is the audit body for one row: the full stored row as JSON,
// timestamped fields included so incident review can order changes exactly.
func providerJSON(p ProviderRow) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"id":            p.ID,
		"providerId":    p.ProviderID,
		"name":          p.Name,
		"baseURL":       p.BaseURL,
		"apiKeyEnv":     p.APIKeyEnv,
		"groups":        p.Groups,
		"nameOverrides": p.NameOverrides,
		"createdAt":     p.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updatedAt":     p.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return json.RawMessage(`null`)
	}
	return raw
}

// CreateProvider inserts a row and appends its audit entry in one transaction
// (SPEC-0004 REQ "Audit"). The caller validates; the store guarantees the
// audit cannot desync from the change. Returns ErrDuplicate when the id
// exists.
func (s *Store) CreateProvider(ctx context.Context, p ProviderRow, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin create provider: %w", err)
	}
	defer tx.Rollback()
	created := p.CreatedAt
	if created.IsZero() {
		created = now()
	}
	tag, err := tx.ExecContext(ctx, `
INSERT INTO llm_providers (id, provider_id, name, base_url, api_key_env, provider_groups, name_overrides, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8) ON CONFLICT (id) DO NOTHING`,
		p.ID, p.ProviderID, p.Name, p.BaseURL, p.APIKeyEnv,
		encodeProviderGroups(p.Groups), encodeNameOverrides(p.NameOverrides), created)
	if err != nil {
		return fmt.Errorf("store: create provider: %w", err)
	}
	affected, err := tag.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: create provider rows: %w", err)
	}
	if affected == 0 {
		return ErrDuplicate
	}
	p.CreatedAt, p.UpdatedAt = created, created
	if err := insertAudit(ctx, tx, actor, "create", p.ID, nil, providerJSON(p)); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateProvider replaces every mutable field of one row and appends its
// audit entry in one transaction, returning the new updated_at. A non-zero
// expectUpdatedAt is the precondition behind the admin surface's etag: the
// check runs inside the row lock, so two writers racing on the same stale
// etag cannot both land — the loser gets ErrStale instead of clobbering the
// winner. It returns ErrNotFound when the row does not exist.
func (s *Store) UpdateProvider(ctx context.Context, p ProviderRow, expectUpdatedAt time.Time, actor string) (time.Time, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: begin update provider: %w", err)
	}
	defer tx.Rollback()
	before, err := getProviderTx(ctx, tx, p.ID)
	if err != nil {
		return time.Time{}, err
	}
	if !expectUpdatedAt.IsZero() && !before.UpdatedAt.Equal(expectUpdatedAt) {
		return time.Time{}, ErrStale
	}
	updated := now()
	if err := tx.QueryRowContext(ctx, `
UPDATE llm_providers
SET provider_id = $2, name = $3, base_url = $4, api_key_env = $5, provider_groups = $6, name_overrides = $7, updated_at = $8
WHERE id = $1 RETURNING updated_at`,
		p.ID, p.ProviderID, p.Name, p.BaseURL, p.APIKeyEnv,
		encodeProviderGroups(p.Groups), encodeNameOverrides(p.NameOverrides), updated).
		Scan(&updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, ErrNotFound
		}
		return time.Time{}, fmt.Errorf("store: update provider: %w", err)
	}
	p.CreatedAt, p.UpdatedAt = before.CreatedAt, updated
	after := p
	if err := insertAudit(ctx, tx, actor, "update", p.ID, providerJSON(before), providerJSON(after)); err != nil {
		return time.Time{}, err
	}
	return updated, tx.Commit()
}

// DeleteProvider removes one row and appends its audit entry in one
// transaction. It returns ErrNotFound when the row does not exist.
func (s *Store) DeleteProvider(ctx context.Context, id, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin delete provider: %w", err)
	}
	defer tx.Rollback()
	before, err := getProviderTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM llm_providers WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: delete provider: %w", err)
	}
	if err := insertAudit(ctx, tx, actor, "delete", id, providerJSON(before), nil); err != nil {
		return err
	}
	return tx.Commit()
}

func getProviderTx(ctx context.Context, tx *sql.Tx, id string) (ProviderRow, error) {
	p, err := scanProvider(tx.QueryRowContext(ctx, `SELECT `+providerColumns+` FROM llm_providers WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProviderRow{}, ErrNotFound
		}
		return ProviderRow{}, fmt.Errorf("store: get provider: %w", err)
	}
	return p, nil
}

func insertAudit(ctx context.Context, tx *sql.Tx, actor, action, providerID string, before, after json.RawMessage) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO llm_provider_audit (created_at, actor, action, provider_id, before, after) VALUES ($1, $2, $3, $4, $5, $6)`,
		now(), actor, action, providerID, nullJSON(before), nullJSON(after))
	if err != nil {
		return fmt.Errorf("store: audit %s %s: %w", action, providerID, err)
	}
	return nil
}

func nullJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// GetProvider fetches one provider row by id.
func (s *Store) GetProvider(ctx context.Context, id string) (ProviderRow, error) {
	p, err := scanProvider(s.db.QueryRowContext(ctx, `SELECT `+providerColumns+` FROM llm_providers WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProviderRow{}, ErrNotFound
		}
		return ProviderRow{}, fmt.Errorf("store: get provider: %w", err)
	}
	return p, nil
}

// ListProviders returns every row, oldest first.
func (s *Store) ListProviders(ctx context.Context) ([]ProviderRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+providerColumns+` FROM llm_providers ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list providers: %w", err)
	}
	defer rows.Close()
	var out []ProviderRow
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan provider: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountProviders reports how many rows exist; main.go consults it to decide
// whether BURROW_PROVIDERS_FILE is still a seed or already superseded.
func (s *Store) CountProviders(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM llm_providers`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count providers: %w", err)
	}
	return n, nil
}

// BumpProviderUpdatedAt advances one row's updated_at without an audit
// entry: a derived model menu changed (ADR-0004), which is a gateway event,
// not an admin edit. The snapshot poll is the consumer of this timestamp.
// It returns the new updated_at as the database recorded it.
func (s *Store) BumpProviderUpdatedAt(ctx context.Context, id string) (time.Time, error) {
	var updated time.Time
	err := s.db.QueryRowContext(ctx,
		`UPDATE llm_providers SET updated_at = $2 WHERE id = $1 RETURNING updated_at`, id, now()).
		Scan(&updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, ErrNotFound
		}
		return time.Time{}, fmt.Errorf("store: bump provider %s: %w", id, err)
	}
	return updated, nil
}

// ListAudit returns audit entries newest first, capped at limit (SPEC-0004
// REQ "Audit"). limit <= 0 falls back to 50.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_at, actor, action, provider_id, before, after FROM llm_provider_audit ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var (
			e             AuditEntry
			before, after sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Actor, &e.Action, &e.ProviderID, &before, &after); err != nil {
			return nil, fmt.Errorf("store: scan audit: %w", err)
		}
		if before.Valid {
			e.Before = json.RawMessage(before.String)
		}
		if after.Valid {
			e.After = json.RawMessage(after.String)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// APIToken is one admin API token row. The raw value exists only in the
// MintAPIToken response; the store keeps its SHA-256 hash like every other
// burrow credential (SPEC-0004 REQ "Admin authorization").
type APIToken struct {
	TokenHash  string
	UserID     string // the human whose session minted it
	Name       string
	Scopes     []string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt *time.Time
}

// AdminTokenPrefix marks the raw value of an admin API token, so the
// middleware can tell "revoked machine token" (401 invalid_token) from "dead
// session" (the contract's session-terminating codes) by shape alone.
const AdminTokenPrefix = "bat_"

// MintAPIToken issues a named, scoped token valid for ttl and returns the
// raw value once, beside its stored record.
func (s *Store) MintAPIToken(ctx context.Context, userID, name string, scopes []string, ttl time.Duration) (string, APIToken, error) {
	if scopes == nil {
		scopes = []string{}
	}
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return "", APIToken{}, fmt.Errorf("store: encode scopes: %w", err)
	}
	raw := AdminTokenPrefix + randomToken()
	created := now()
	tok := APIToken{
		TokenHash: hashToken(raw),
		UserID:    userID,
		Name:      name,
		Scopes:    scopes,
		CreatedAt: created,
		ExpiresAt: created.Add(ttl),
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO api_tokens (token_hash, user_id, name, scopes, created_at, expires_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		tok.TokenHash, userID, name, string(scopesJSON), created, tok.ExpiresAt); err != nil {
		return "", APIToken{}, fmt.Errorf("store: mint api token: %w", err)
	}
	return raw, tok, nil
}

// ListAPITokens returns every token, oldest first. The hash is the row's
// identifier: it is the primary key, and knowing it grants nothing — only
// the raw token authenticates.
func (s *Store) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT token_hash, user_id, name, scopes, created_at, expires_at, last_used_at FROM api_tokens ORDER BY created_at, token_hash`)
	if err != nil {
		return nil, fmt.Errorf("store: list api tokens: %w", err)
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var (
			t        APIToken
			scopes   string
			lastUsed sql.NullTime
		)
		if err := rows.Scan(&t.TokenHash, &t.UserID, &t.Name, &scopes, &t.CreatedAt, &t.ExpiresAt, &lastUsed); err != nil {
			return nil, fmt.Errorf("store: scan api token: %w", err)
		}
		if err := json.Unmarshal([]byte(scopes), &t.Scopes); err != nil {
			return nil, fmt.Errorf("store: decode scopes: %w", err)
		}
		if lastUsed.Valid {
			t.LastUsedAt = &lastUsed.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAPIToken deletes a token by its hash, or by its raw value (whose
// hash is the same row), so console and CLI callers can point DELETE at
// whichever they hold. ErrNotFound when no such token exists.
func (s *Store) RevokeAPIToken(ctx context.Context, hashOrToken string) error {
	tag, err := s.db.ExecContext(ctx,
		`DELETE FROM api_tokens WHERE token_hash = $1 OR token_hash = $2`, hashOrToken, hashToken(hashOrToken))
	if err != nil {
		return fmt.Errorf("store: revoke api token: %w", err)
	}
	affected, err := tag.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: revoke api token rows: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// LookupAPIToken resolves a raw token to its record, distinguishing unknown
// from expired (both are plain invalid_token to the admin surface, never a
// session-terminating code), and stamps last_used_at on a successful use.
func (s *Store) LookupAPIToken(ctx context.Context, rawToken string) (APIToken, error) {
	var (
		t      APIToken
		scopes string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT token_hash, user_id, name, scopes, created_at, expires_at FROM api_tokens WHERE token_hash = $1`, hashToken(rawToken)).
		Scan(&t.TokenHash, &t.UserID, &t.Name, &scopes, &t.CreatedAt, &t.ExpiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return APIToken{}, ErrNotFound
		}
		return APIToken{}, fmt.Errorf("store: lookup api token: %w", err)
	}
	if err := json.Unmarshal([]byte(scopes), &t.Scopes); err != nil {
		return APIToken{}, fmt.Errorf("store: decode scopes: %w", err)
	}
	if now().After(t.ExpiresAt) {
		return APIToken{}, ErrExpired
	}
	used := now()
	if _, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = $2 WHERE token_hash = $1`, t.TokenHash, used); err != nil {
		return APIToken{}, fmt.Errorf("store: stamp api token use: %w", err)
	}
	t.LastUsedAt = &used
	return t, nil
}

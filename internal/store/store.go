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

	"github.com/jackc/pgx/v5/pgconn"
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
// Every start converges it — burrow is one service with one schema and carries
// no migration history — and a start that finds it already current changes
// nothing and waits on no table lock. ctx bounds the whole of it: the connect,
// and any wait for a lock a schema change needs.
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
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the pool.
func (s *Store) Close() error { return s.db.Close() }

// migrateLockKey is the advisory lock a boot holds while it converges the
// schema ("burrow" in ASCII). Advisory locks are scoped to the database, which
// burrow has to itself, so the key only has to be the same in every burrow.
const migrateLockKey int64 = 0x627572726f77

// migrateLockTimeout bounds each lock wait of a boot. It is short on purpose:
// a queued ACCESS EXCLUSIVE request makes every new query on the table wait
// behind it, so a boot that waits for its lock stalls the instance that is
// already serving for exactly as long. A var so the tests can shorten it.
var migrateLockTimeout = 5 * time.Second

// schemaChanges are the changes to objects a deployed database already has —
// what CREATE ... IF NOT EXISTS cannot express. Each runs only while its
// pending query still answers true.
//
// Governing: ADR-0002 (the store is Postgres, converged at boot).
//
// The check is not an optimisation. ALTER TABLE takes ACCESS EXCLUSIVE, and
// CREATE INDEX takes SHARE, before IF [NOT] EXISTS is ever evaluated, so an
// unguarded statement here locks its table on every boot: behind one idle
// reader the boot hangs, and every request queues behind the boot. A new
// column goes in its CREATE TABLE below (for a fresh database) and in a
// guarded entry here (for an existing one).
var schemaChanges = []struct {
	name    string
	pending string // answers true while the change has yet to be made
	ddl     string
}{
	{
		// SPEC-0003 REQ "Groups claim": databases older than the column.
		name:    "users.oidc_groups",
		pending: `SELECT NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('users') AND attname = 'oidc_groups' AND NOT attisdropped)`,
		ddl:     `ALTER TABLE users ADD COLUMN IF NOT EXISTS oidc_groups TEXT NOT NULL DEFAULT '[]'`,
	},
	{
		// users.email was UNIQUE in the first Postgres schema. Identity is the
		// OIDC subject and nothing looks a user up by email, so all the
		// constraint did was refuse sign-in to a re-created account (new
		// subject, same email) and to the second account with no email claim.
		name:    "users.email unique",
		pending: `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('users') AND conname = 'users_email_key')`,
		ddl:     `ALTER TABLE users DROP CONSTRAINT IF EXISTS users_email_key`,
	},
	{
		// SPEC-0001 REQ "Session lifetime": databases older than the column.
		name:    "grants.session_expires_at",
		pending: `SELECT NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('grants') AND attname = 'session_expires_at' AND NOT attisdropped)`,
		ddl:     `ALTER TABLE grants ADD COLUMN IF NOT EXISTS session_expires_at TIMESTAMPTZ`,
	},
	{
		name:    "llm_provider_audit_provider_idx",
		pending: `SELECT to_regclass('llm_provider_audit_provider_idx') IS NULL`,
		ddl:     `CREATE INDEX IF NOT EXISTS llm_provider_audit_provider_idx ON llm_provider_audit (provider_id)`,
	},
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS users (
	id         TEXT PRIMARY KEY,
	email      TEXT NOT NULL,
	name       TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	-- SPEC-0003 REQ "Groups claim": the member's groups as of their last
	-- sign-in, a JSON array. Named oidc_groups because GROUPS is a reserved word.
	oidc_groups TEXT NOT NULL DEFAULT '[]'
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
	expires_at TIMESTAMPTZ NOT NULL,
	-- SPEC-0001 REQ "Session lifetime": the expiry of the session that minted
	-- this grant, which the session it buys may not outlive. NULL for a grant
	-- minted by the OIDC callback, which has no parent session.
	session_expires_at TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS oidc_states (
	state      TEXT PRIMARY KEY,
	verifier   TEXT NOT NULL,
	desktop    BOOLEAN NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL
);
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
	// One transaction: the lock_timeout is local to it, so it bounds the boot's
	// lock waits and nothing the pool does afterwards. It is the server's own
	// deadline, and holds even where cancelling ctx cannot reach the server.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return migrateError("begin", err)
	}
	defer tx.Rollback()
	timeout := fmt.Sprintf("%dms", migrateLockTimeout.Milliseconds())
	if _, err := tx.ExecContext(ctx, `SELECT set_config('lock_timeout', $1, true)`, timeout); err != nil {
		return migrateError("set lock_timeout", err)
	}
	// Boots take turns, and the lock lets go on its own however this one ends.
	// Without it two boots on an empty database both see no tables and both
	// create them, and all but one die on the catalog's unique index — and a
	// catalog check below could be stale by the time its statement runs.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLockKey); err != nil {
		return migrateError("wait for another boot's migration", err)
	}
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return migrateError("create tables", err)
	}
	for _, change := range schemaChanges {
		var pending bool
		if err := tx.QueryRowContext(ctx, change.pending).Scan(&pending); err != nil {
			return migrateError("check "+change.name, err)
		}
		if !pending {
			continue
		}
		if _, err := tx.ExecContext(ctx, change.ddl); err != nil {
			return migrateError(change.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return migrateError("commit", err)
	}
	return nil
}

// migrateError names the step that failed, and says in words when the failure
// is a lock wait that ran out: "lock_not_available" alone does not tell an
// operator that some other session is sitting on one of burrow's tables.
func migrateError(step string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return fmt.Errorf("store: migrate: %s: gave up after %s waiting for a lock another session holds: %w", step, migrateLockTimeout, err)
	}
	return fmt.Errorf("store: migrate: %s: %w", step, err)
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
// It returns the raw grant value — the only time it exists in the clear. This
// is the grant of a fresh OIDC sign-in: it has no parent session, so the
// session it buys gets a full lifetime.
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

// MintHandoffGrant issues a single-use grant on behalf of a live session, for
// that session's user, valid for ttl. The grant records the session's expiry,
// and the session it buys cannot outlive it (SPEC-0001 REQ "Session
// lifetime") — otherwise a session could mint its own successor forever and
// the session TTL would bound nothing. The user and the expiry are read from
// the session row in the same statement as the insert, so a session signed
// out in between mints nothing: ErrNotFound.
func (s *Store) MintHandoffGrant(ctx context.Context, sessionToken string, ttl time.Duration) (string, error) {
	grant := randomToken()
	tag, err := s.db.ExecContext(ctx, `
INSERT INTO grants (token_hash, user_id, created_at, expires_at, session_expires_at)
SELECT $1, user_id, $2, $3, expires_at FROM sessions WHERE token_hash = $4`,
		hashToken(grant), now(), now().Add(ttl), hashToken(sessionToken))
	if err != nil {
		return "", fmt.Errorf("store: mint handoff grant: %w", err)
	}
	affected, err := tag.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("store: mint handoff grant rows: %w", err)
	}
	if affected == 0 {
		return "", ErrNotFound
	}
	return grant, nil
}

// ErrGrantInvalid marks a grant that is unknown, already used, or expired.
var ErrGrantInvalid = errors.New("store: grant is invalid, used, or expired")

// ConsumeGrant validates and burns a grant in one statement, returning the
// user it was minted for. The DELETE is the read: a second call with the same
// grant finds no row, so no interleaving of two requests can spend a grant
// twice. An expired row is deleted and answered the same way a reused one is.
//
// notAfter is the expiry of the session that minted the grant — the latest
// moment the session this grant buys may live to — and the zero time for a
// grant from a fresh OIDC sign-in. A grant whose minting session has already
// expired is refused like an expired grant: the sign-in it descends from is
// over.
func (s *Store) ConsumeGrant(ctx context.Context, grant string) (userID string, notAfter time.Time, err error) {
	var expiresAt time.Time
	var sessionExpiresAt sql.NullTime
	err = s.db.QueryRowContext(ctx,
		`DELETE FROM grants WHERE token_hash = $1 RETURNING user_id, expires_at, session_expires_at`, hashToken(grant)).
		Scan(&userID, &expiresAt, &sessionExpiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", time.Time{}, ErrGrantInvalid
		}
		return "", time.Time{}, fmt.Errorf("store: consume grant: %w", err)
	}
	if now().After(expiresAt) {
		return "", time.Time{}, ErrGrantInvalid
	}
	if sessionExpiresAt.Valid {
		if now().After(sessionExpiresAt.Time) {
			return "", time.Time{}, ErrGrantInvalid
		}
		notAfter = sessionExpiresAt.Time
	}
	return userID, notAfter, nil
}

// CreateSession issues a session token for user, valid for ttl, returning the
// raw token value.
func (s *Store) CreateSession(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	return s.CreateSessionCapped(ctx, userID, ttl, time.Time{})
}

// CreateSessionCapped is CreateSession for a session bought with a grant: it
// is valid for ttl but never past notAfter, the value ConsumeGrant returned.
// A zero notAfter is no cap. The expiry is stored as notAfter itself, not as
// a ttl recomputed from it, so a chain of handoffs cannot creep past the
// sign-in it started from.
func (s *Store) CreateSessionCapped(ctx context.Context, userID string, ttl time.Duration, notAfter time.Time) (string, error) {
	token := randomToken()
	expiresAt := now().Add(ttl)
	if !notAfter.IsZero() && notAfter.Before(expiresAt) {
		expiresAt = notAfter
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
		hashToken(token), userID, now(), expiresAt)
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

// Purged counts the expired rows one PurgeExpired call removed, per table.
type Purged struct {
	Sessions   int64
	Grants     int64
	OIDCStates int64
	MCPTokens  int64
}

// PurgeExpired deletes every session, grant, OIDC sign-in state and MCP token
// past its expires_at. It is housekeeping, not enforcement: each lookup still
// checks expiry itself, so a row this has not reached yet is refused exactly
// as before. Without it nothing ever removes an expired row — and
// /auth/oidc/start writes a state row for every anonymous request, so the
// tables only grow. Once swept, an expired token reads as unknown rather than
// expired, which every caller already answers with a refusal.
//
// Admin API tokens are left alone on purpose: they are a short list an admin
// mints and revokes by name, and an expired one is still worth seeing there.
func (s *Store) PurgeExpired(ctx context.Context) (Purged, error) {
	var purged Purged
	cutoff := now()
	for _, t := range []struct {
		table string
		count *int64
	}{
		{"sessions", &purged.Sessions},
		{"grants", &purged.Grants},
		{"oidc_states", &purged.OIDCStates},
		{"mcp_tokens", &purged.MCPTokens},
	} {
		tag, err := s.db.ExecContext(ctx, `DELETE FROM `+t.table+` WHERE expires_at < $1`, cutoff)
		if err != nil {
			return purged, fmt.Errorf("store: purge expired %s: %w", t.table, err)
		}
		if *t.count, err = tag.RowsAffected(); err != nil {
			return purged, fmt.Errorf("store: purge expired %s rows: %w", t.table, err)
		}
	}
	return purged, nil
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

// ErrAlreadySeeded marks a seed refused because the provider store has
// already been written: by an earlier seed, a concurrent one, or an admin.
var ErrAlreadySeeded = errors.New("store: providers already seeded")

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
	if err := createProviderTx(ctx, tx, p, actor); err != nil {
		return err
	}
	return tx.Commit()
}

// createProviderTx is one row's insert and its audit entry inside the
// caller's transaction, shared by CreateProvider and SeedProviders so a
// seeded row is stored and audited exactly like an admin-created one.
func createProviderTx(ctx context.Context, tx *sql.Tx, p ProviderRow, actor string) error {
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
	return insertAudit(ctx, tx, actor, "create", p.ID, nil, providerJSON(p))
}

// SeedProviders inserts the first-boot seed — every row and its audit entry
// — in one transaction, and only into a store nothing has written (ADR-0004,
// SPEC-0004 REQ "Rows in the store", scenario "First-boot seed"). A seed
// therefore lands whole or not at all: were each row its own transaction, a
// failure after the first insert (a dropped connection, a SIGTERM mid-boot, a
// value Postgres refuses) would leave rows behind, the next boot would read
// them as "already seeded", and burrow would serve a partial provider set
// for good. It returns ErrAlreadySeeded, inserting nothing, when the store
// has been written.
func (s *Store) SeedProviders(ctx context.Context, rows []ProviderRow, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin seed providers: %w", err)
	}
	defer tx.Rollback()
	// SHARE ROW EXCLUSIVE conflicts with itself and with the lock every
	// insert and delete takes, so the already-seeded check below and the
	// inserts are atomic against a second process booting on the same empty
	// database and against an admin write: the loser waits here, then sees a
	// seeded store instead of colliding on a row id.
	if _, err := tx.ExecContext(ctx, `LOCK TABLE llm_providers IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("store: lock providers for seed: %w", err)
	}
	seeded, err := providersSeeded(ctx, tx)
	if err != nil {
		return err
	}
	if seeded {
		return ErrAlreadySeeded
	}
	for _, p := range rows {
		if err := createProviderTx(ctx, tx, p, actor); err != nil {
			return fmt.Errorf("store: seed provider %s: %w", p.ID, err)
		}
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
// transaction. A non-zero expectUpdatedAt is the same precondition
// UpdateProvider takes, checked inside the same row lock: a row that changed
// since the caller read it is not deleted — the caller gets ErrStale, and a
// delete racing an update on one etag cannot remove the version nobody
// looked at. It returns ErrNotFound when the row does not exist.
func (s *Store) DeleteProvider(ctx context.Context, id string, expectUpdatedAt time.Time, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin delete provider: %w", err)
	}
	defer tx.Rollback()
	before, err := getProviderTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if !expectUpdatedAt.IsZero() && !before.UpdatedAt.Equal(expectUpdatedAt) {
		return ErrStale
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

// CountProviders reports how many rows exist. It does not say whether the
// store was ever seeded — an admin may have deleted every row — so the seed
// decision is ProvidersSeeded's.
func (s *Store) CountProviders(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM llm_providers`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count providers: %w", err)
	}
	return n, nil
}

// ProvidersSeeded reports whether the provider store has ever been written:
// a row exists, or the audit trail has an entry. main.go consults it to
// decide whether BURROW_PROVIDERS_FILE is still a seed or already superseded
// (ADR-0004). The audit trail is what makes the answer survive deletes: every
// create, update and delete appends to it in the change's own transaction,
// so a table an admin emptied on purpose still reads as seeded and the next
// restart does not hand the file's rows back. The row check covers a table
// whose audit trail was pruned.
func (s *Store) ProvidersSeeded(ctx context.Context) (bool, error) {
	return providersSeeded(ctx, s.db)
}

// rowQuerier is shared by *sql.DB and *sql.Tx.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func providersSeeded(ctx context.Context, q rowQuerier) (bool, error) {
	var seeded bool
	if err := q.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM llm_providers) OR EXISTS (SELECT 1 FROM llm_provider_audit)`).
		Scan(&seeded); err != nil {
		return false, fmt.Errorf("store: providers seeded: %w", err)
	}
	return seeded, nil
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

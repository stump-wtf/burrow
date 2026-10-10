// Package store is burrow's Postgres persistence layer.
//
// Governing: ADR-0002 (Postgres on the shared cluster, superseding ADR-0001's
// SQLite choice), ADR-0001 (what the control plane must remember). Everything
// the control plane must remember across restarts lives here: users, sessions,
// sign-in grants, OIDC flow states, and MCP tokens. Tokens and grants are
// stored only as SHA-256 hashes; the raw values exist once, in the response
// that carries them to the client.
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

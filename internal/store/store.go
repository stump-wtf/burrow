// Package store is burrow's SQLite persistence layer.
//
// Governing: ADR-0001 (SQLite, one file, no separate database to operate).
// Everything the control plane must remember across restarts lives here:
// users, sessions, sign-in grants, OIDC flow states, and MCP tokens. Tokens
// and grants are stored only as SHA-256 hashes; the raw values exist once, in
// the response that carries them to the client.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// User is an identity established through OIDC sign-in.
type User struct {
	ID    string // the OIDC subject; opaque to the client
	Email string
	Name  string
}

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the database at path and applies the
// schema. Stage A carries a single embedded schema version.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// modernc/sqlite serializes writes; one connection avoids SQLITE_BUSY
	// under the desktop app's polling.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS users (
	id         TEXT PRIMARY KEY,
	email      TEXT NOT NULL UNIQUE,
	name       TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL REFERENCES users(id),
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS grants (
	token_hash TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL REFERENCES users(id),
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS oidc_states (
	state      TEXT PRIMARY KEY,
	verifier   TEXT NOT NULL,
	desktop    INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS mcp_tokens (
	token_hash TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL REFERENCES users(id),
	org_id     TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
`
	_, err := s.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }

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
// email and name claims on every sign-in.
func (s *Store) UpsertUser(ctx context.Context, u User) error {
	now := nowUTC()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO users (id, email, name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET email = excluded.email, name = excluded.name, updated_at = excluded.updated_at`,
		u.ID, u.Email, u.Name, now, now)
	if err != nil {
		return fmt.Errorf("store: upsert user: %w", err)
	}
	return nil
}

// GetUser fetches one user by id.
func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT id, email, name FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Email, &u.Name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("store: get user: %w", err)
	}
	return u, nil
}

// ListUsers returns every user, oldest first (the /v1/org member directory).
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, email, name FROM users ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.Name); err != nil {
			return nil, fmt.Errorf("store: scan user: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// MintGrant issues a single-use sign-in grant for user, valid for ttl.
// It returns the raw grant value — the only time it exists in the clear.
func (s *Store) MintGrant(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	grant := randomToken()
	_, err := s.db.ExecContext(ctx, `INSERT INTO grants (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hashToken(grant), userID, nowUTC(), time.Now().UTC().Add(ttl).Format(time.RFC3339Nano))
	if err != nil {
		return "", fmt.Errorf("store: mint grant: %w", err)
	}
	return grant, nil
}

// ErrGrantInvalid marks a grant that is unknown, already used, or expired.
var ErrGrantInvalid = errors.New("store: grant is invalid, used, or expired")

// ConsumeGrant atomically validates and burns a grant, returning the user it
// was minted for. A second call with the same grant fails: the row is deleted
// in the same transaction that reads it.
func (s *Store) ConsumeGrant(ctx context.Context, grant string) (string, error) {
	var userID, expiresAt string
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT user_id, expires_at FROM grants WHERE token_hash = ?`, hashToken(grant))
		if err := row.Scan(&userID, &expiresAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrGrantInvalid
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM grants WHERE token_hash = ?`, hashToken(grant)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	expiry, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || time.Now().UTC().After(expiry) {
		return "", ErrGrantInvalid
	}
	return userID, nil
}

// CreateSession issues a session token for user, valid for ttl, returning the
// raw token value.
func (s *Store) CreateSession(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	token := randomToken()
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hashToken(token), userID, nowUTC(), time.Now().UTC().Add(ttl).Format(time.RFC3339Nano))
	if err != nil {
		return "", fmt.Errorf("store: create session: %w", err)
	}
	return token, nil
}

// LookupSession resolves a session token to its user, distinguishing an
// unknown token from an expired one so the API can answer with the exact
// error code the desktop client expects.
func (s *Store) LookupSession(ctx context.Context, token string) (userID string, err error) {
	var expiresAt string
	err = s.db.QueryRowContext(ctx, `SELECT user_id, expires_at FROM sessions WHERE token_hash = ?`, hashToken(token)).
		Scan(&userID, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("store: lookup session: %w", err)
	}
	expiry, perr := time.Parse(time.RFC3339Nano, expiresAt)
	if perr != nil || time.Now().UTC().After(expiry) {
		return "", ErrExpired
	}
	return userID, nil
}

// ErrExpired marks a token that exists but is no longer valid.
var ErrExpired = errors.New("store: token expired")

// DeleteSession removes a session (sign-out).
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	if err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// SaveOIDCState records the PKCE verifier for one in-flight sign-in.
func (s *Store) SaveOIDCState(ctx context.Context, state, verifier string, desktop bool, ttl time.Duration) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO oidc_states (state, verifier, desktop, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		state, verifier, boolInt(desktop), nowUTC(), time.Now().UTC().Add(ttl).Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store: save oidc state: %w", err)
	}
	return nil
}

// TakeOIDCState atomically reads and removes one sign-in state.
func (s *Store) TakeOIDCState(ctx context.Context, state string) (verifier string, desktop bool, err error) {
	var desktopInt int
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT verifier, desktop, expires_at FROM oidc_states WHERE state = ?`, state)
		var expiresAt string
		if err := row.Scan(&verifier, &desktopInt, &expiresAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM oidc_states WHERE state = ?`, state); err != nil {
			return err
		}
		expiry, perr := time.Parse(time.RFC3339Nano, expiresAt)
		if perr != nil || time.Now().UTC().After(expiry) {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return verifier, desktopInt == 1, nil
}

// MintMCPToken issues an org-scoped MCP token valid for ttl.
func (s *Store) MintMCPToken(ctx context.Context, userID, orgID string, ttl time.Duration) (string, time.Time, error) {
	token := randomToken()
	expiresAt := time.Now().UTC().Add(ttl)
	_, err := s.db.ExecContext(ctx, `INSERT INTO mcp_tokens (token_hash, user_id, org_id, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		hashToken(token), userID, orgID, nowUTC(), expiresAt.Format(time.RFC3339Nano))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("store: mint mcp token: %w", err)
	}
	return token, expiresAt, nil
}

// LookupMCPToken resolves an MCP token to its user and org.
func (s *Store) LookupMCPToken(ctx context.Context, token string) (userID, orgID string, err error) {
	var expiresAt string
	err = s.db.QueryRowContext(ctx, `SELECT user_id, org_id, expires_at FROM mcp_tokens WHERE token_hash = ?`, hashToken(token)).
		Scan(&userID, &orgID, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrNotFound
		}
		return "", "", fmt.Errorf("store: lookup mcp token: %w", err)
	}
	expiry, perr := time.Parse(time.RFC3339Nano, expiresAt)
	if perr != nil || time.Now().UTC().After(expiry) {
		return "", "", ErrExpired
	}
	return userID, orgID, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func withTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

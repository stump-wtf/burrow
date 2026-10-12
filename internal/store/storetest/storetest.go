// Package storetest gives each test its own Postgres schema.
//
// Governing: ADR-0002 (the store is Postgres, and a test store is a real
// store, never a fake). Point BURROW_TEST_DATABASE_URL at a server and each
// test gets a private schema that is dropped when it finishes; `make test`
// starts a disposable server and sets the variable.
package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

var seq atomic.Int64

// Open returns a store whose tables live in a schema only this test can see,
// dropped when the test finishes. It fails rather than skips: a suite that
// quietly ran against nothing would still report green.
func Open(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(context.Background(), DSN(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// DSN creates an empty schema only this test can see, dropped when the test
// finishes, and returns a postgres:// URL whose connections start in it. It
// does not open a store: the tests of the boot path need the schema before
// burrow has touched it, to open it more than once, or to hold a lock in it
// from a connection of their own.
func DSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv("BURROW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatalf("BURROW_TEST_DATABASE_URL is unset, and burrow's tests only run against a real Postgres.\n" +
			"Run `make test` (it starts a disposable server), or export the variable with a postgres:// URL\n" +
			"for a server whose user may CREATE SCHEMA.")
	}

	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	admin.SetMaxOpenConns(1)
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		t.Fatalf("connect to the test server: %v", err)
	}

	schema := fmt.Sprintf("burrow_test_%d_%d", time.Now().UnixNano(), seq.Add(1))
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(dropCtx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema)); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
		admin.Close()
	})

	return withSearchPath(t, dsn, schema)
}

// withSearchPath repoints a postgres:// URL at one schema. Every connection
// the pool opens then starts in that schema, which is what keeps two tests
// running against one server from seeing each other's rows — and keeps a test
// from ever touching the database's public schema.
func withSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("BURROW_TEST_DATABASE_URL must be a postgres:// URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

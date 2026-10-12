package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
	"gitea.stump.rocks/stump.wtf/burrow/internal/store/storetest"
)

// legacyUsers is the users table as burrow first created it on Postgres:
// email UNIQUE, and no oidc_groups column yet. A database made by that build
// still has both, which is what the boot path has to converge.
const legacyUsers = `
CREATE TABLE users (
	id         TEXT PRIMARY KEY,
	email      TEXT NOT NULL UNIQUE,
	name       TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
)`

// legacyGroups is what the next build did to that table on every boot.
// Together they are the users table of every database made before the
// constraint was dropped.
const legacyGroups = `ALTER TABLE users ADD COLUMN IF NOT EXISTS oidc_groups TEXT NOT NULL DEFAULT '[]'`

// rawDB is a plain pool on a test schema, for the things a test does behind
// the store's back: build an old schema, hold a lock, read the catalog.
func rawDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw connection: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Identity is the OIDC subject, never the email: an account re-created at the
// identity provider arrives with a new subject and the old email, and two
// accounts with no email claim both carry the empty string. Each must be able
// to sign in.
func TestSecondSubjectWithSameEmailSignsIn(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	for _, u := range []store.User{
		{ID: "old-sub", Email: "joe@example.com", Name: "Joe"},
		{ID: "new-sub", Email: "joe@example.com", Name: "Joe"},
		{ID: "no-email-1"},
		{ID: "no-email-2"},
	} {
		if err := st.UpsertUser(ctx, u); err != nil {
			t.Fatalf("upsert %s (email %q): %v", u.ID, u.Email, err)
		}
	}
	for id, email := range map[string]string{"old-sub": "joe@example.com", "new-sub": "joe@example.com", "no-email-1": "", "no-email-2": ""} {
		got, err := st.GetUser(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Email != email {
			t.Fatalf("%s: email = %q, want %q", id, got.Email, email)
		}
	}
}

// A database created before the constraint was dropped carries users.email
// UNIQUE. Booting the current build on it must drop the constraint, keep the
// rows, and be a no-op the next time.
func TestLegacySchemaIsMigrated(t *testing.T) {
	for name, legacy := range map[string][]string{
		"first Postgres schema": {legacyUsers},
		"with oidc_groups":      {legacyUsers, legacyGroups},
	} {
		t.Run(name, func(t *testing.T) {
			dsn := storetest.DSN(t)
			db := rawDB(t, dsn)
			ctx := context.Background()

			for _, ddl := range legacy {
				if _, err := db.Exec(ddl); err != nil {
					t.Fatalf("create the legacy users table: %v", err)
				}
			}
			if _, err := db.Exec(`INSERT INTO users (id, email, name, created_at, updated_at) VALUES ('old-sub', 'joe@example.com', 'Joe', now(), now())`); err != nil {
				t.Fatalf("seed the legacy row: %v", err)
			}

			for boot := 1; boot <= 2; boot++ {
				st, err := store.Open(ctx, dsn)
				if err != nil {
					t.Fatalf("boot %d on the legacy schema: %v", boot, err)
				}
				t.Cleanup(func() { st.Close() })

				old, err := st.GetUser(ctx, "old-sub")
				if err != nil {
					t.Fatalf("boot %d lost the legacy row: %v", boot, err)
				}
				if old.Email != "joe@example.com" || len(old.Groups) != 0 {
					t.Fatalf("boot %d: legacy row = %+v", boot, old)
				}
				if err := st.UpsertUser(ctx, store.User{ID: "new-sub", Email: "joe@example.com", Groups: []string{"eng"}}); err != nil {
					t.Fatalf("boot %d: second subject with the same email: %v", boot, err)
				}

				var unique int
				if err := db.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conrelid = to_regclass('users') AND contype = 'u'`).Scan(&unique); err != nil {
					t.Fatalf("read constraints: %v", err)
				}
				if unique != 0 {
					t.Fatalf("boot %d left %d unique constraint(s) on users", boot, unique)
				}
			}
		})
	}
}

// hangBound is how long a boot that should not wait on anything may take
// before the test calls it hung. A passing boot returns in milliseconds; the
// bound only has to be shorter than forever, and longer than the boot's own
// lock timeout so that a boot which does wait fails with its own error.
const hangBound = 10 * time.Second

// readBound is how long a plain read may take while a lock holder is still
// around: long enough for a loaded server, far short of a queued lock.
const readBound = 2 * time.Second

// holdLock takes a table lock in a transaction it leaves open, the way an idle
// psql session, a dump or a slow writer does, and returns the func that lets
// go. The lock is also released when the test ends, before the schema is
// dropped.
func holdLock(t *testing.T, db *sql.DB, mode string, tables ...string) (release func()) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { tx.Rollback() }) }
	t.Cleanup(release)
	for _, table := range tables {
		if _, err := tx.Exec(fmt.Sprintf(`LOCK TABLE %q IN %s MODE`, table, mode)); err != nil {
			t.Fatalf("lock %s in %s mode: %v", table, mode, err)
		}
	}
	return release
}

// openBounded runs store.Open and reports whether it returned within bound.
// When it did not, unblock is called so the stuck boot can finish and be
// closed instead of outliving the test.
func openBounded(t *testing.T, ctx context.Context, dsn string, bound time.Duration, unblock func()) (st *store.Store, err error, returned bool) {
	t.Helper()
	type result struct {
		st  *store.Store
		err error
	}
	done := make(chan result, 1)
	go func() {
		st, err := store.Open(ctx, dsn)
		done <- result{st, err}
	}()
	closeLater := func(r result) {
		if r.st != nil {
			t.Cleanup(func() { r.st.Close() })
		}
	}
	select {
	case r := <-done:
		closeLater(r)
		return r.st, r.err, true
	case <-time.After(bound):
		unblock()
		closeLater(<-done)
		return nil, nil, false
	}
}

func tablesIn(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT tablename FROM pg_tables WHERE schemaname = current_schema() ORDER BY tablename`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	return tables
}

// A boot on a schema that is already current must not queue behind anyone:
// not a reader or a dump (ACCESS SHARE), and not a writer in the middle of a
// transaction (ROW EXCLUSIVE). Queueing is worse than slow — a waiting
// exclusive lock makes every new query on the table wait behind it, so the
// instance that is already serving stops answering too.
func TestBootOnCurrentSchemaWaitsForNoLock(t *testing.T) {
	for _, mode := range []string{"ACCESS SHARE", "ROW EXCLUSIVE"} {
		t.Run(mode, func(t *testing.T) {
			dsn := storetest.DSN(t)
			running, err := store.Open(context.Background(), dsn)
			if err != nil {
				t.Fatalf("first boot: %v", err)
			}
			t.Cleanup(func() { running.Close() })
			seedUser(t, running)

			db := rawDB(t, dsn)
			release := holdLock(t, db, mode, tablesIn(t, db)...)

			_, err, returned := openBounded(t, context.Background(), dsn, hangBound, release)
			if !returned {
				t.Fatalf("second boot was still blocked after %s behind a %s lock", hangBound, mode)
			}
			if err != nil {
				t.Fatalf("second boot: %v", err)
			}

			readCtx, cancel := context.WithTimeout(context.Background(), readBound)
			defer cancel()
			if _, err := running.GetUser(readCtx, "sub-123"); err != nil {
				t.Fatalf("the running store stopped answering reads: %v", err)
			}
		})
	}
}

// When a boot does need a lock it cannot have — the schema is behind and
// another session is sitting on the table — it must give up with an error, not
// hang with the lock request queued in front of every reader. Two things bound
// it: the server-side lock_timeout, and the caller's deadline.
func TestMigrateBlockedByALockGivesUp(t *testing.T) {
	blockedBoot := func(t *testing.T) (dsn string, db *sql.DB, release func()) {
		dsn = storetest.DSN(t)
		db = rawDB(t, dsn)
		if _, err := db.Exec(legacyUsers); err != nil {
			t.Fatalf("create the legacy users table: %v", err)
		}
		return dsn, db, holdLock(t, db, "ACCESS SHARE", "users")
	}
	// The point of giving up: the abandoned boot's lock request is gone from
	// the queue, so readers get through again while the holder is still there.
	readersGetThrough := func(t *testing.T, db *sql.DB) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), readBound)
		defer cancel()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
			t.Fatalf("a read after the boot gave up still waits behind it: %v", err)
		}
	}

	t.Run("lock_timeout", func(t *testing.T) {
		dsn, db, release := blockedBoot(t)
		store.SetMigrateLockTimeout(t, 200*time.Millisecond)

		_, err, returned := openBounded(t, context.Background(), dsn, hangBound, release)
		if !returned {
			t.Fatalf("Open was still blocked after %s with a 200ms lock_timeout", hangBound)
		}
		if err == nil {
			t.Fatalf("Open succeeded although the table it must alter was locked")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("want lock_not_available (55P03), got %v", err)
		}
		if !strings.Contains(err.Error(), "waiting for a lock another session holds") {
			t.Fatalf("the error must say what the boot was waiting for, got %v", err)
		}
		readersGetThrough(t, db)

		// Giving up is not giving in: once the other session lets go, the
		// next boot converges the schema.
		release()
		st, err := store.Open(context.Background(), dsn)
		if err != nil {
			t.Fatalf("boot after the lock was released: %v", err)
		}
		st.Close()
	})

	t.Run("caller's deadline", func(t *testing.T) {
		dsn, db, release := blockedBoot(t)

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, err, returned := openBounded(t, ctx, dsn, hangBound, release)
		if !returned {
			t.Fatalf("Open ignored its 300ms deadline: still blocked after %s", hangBound)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want the caller's deadline, got %v", err)
		}
		readersGetThrough(t, db)
	})
}

// Two replicas, or a restart racing the instance it replaces, boot at the same
// moment. On an empty database each used to see "no such table" and each tried
// to create it; all but one lost on the catalog's own unique index.
func TestConcurrentFirstBootsAllSucceed(t *testing.T) {
	dsn := storetest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*hangBound)
	defer cancel()

	const boots = 8
	errs := make([]error, boots)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := store.Open(ctx, dsn)
			if err != nil {
				errs[i] = err
				return
			}
			st.Close()
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("boot %d of %d: %v", i+1, boots, err)
		}
	}
}

// A database created before grants carried session_expires_at gets the column
// from the guarded change, keeps a grant that was in flight across the
// upgrade, and can mint a handoff grant afterwards.
func TestLegacyGrantsTableGainsItsSessionExpiry(t *testing.T) {
	dsn := storetest.DSN(t)
	db := rawDB(t, dsn)
	ctx := context.Background()

	first, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if err := first.UpsertUser(ctx, store.User{ID: "u1", Email: "u1@example.com"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	inFlight, err := first.MintGrant(ctx, "u1", time.Minute)
	if err != nil {
		t.Fatalf("mint the in-flight grant: %v", err)
	}
	first.Close()
	// Back to the table as the build before the column left it.
	if _, err := db.Exec(`ALTER TABLE grants DROP COLUMN session_expires_at`); err != nil {
		t.Fatalf("drop the column: %v", err)
	}

	for boot := 1; boot <= 2; boot++ {
		st, err := store.Open(ctx, dsn)
		if err != nil {
			t.Fatalf("boot %d on the legacy grants table: %v", boot, err)
		}
		t.Cleanup(func() { st.Close() })
		session, err := st.CreateSession(ctx, "u1", time.Hour)
		if err != nil {
			t.Fatalf("boot %d: create session: %v", boot, err)
		}
		if _, err := st.MintHandoffGrant(ctx, session, time.Minute); err != nil {
			t.Fatalf("boot %d: a handoff grant needs the column: %v", boot, err)
		}
		if boot == 1 {
			if _, notAfter, err := st.ConsumeGrant(ctx, inFlight); err != nil || !notAfter.IsZero() {
				t.Fatalf("the in-flight grant must still exchange, uncapped: notAfter=%v err=%v", notAfter, err)
			}
		}
	}
}

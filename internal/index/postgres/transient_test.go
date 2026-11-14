package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestTransientStates makes the server end a session on each of its timeouts, and abort a
// transaction on a deadlock and on a conflict it cannot serialize, through the store: each
// error wraps index.ErrUnavailable, 503 with Retry-After for the client, and carries the
// SQLSTATE that the server sent (#627). Each timeout is a setting of a role of the test's own,
// so that it touches no other session of the database.
func TestTransientStates(t *testing.T) {
	dsn := testDSN(t)
	if onYugabyte(t, dsn) {
		t.Skip("YugabyteDB: the timeouts, deadlocks and conflicts of Postgres are tested; YugabyteDB's mapping is TestYugabyteDialect's")
	}
	if onCockroach(t, openDB(t, dsn)) {
		// Measured on v26.3.5: idle_in_transaction_session_timeout closes the connection (an
		// EOF, ErrUnavailable), transaction_timeout is 57014, a deadlock 40001, and the store's
		// read committed overrides the role's repeatable read (docs/deploy/databases.md).
		t.Skip("CockroachDB: its timeouts and aborts have codes of their own; its mapping is TestCockroachDialect's")
	}

	t.Run("25P03 idle_in_transaction_session_timeout", func(t *testing.T) {
		s := openAs(t, settingRole(t, dsn, "idle_in_transaction_session_timeout", "500"))
		wantTransient(t, s, "25P03", idleTx(t.Context(), s, 2*time.Second))
	})

	t.Run("25P04 transaction_timeout", func(t *testing.T) {
		s := openAs(t, settingRole(t, dsn, "transaction_timeout", "500"))
		wantTransient(t, s, "25P04", idleTx(t.Context(), s, 2*time.Second))
	})

	t.Run("57P05 idle_session_timeout", func(t *testing.T) {
		if os.Getenv(isolationEnv) == "database" {
			t.Skip("behind a pooler the server's idle_session_timeout ends the pooler's connections, not the store's")
		}
		ctx := t.Context()
		// Shorter than the second after which database/sql's driver pings a pooled connection
		// before it hands it out (stdlib.Conn.ResetSession), else the ping finds the connection
		// ended and the pool opens another one.
		s := openAs(t, settingRole(t, dsn, "idle_session_timeout", "100"))
		if _, err := s.DatabaseTime(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		_, err := s.DatabaseTime(ctx)
		wantTransient(t, s, "57P05", err)
	})

	t.Run("40P01 deadlock_detected", func(t *testing.T) {
		ctx := t.Context()
		s := openMigrated(t, isolatedDSN(t, dsn))
		r := index.Repo{Format: "raw", Name: "scratch", Registry: "local", Kind: index.KindHosted}
		if err := s.CreateRepo(ctx, &r); err != nil {
			t.Fatal(err)
		}
		claim := func(tag string) *index.TagClaim {
			return &index.TagClaim{RepoID: r.ID, Package: "pkg", Tag: tag, Digest: digest.FromString(tag), Principal: "test"}
		}
		// Each transaction claims one tag, then, once both did, the other's, which waits for it.
		var claimed sync.WaitGroup
		claimed.Add(2)
		errs := make(chan error, 2)
		for _, tags := range [][2]string{{"a", "b"}, {"b", "a"}} {
			go func() {
				errs <- s.Tx(ctx, func(w index.Writer) error {
					err := w.ClaimTag(ctx, claim(tags[0]), time.Time{})
					claimed.Done()
					if err != nil {
						return err
					}
					claimed.Wait()
					return w.ClaimTag(ctx, claim(tags[1]), time.Time{})
				})
			}()
		}
		first, second := <-errs, <-errs
		if (first == nil) == (second == nil) {
			t.Fatalf("the transactions returned %v and %v; want one aborted and one committed", first, second)
		}
		wantTransient(t, s, "40P01", errors.Join(first, second))
	})

	t.Run("40001 serialization_failure", func(t *testing.T) {
		ctx := t.Context()
		role, password := newRole(t, dsn)
		ns := newNamespace(t, dsn)
		owner := openMigrated(t, ns.dsn(dsn))
		admin := openDB(t, ns.dsn(dsn))
		for _, stmt := range []string{fmt.Sprintf(dataGrants, ns.schema, role),
			"ALTER ROLE " + role + " SET default_transaction_isolation = 'repeatable read'"} {
			if _, err := admin.ExecContext(ctx, stmt); err != nil {
				t.Fatal(err)
			}
		}
		s := openAs(t, ns.dsn(withUser(t, dsn, role, password)))
		r := index.Repo{Format: "raw", Name: "scratch", Registry: "local", Kind: index.KindHosted}
		if err := owner.CreateRepo(ctx, &r); err != nil {
			t.Fatal(err)
		}
		// The transaction reads the repo, the owner updates it meanwhile, and the transaction's
		// own update cannot be serialized after the owner's.
		err := s.Tx(ctx, func(w index.Writer) error {
			mine, err := w.GetRepo(ctx, "raw", "scratch")
			if err != nil {
				return fmt.Errorf("before: %w", err)
			}
			if err := owner.UpdateRepo(ctx, &r); err != nil {
				return fmt.Errorf("the owner's update: %w", err)
			}
			return w.UpdateRepo(ctx, mine)
		})
		wantTransient(t, s, "40001", err)
	})
}

// wantTransient fails the test unless err wraps index.ErrUnavailable and a server error of
// state, and the store s serves again.
func wantTransient(t *testing.T, s *Store, state string, err error) {
	t.Helper()
	if !errors.Is(err, index.ErrUnavailable) || sqlState(err) != state {
		t.Fatalf("got %v (SQLSTATE %q), want index.ErrUnavailable of SQLSTATE %s", err, sqlState(err), state)
	}
	if _, err := s.DatabaseTime(t.Context()); err != nil {
		t.Errorf("after the error: %v", err)
	}
}

// idleTx returns the error of a transaction that idles for d between two statements.
func idleTx(ctx context.Context, s *Store, d time.Duration) error {
	return s.Tx(ctx, func(w index.Writer) error {
		if _, err := w.DatabaseTime(ctx); err != nil {
			return fmt.Errorf("before: %w", err)
		}
		time.Sleep(d)
		_, err := w.DatabaseTime(ctx)
		return err
	})
}

// settingRole creates a role (newRole) whose sessions run with the setting at value, and returns
// the DSN that logs in as it. A setting the server does not know skips the test.
func settingRole(t *testing.T, dsn, setting, value string) string {
	t.Helper()
	role, password := newRole(t, dsn)
	if _, err := openDB(t, dsn).ExecContext(t.Context(), "ALTER ROLE "+role+" SET "+setting+" = "+value); err != nil {
		if sqlState(err) == "42704" { // undefined_object: transaction_timeout before Postgres 17
			t.Skipf("the server has no %s: %v", setting, err)
		}
		t.Fatal(err)
	}
	return withUser(t, dsn, role, password)
}

// openAs opens the store on dsn, without a migration, and closes it when the test ends.
func openAs(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// onYugabyte reports whether the database of dsn is YugabyteDB, as its version() says.
func onYugabyte(t *testing.T, dsn string) bool {
	t.Helper()
	return strings.Contains(serverVersion(t, openDB(t, dsn)), "-YB-")
}

// onCockroach reports whether db is CockroachDB, as its version() says.
func onCockroach(t *testing.T, db *sql.DB) bool {
	t.Helper()
	return strings.Contains(serverVersion(t, db), "CockroachDB")
}

// serverVersion returns the version() of db.
func serverVersion(t *testing.T, db *sql.DB) string {
	t.Helper()
	var version string
	if err := db.QueryRowContext(t.Context(), "SELECT version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

// terminate ends, from admin's session, the sessions of a role (user) or of an application
// name (app) that are inside a transaction, as a server that shuts down does, and returns how
// many it ended: pg_terminate_backend on Postgres, CANCEL SESSION on CockroachDB, which has no
// pg_terminate_backend.
func terminate(ctx context.Context, t *testing.T, admin *sql.DB, user, app string) (int, error) {
	t.Helper()
	if !onCockroach(t, admin) {
		var n int
		err := admin.QueryRowContext(ctx, "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity"+
			" WHERE ($1 = '' OR usename = $1) AND ($2 = '' OR application_name = $2) AND state = 'idle in transaction'",
			user, app).Scan(&n)
		return n, err
	}
	rows, err := admin.QueryContext(ctx, "SELECT session_id FROM crdb_internal.cluster_sessions"+
		" WHERE ($1 = '' OR user_name = $1) AND ($2 = '' OR application_name = $2) AND kv_txn IS NOT NULL",
		user, app)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := admin.ExecContext(ctx, "CANCEL SESSION $1", id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

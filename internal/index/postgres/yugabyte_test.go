package postgres

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/internal/sqlstore"
)

// TestYugabyteDialect checks what the YugabyteDB dialect changes: only it has the package lock,
// and the transactions YugabyteDB aborts are ErrUnavailable, as on Postgres (#627).
func TestYugabyteDialect(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"deadlock", fmt.Errorf("index: packages: %w", &pgconn.PgError{Code: "40P01"}), true},
		{"serialization failure", &pgconn.PgError{Code: "40001"}, true},
		{"admin shutdown", &pgconn.PgError{Code: "57P01"}, true},
		{"unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"other", errors.New("boom"), false},
	} {
		if got := yugabyte.IsUnavailable(tt.err); got != tt.want {
			t.Errorf("%s: IsUnavailable on YugabyteDB = %v, want %v", tt.name, got, tt.want)
		}
	}
	if dialect.PackageLock != "" || yugabyte.PackageLock == "" {
		t.Errorf("PackageLock = %q on Postgres, %q on YugabyteDB; want only YugabyteDB's", dialect.PackageLock, yugabyte.PackageLock)
	}
}

// TestCockroachDialect checks what the CockroachDB dialect changes: its own migrations, read
// committed asked for by every transaction, and a method's aborted transaction run again; its
// locks are Postgres's (TestCockroachErrors checks its errors).
func TestCockroachDialect(t *testing.T) {
	if cockroach.Name != index.DialectCockroach || dialect.Name != index.DialectPostgres || yugabyte.Name != index.DialectPostgres {
		t.Errorf("migrations of %s on Postgres, %s on YugabyteDB, %s on CockroachDB", dialect.Name, yugabyte.Name, cockroach.Name)
	}
	if cockroach.Attempts != sqlstore.TxAttempts || dialect.Attempts > 1 || yugabyte.Attempts > 1 {
		t.Errorf("a method's aborted transaction runs %d times on CockroachDB, %d on Postgres, %d on YugabyteDB; want %d, once, once",
			cockroach.Attempts, dialect.Attempts, yugabyte.Attempts, sqlstore.TxAttempts)
	}
	if cockroach.Isolation != sql.LevelReadCommitted || dialect.Isolation != sql.LevelDefault {
		t.Errorf("isolation %v on CockroachDB, %v on Postgres; want read committed, the default", cockroach.Isolation, dialect.Isolation)
	}
	if cockroach.PackageLock != dialect.PackageLock || cockroach.XactLock != dialect.XactLock {
		t.Errorf("CockroachDB's locks differ from Postgres's: %q, %q", cockroach.PackageLock, cockroach.XactLock)
	}
}

// TestCockroachErrors checks the errors of the CockroachDB dialect: an ambiguous result (40003)
// is ErrUnavailable but never run again, and is not ErrUnavailable on Postgres's dialect; the
// aborts and the states of an unavailable server are ErrUnavailable, as on Postgres.
func TestCockroachErrors(t *testing.T) {
	ambiguous := fmt.Errorf("index: commit: %w", &pgconn.PgError{Code: ambiguousResult})
	if !cockroach.IsUnavailable(ambiguous) || cockroach.IsAborted(ambiguous) || dialect.IsUnavailable(ambiguous) {
		t.Errorf("40003: unavailable %v, aborted %v on CockroachDB, unavailable %v on Postgres; want true, false, false",
			cockroach.IsUnavailable(ambiguous), cockroach.IsAborted(ambiguous), dialect.IsUnavailable(ambiguous))
	}
	for _, code := range []string{"40001", "40P01", "57P01", "08006"} {
		if err := (&pgconn.PgError{Code: code}); !cockroach.IsUnavailable(err) {
			t.Errorf("%s is not ErrUnavailable on CockroachDB", code)
		}
	}
}

// TestDialectOf checks that Open's dialect is YugabyteDB's on YugabyteDB only and CockroachDB's on
// CockroachDB only, as version() says.
func TestDialectOf(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("pgx", testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var version string
	if err := db.QueryRowContext(ctx, "SELECT version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	d, err := dialectOf(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if yb := strings.Contains(version, "-YB-"); (d.PackageLock != "") != yb {
		t.Errorf("version() %q: the dialect has PackageLock %q", version, d.PackageLock)
	}
	if crdb := strings.Contains(version, "CockroachDB"); (d.Name == index.DialectCockroach) != crdb {
		t.Errorf("version() %q: the dialect's migrations are %s", version, d.Name)
	}
}

// TestPackageLock checks the lock UpsertPackage takes on a package path before it inserts a
// package (#629): on YugabyteDB's dialect it holds the lock until the transaction ends, and
// takes none for a package that exists; on Postgres's it takes none.
func TestPackageLock(t *testing.T) {
	ctx := t.Context()
	dsn := isolatedDSN(t, testDSN(t))
	openMigrated(t, dsn)
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	for _, tt := range []struct {
		name string
		d    sqlstore.Dialect
		want bool
	}{
		{"postgres", dialect, false},
		{"yugabyte", yugabyte, true},
	} {
		s := &Store{sqlstore.New(stdlib.OpenDB(*cfg), tt.d)}
		t.Cleanup(func() { _ = s.Close() })
		repo := index.Repo{Format: "raw", Name: tt.name, Registry: "local", Kind: "hosted"}
		if err := s.CreateRepo(ctx, &repo); err != nil {
			t.Fatal(err)
		}
		held := func() bool { // by another transaction than other's
			var free bool
			if err := other.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock($1)",
				sqlstore.PackagePathLockKey(repo.ID, "tool")).Scan(&free); err != nil {
				t.Fatal(err)
			}
			return !free
		}
		for _, step := range []struct {
			name string
			want bool
		}{
			{"the insert", tt.want},
			{"an upsert of the package", false},
		} {
			if err := s.Tx(ctx, func(w index.Writer) error {
				if err := w.UpsertPackage(ctx, &index.Package{RepoID: repo.ID, Name: "Tool", NameOCI: "tool"}); err != nil {
					return err
				}
				if got := held(); got != step.want {
					t.Errorf("%s, %s: the lock is held: %v, want %v", tt.name, step.name, got, step.want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if held() {
				t.Errorf("%s, %s: the lock is held after the commit", tt.name, step.name)
			}
		}
	}
}

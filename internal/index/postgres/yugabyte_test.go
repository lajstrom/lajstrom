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

// TestDialectOf checks that Open's dialect is YugabyteDB's on YugabyteDB only, as its version()
// says.
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

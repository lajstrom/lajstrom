package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/storetest"
)

// dsnEnv names the variable with the DSN of a Postgres database the tests may create schemas
// in, e.g. postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable.
const dsnEnv = "LAJSTROM_TEST_POSTGRES_DSN"

// testDSN returns the DSN of the test database, or skips the test.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("set %s to run the store tests on Postgres", dsnEnv)
	}
	return dsn
}

// schemaDSN creates an empty schema, drops it when the test ends, and returns dsn with the
// schema as its search_path: every test runs in a namespace of its own.
func schemaDSN(t *testing.T, dsn string) string {
	t.Helper()
	schema := "lajstrom_test_" + strings.ToLower(rand.Text())
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
		_ = db.Close()
	})
	return withSearchPath(dsn, schema)
}

// withSearchPath returns dsn, a URL or a keyword/value string, with search_path set.
func withSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err == nil {
			q := u.Query()
			q.Set("search_path", schema)
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	return dsn + " search_path=" + schema
}

// openMigrated opens dsn, migrates it and closes it when the test ends.
func openMigrated(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConformance(t *testing.T) {
	dsn := testDSN(t)
	storetest.Run(t, func(t *testing.T) index.Store {
		return openMigrated(t, schemaDSN(t, dsn))
	})
}

// TestConcurrentMigrate migrates one empty schema from two stores at once, as two replicas
// starting together do: the advisory lock lets each migration apply once.
func TestConcurrentMigrate(t *testing.T) {
	ctx := t.Context()
	dsn := schemaDSN(t, testDSN(t))
	stores := make([]*Store, 2)
	for i := range stores {
		s, err := Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		stores[i] = s
	}
	applied := make([][]int, len(stores))
	errs := make([]error, len(stores))
	var wg sync.WaitGroup
	for i, s := range stores {
		wg.Go(func() { applied[i], errs[i] = s.ApplyMigrations(ctx) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	want, err := index.MigrationVersions(index.DialectPostgres)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(applied[0]) + len(applied[1]); n != len(want) {
		t.Errorf("concurrent migrations applied %v and %v, want %v once", applied[0], applied[1], want)
	}
	got, err := stores[0].AppliedMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("AppliedMigrations = %v, want %v", got, want)
	}
}

func TestPoolSize(t *testing.T) {
	tests := []struct {
		dsn     string
		want    int
		wantErr bool
	}{
		{"postgres://u@db/lajstrom", max(4, runtime.NumCPU()), false},
		{"postgres://u@db/lajstrom?pool_max_conns=3&sslmode=disable", 3, false},
		{"host=db user=u pool_max_conns=7", 7, false},
		{"postgres://u@db/lajstrom?pool_max_conns=0", 0, true},
		{"postgres://u@db/lajstrom?pool_max_conns=many", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.dsn, func(t *testing.T) {
			cfg, err := pgx.ParseConfig(tt.dsn)
			if err != nil {
				t.Fatal(err)
			}
			got, err := poolSize(cfg)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("poolSize = %d, %v; want %d, error %v", got, err, tt.want, tt.wantErr)
			}
			if _, ok := cfg.RuntimeParams[poolParam]; ok {
				t.Errorf("%s is still a runtime parameter", poolParam)
			}
		})
	}
}

// TestOpenDoesNotLeakTheDSN checks that the errors of Open do not repeat a password.
func TestOpenDoesNotLeakTheDSN(t *testing.T) {
	for _, dsn := range []string{
		"postgres://lajstrom:s3cret@db:notaport/lajstrom",
		"host=db user=lajstrom password=s3cret port=notaport",
		"postgres://lajstrom:s3cret@db/lajstrom?pool_max_conns=x",
	} {
		_, err := Open(t.Context(), dsn)
		if err == nil || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("Open: %v, want an error without the password", err)
		}
	}
}

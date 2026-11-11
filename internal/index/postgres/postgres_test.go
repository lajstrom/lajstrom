package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/internal/sqlstore"
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
	return withSearchPath(dsn, newSchema(t, dsn))
}

// newSchema creates an empty schema, drops it when the test ends, and returns its name.
func newSchema(t *testing.T, dsn string) string {
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
	return schema
}

// withSearchPath returns dsn, a URL or a keyword/value string, with search_path set.
func withSearchPath(dsn, schema string) string { return withParam(dsn, "search_path", schema) }

// withParam returns dsn, a URL or a keyword/value string, with the run-time parameter key set.
func withParam(dsn, key, value string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err == nil {
			q := u.Query()
			q.Set(key, value)
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	return dsn + " " + key + "=" + value
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
		wg.Go(func() {
			rep, err := s.ApplyMigrations(ctx, index.MigrateOptions{})
			applied[i], errs[i] = rep.Applied, err
		})
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

// TestReplicasCoexist checks that the rows of replicas of two stores on one database coexist on
// Postgres, which serves many replicas.
func TestReplicasCoexist(t *testing.T) {
	ctx := t.Context()
	dsn := schemaDSN(t, testDSN(t))
	a, b := openMigrated(t, dsn), openMigrated(t, dsn)
	start := time.Now()
	for i, s := range []*Store{a, b} {
		r := index.Replica{ID: strconv.Itoa(i), Host: "host-" + strconv.Itoa(i), PID: i, Version: "v0.1.0", StartedAt: start}
		if err := s.StartReplica(ctx, &r, start.Add(-2*time.Minute)); err != nil {
			t.Fatalf("replica %d: %v", i, err)
		}
	}
	rows, err := a.ListReplicas(ctx)
	if err != nil || len(rows) != 2 {
		t.Errorf("ListReplicas = %v, %v; want both replicas", rows, err)
	}
}

// TestIsUnavailable checks which driver errors say that the database did not answer.
func TestIsUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"admin shutdown", &pgconn.PgError{Code: "57P01"}, true},
		{"crash shutdown", &pgconn.PgError{Code: "57P02"}, true},
		{"starting up", fmt.Errorf("connect: %w", &pgconn.PgError{Code: "57P03"}), true},
		{"too many connections", &pgconn.PgError{Code: "53300"}, true},
		{"connection failure", &pgconn.PgError{Code: "08006"}, true},
		{"cannot connect", &pgconn.PgError{Code: "08001"}, true},
		{"connect error", fmt.Errorf("index: begin: %w", &pgconn.ConnectError{}), true},
		{"connection reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"bad conn", driver.ErrBadConn, true},
		{"unexpected EOF", fmt.Errorf("failed to receive message: %w", io.ErrUnexpectedEOF), true},
		{"unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"wrong password", fmt.Errorf("connect: %w", &pgconn.PgError{Code: "28P01"}), false},
		{"undefined table", &pgconn.PgError{Code: "42P01"}, false},
		{"no rows", sql.ErrNoRows, false},
		{"canceled", context.Canceled, false},
		{"other", errors.New("boom"), false},
	}
	for _, tt := range tests {
		if got := isUnavailable(tt.err); got != tt.want {
			t.Errorf("%s: isUnavailable = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestNoServer checks that a store whose server does not listen fails every call with
// index.ErrUnavailable: on the pool, in a transaction, and for the referrers lock.
func TestNoServer(t *testing.T) {
	ctx := t.Context()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	cfg, err := pgx.ParseConfig("postgres://lajstrom@" + addr + "/lajstrom?sslmode=disable&connect_timeout=5")
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{sqlstore.New(stdlib.OpenDB(*cfg), dialect)}
	t.Cleanup(func() { _ = s.Close() })
	_, getErr := s.GetRepo(ctx, "raw", "scratch")
	calls := map[string]error{
		"Ping":       s.Ping(ctx),
		"GetRepo":    getErr,
		"CreateRepo": s.CreateRepo(ctx, &index.Repo{Format: "raw", Name: "scratch", Registry: "local", Kind: "hosted"}),
		"Tx":         s.Tx(ctx, func(index.Writer) error { return nil }),
		"WithReferrersLock": s.WithReferrersLock(ctx, "raw/scratch", digest.FromString("subject"),
			func(context.Context) error { return nil }),
	}
	for name, err := range calls {
		if !errors.Is(err, index.ErrUnavailable) {
			t.Errorf("%s: %v, want index.ErrUnavailable", name, err)
		}
	}
}

// TestTerminatedConnection ends the connection of a transaction from another session, as a
// server that shuts down does (57P01): the statement after it wraps index.ErrUnavailable, and
// the store serves again on a new connection.
func TestTerminatedConnection(t *testing.T) {
	ctx := t.Context()
	dsn := testDSN(t)
	app := "lajstrom_test_" + strings.ToLower(rand.Text())
	s := openMigrated(t, withParam(schemaDSN(t, dsn), "application_name", app))
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	err = s.Tx(ctx, func(w index.Writer) error {
		if _, err := w.ListRepos(ctx, ""); err != nil {
			return fmt.Errorf("before: %w", err)
		}
		var n int
		if err := admin.QueryRowContext(ctx, "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE application_name = $1",
			app).Scan(&n); err != nil || n != 1 {
			return fmt.Errorf("pg_terminate_backend: %d sessions, %w", n, err)
		}
		_, err := w.ListRepos(ctx, "")
		return err
	})
	if !errors.Is(err, index.ErrUnavailable) {
		t.Fatalf("a statement on a terminated connection: %v, want index.ErrUnavailable", err)
	}
	if _, err := s.ListRepos(ctx, ""); err != nil {
		t.Errorf("after the termination: %v", err)
	}
}

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// fakeSchema answers checkSchema with fixed results.
type fakeSchema struct {
	applied   []int
	err, ping error
}

func (f fakeSchema) Ping(context.Context) error { return f.ping }

func (f fakeSchema) AppliedMigrations(context.Context) ([]int, error) { return f.applied, f.err }

func TestCheckSchema(t *testing.T) {
	known, err := index.MigrationVersions(index.DialectSQLite)
	if err != nil {
		t.Fatal(err)
	}
	last := known[len(known)-1]
	unavailable := fmt.Errorf("index: ping: %w", index.ErrUnavailable)
	tests := []struct {
		name    string
		st      fakeSchema
		wantErr string // substring; "" means nil
		is      error
	}{
		{name: "current", st: fakeSchema{applied: known}},
		{name: "newer than the binary", st: fakeSchema{applied: append(known[:len(known):len(known)], last+1)}},
		{name: "older than the binary", st: fakeSchema{applied: known[:len(known)-1]},
			wantErr: fmt.Sprintf("the index schema is at version %04d, older than this binary's %04d: %s", known[len(known)-2], last, migrateHint)},
		{name: "no version", st: fakeSchema{applied: []int{}}, wantErr: "the index database is not migrated: " + migrateHint},
		{name: "no schema_migrations", st: fakeSchema{err: errors.New("no such table: schema_migrations")},
			wantErr: "the index database is not migrated: " + migrateHint + " (no such table: schema_migrations)"},
		{name: "database down", st: fakeSchema{err: errors.New("connection refused"), ping: unavailable}, is: index.ErrUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSchema(t.Context(), tt.st, index.DialectSQLite)
			switch {
			case tt.is != nil:
				if !errors.Is(err, tt.is) || strings.Contains(err.Error(), migrateHint) {
					t.Errorf("error = %v, want %v without the hint", err, tt.is)
				}
			case tt.wantErr == "" && err != nil:
				t.Errorf("error: %v", err)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Errorf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// unmigratedCommands are the commands that need the schema and do not migrate it.
var unmigratedCommands = [][]string{
	{"admin", "user", "list"},
	{"admin", "repo", "list"},
	{"admin", "binding", "list"},
	{"admin", "token", "list", "--user", "alice"},
	{"admin", "key", "export", "--token"},
	{"reindex"},
}

// emptyDatabase makes the SQLite file lajstrom.db in dir, empty: a database that migrate never
// ran on, which SQLite opens.
func emptyDatabase(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "lajstrom.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestUnmigrated runs the commands that need the schema and do not migrate it on a database
// that migrate never ran on: an empty SQLite file, and on Postgres (LAJSTROM_TEST_POSTGRES_DSN)
// a search_path without tables. Each names the command that migrates it (#575), and on SQLite
// the file (#719).
func TestUnmigrated(t *testing.T) {
	dir := t.TempDir()
	cfg, _ := writeKeyConfig(t, dir)
	emptyDatabase(t, dir)
	databases := map[string]map[string]string{"sqlite": {"LAJSTROM_MASTER_KEY": testMasterKey}}
	if dsn, ok := emptyPostgres(t); ok {
		databases["postgres"] = map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey,
			"LAJSTROM_DATABASE_DRIVER": "postgres", "LAJSTROM_DATABASE_DSN": dsn}
	}
	for name, env := range databases {
		want := "the index database is not migrated: " + migrateHint
		if name == "sqlite" {
			want = filepath.Join(dir, "lajstrom.db") + " (database.dsn): " + want
		}
		for _, args := range unmigratedCommands {
			t.Run(name+"/"+strings.Join(args, " "), func(t *testing.T) {
				_, _, err := run(t, env, append(args, "--config", cfg)...)
				if err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Errorf("error = %v, want it to start with %q", err, want)
				}
			})
		}
	}
	runEnv(t, cfg, nil, []step{{args: []string{"migrate"}}, {args: []string{"admin", "user", "list"}}})
}

// TestNoDatabase runs the commands that need the schema and do not migrate it with a SQLite file
// that does not exist: each says so, naming the absolute path and database.dsn, and creates
// nothing, also with a relative DSN in another working directory than the configuration's, which
// SQLite resolves against the working directory (#719). migrate and serve do create it.
func TestNoDatabase(t *testing.T) {
	dir, elsewhere := t.TempDir(), t.TempDir()
	cfg, _ := writeKeyConfig(t, dir)
	relative := filepath.Join(dir, "relative.yaml")
	body := "server:\n  listen: \":8080\"\n  externalURL: https://art.example.com\ndatabase:\n  dsn: lajstrom.db\n" +
		"keys:\n  masterKeyEnv: LAJSTROM_MASTER_KEY\n"
	if err := os.WriteFile(relative, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}
	t.Chdir(elsewhere)
	for _, c := range []struct{ cfg, file string }{{cfg, filepath.Join(dir, "lajstrom.db")}, {relative, filepath.Join(elsewhere, "lajstrom.db")}} {
		want := "no database at " + c.file + " (database.dsn): run lajstrom migrate there, or point --config/database.dsn at the server's"
		for _, args := range unmigratedCommands {
			_, _, err := run(t, env, append(args, "--config", c.cfg)...)
			if err == nil || err.Error() != want {
				t.Errorf("%s %v: %v, want %q", filepath.Base(c.cfg), args, err, want)
			}
		}
		if _, err := os.Stat(c.file); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: %s exists after the commands: %v", filepath.Base(c.cfg), c.file, err)
		}
	}
	runEnv(t, relative, env, []step{{args: []string{"migrate"}, wantOut: "applied 0001"}, {args: []string{"admin", "user", "list"}}})
	if _, err := os.Stat(filepath.Join(elsewhere, "lajstrom.db")); err != nil {
		t.Errorf("migrate did not make the file in the working directory: %v", err)
	}
}

// TestDatabaseFileInErrors: an error of SQLite on open names the absolute path and database.dsn,
// which SQLite's own "unable to open database file (14)" does not (#719); and serve's first line
// names the file, never the DSN of postgres.
func TestDatabaseFileInErrors(t *testing.T) {
	dir := t.TempDir()
	readOnly := filepath.Join(dir, "ro")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(readOnly, "lajstrom.db")
	_, err := openStore(t.Context(), config.Database{Driver: index.DialectSQLite, DSN: "file:" + file})
	if want := file + " (database.dsn): index: sqlite: open: "; os.Geteuid() != 0 && (err == nil || !strings.HasPrefix(err.Error(), want)) {
		t.Errorf("open in a read-only directory: %v, want it to start with %q", err, want) // root writes there
	}
	// serve, on a port that is taken: it logs its first line and opens the file before it listens.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	_, stderr, err := run(t, map[string]string{"LAJSTROM_SERVER_LISTEN": taken.Addr().String()}, "serve", "--config", writeConfig(t, dir))
	if first, _, _ := strings.Cut(stderr, "\n"); err == nil || !strings.Contains(first, `msg="starting lajstrom"`) ||
		!strings.Contains(first, " database.driver=sqlite database.file="+filepath.Join(dir, "lajstrom.db")+" ") {
		t.Errorf("serve: %v; first line %q, want database.file=%s", err, first, filepath.Join(dir, "lajstrom.db"))
	}
	for _, tt := range []struct {
		db   config.Database
		want string
	}{
		{config.Database{Driver: index.DialectSQLite, DSN: "file:" + file + "?_busy_timeout=1"}, "database.file=" + file},
		{config.Database{Driver: index.DialectSQLite, DSN: "file:x?mode=memory&cache=shared"}, ""},
		{config.Database{Driver: index.DialectPostgres, DSN: "postgres://u:p@db/x"}, ""}, //nolint:gosec // a fake password the line must not show
	} {
		var buf bytes.Buffer
		slog.New(slog.NewTextHandler(&buf, nil)).Info("starting lajstrom", databaseFile(tt.db))
		if got := buf.String(); (tt.want != "" && !strings.Contains(got, " "+tt.want+"\n")) || (tt.want == "" && strings.Contains(got, "database")) {
			t.Errorf("%s: %q, want %q", tt.db.Driver, got, tt.want)
		}
	}
}

// emptyPostgres returns the DSN of LAJSTROM_TEST_POSTGRES_DSN with a search_path that names no
// schema, where no table resolves, as in a database migrate never ran on. A transaction pooler
// does not carry search_path (LAJSTROM_TEST_POSTGRES_ISOLATION=database): skipped there.
func emptyPostgres(t *testing.T) (string, bool) {
	t.Helper()
	dsn := os.Getenv("LAJSTROM_TEST_POSTGRES_DSN")
	if dsn == "" || os.Getenv("LAJSTROM_TEST_POSTGRES_ISOLATION") == "database" {
		t.Log("not on Postgres: set LAJSTROM_TEST_POSTGRES_DSN, without LAJSTROM_TEST_POSTGRES_ISOLATION=database")
		return "", false
	}
	schema := "lajstrom_test_none_" + strings.ToLower(rand.Text())
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return dsn + " search_path=" + schema, true
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), true
}

// fakeClock is a database whose clock reads at, or fails with err.
type fakeClock struct {
	at  time.Time
	err error
}

func (f fakeClock) DatabaseTime(context.Context) (time.Time, error) { return f.at, f.err }

// TestCheckClock checks the comparison of the CLI's clock with the database's, with a clock of
// this host that runs off by skew: within 10 s nothing is logged; past it a warning; past 60 s
// the command is refused, naming the skew, ahead or behind. A clock that cannot be read is an
// error.
func TestCheckClock(t *testing.T) {
	db := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	down := fmt.Errorf("index: the database's clock: %w", index.ErrUnavailable)
	for _, tt := range []struct {
		name    string
		skew    time.Duration
		err     error  // of DatabaseTime
		level   string // of the line logged, "" for none
		wantErr string // a prefix; "" for none
	}{
		{name: "in sync"},
		{name: "10s ahead", skew: 10 * time.Second},
		{name: "15s ahead", skew: 15 * time.Second, level: "WARN"},
		{name: "15s behind", skew: -15 * time.Second, level: "WARN"},
		{name: "60s ahead", skew: 60 * time.Second, level: "WARN"},
		{name: "75s ahead", skew: 75 * time.Second, wantErr: "the clock of this host is 1m15s ahead of the database's; " +
			"the CLI and the replicas must agree within 60s: run NTP on this host and the database's, then run the command again"},
		{name: "75s behind", skew: -75 * time.Second, wantErr: "the clock of this host is 1m15s behind the database's; "},
		{name: "unreadable", err: down, wantErr: "read the database's clock: "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			err := checkClock(t.Context(), fakeClock{at: db, err: tt.err}, func() time.Time { return db.Add(tt.skew) },
				slog.New(slog.NewTextHandler(&logs, nil)))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.HasPrefix(err.Error(), tt.wantErr)):
				t.Errorf("error = %v, want it to start with %q", err, tt.wantErr)
			case tt.err != nil && !errors.Is(err, tt.err):
				t.Errorf("error = %v, want it to wrap %v", err, tt.err)
			}
			if level := logLevel(logs.String()); level != tt.level {
				t.Errorf("logged %q, want a line of level %q:\n%s", level, tt.level, logs.String())
			}
		})
	}
}

// TestOpenMigratedClock checks that the commands of the CLI that work on the index compare the
// clocks first: with this host's clock 75 s off the database's (SQLite, whose clock is the
// host's), admin refuses before its function runs, so nothing is written; in sync it runs.
func TestOpenMigratedClock(t *testing.T) {
	dir := t.TempDir()
	openIndex(t, dir) // migrated
	var skew time.Duration
	a := &app{cfg: &config.Config{Database: config.Database{Driver: index.DialectSQLite, DSN: "file:" + filepath.Join(dir, "lajstrom.db")}},
		log: slog.New(slog.DiscardHandler), now: func() time.Time { return time.Now().Add(skew) }}
	for _, tt := range []struct {
		skew    time.Duration
		wantErr string // a prefix; "" when the command runs
	}{
		{75 * time.Second, "the clock of this host is 1m15s ahead of the database's"},
		{-75 * time.Second, "the clock of this host is 1m15s behind the database's"},
		{0, ""},
	} {
		skew = tt.skew
		ran := false
		err := a.withUsers(t.Context(), func(context.Context, *auth.Users) error {
			ran = true
			return nil
		})
		switch {
		case tt.wantErr == "" && (err != nil || !ran):
			t.Errorf("skew %s: %v, ran %v; want it to run", tt.skew, err, ran)
		case tt.wantErr != "" && (err == nil || !strings.HasPrefix(err.Error(), tt.wantErr) || ran):
			t.Errorf("skew %s: %v, ran %v; want %q and not to run", tt.skew, err, ran, tt.wantErr)
		}
	}
}

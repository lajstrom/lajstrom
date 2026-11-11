package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
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

// TestUnmigrated runs the commands that need the schema and do not migrate it on a database
// that migrate never ran on: a new SQLite file, and on Postgres (LAJSTROM_TEST_POSTGRES_DSN) a
// search_path without tables. Each names the command that migrates it (#575).
func TestUnmigrated(t *testing.T) {
	cfg, _ := writeKeyConfig(t, t.TempDir())
	databases := map[string]map[string]string{"sqlite": {"LAJSTROM_MASTER_KEY": testMasterKey}}
	if dsn, ok := emptyPostgres(t); ok {
		databases["postgres"] = map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey,
			"LAJSTROM_DATABASE_DRIVER": "postgres", "LAJSTROM_DATABASE_DSN": dsn}
	}
	commands := [][]string{
		{"admin", "user", "list"},
		{"admin", "repo", "list"},
		{"admin", "binding", "list"},
		{"admin", "token", "list", "--user", "alice"},
		{"admin", "key", "export", "--token"},
		{"reindex"},
	}
	for name, env := range databases {
		for _, args := range commands {
			t.Run(name+"/"+strings.Join(args, " "), func(t *testing.T) {
				_, _, err := run(t, env, append(args, "--config", cfg)...)
				if want := "the index database is not migrated: " + migrateHint; err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Errorf("error = %v, want it to start with %q", err, want)
				}
			})
		}
	}
	runEnv(t, cfg, nil, []step{{args: []string{"migrate"}}, {args: []string{"admin", "user", "list"}}})
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

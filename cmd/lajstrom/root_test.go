package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// run executes the command tree with args and returns stdout, stderr and the error.
func run(t *testing.T, env map[string]string, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := newRootCmd(func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

// writeConfig writes a configuration file whose index database is a SQLite file in dir, and
// returns its path.
func writeConfig(t *testing.T, dir string) string {
	t.Helper()
	cfg := filepath.Join(dir, "lajstrom.yaml")
	body := "server:\n  listen: \":8080\"\n  externalURL: https://art.example.com\ndatabase:\n  driver: sqlite\n  dsn: \"file:" + filepath.Join(dir, "lajstrom.db") + "\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCommands(t *testing.T) {
	cfg := writeConfig(t, t.TempDir())
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	tests := []struct {
		name    string
		env     map[string]string
		args    []string
		wantErr string // substring; "" means success
		wantOut string // substring of stdout
		status  int    // the exit status of the error: 2 for the configuration, else 1
	}{
		{name: "version", args: []string{"version"}, wantOut: "lajstrom dev (unknown, unknown)\n"},
		{name: "reindex before migrate", args: []string{"reindex", "--config", cfg}, wantErr: "no such table"},
		{name: "config from env", env: map[string]string{"LAJSTROM_CONFIG": cfg}, args: []string{"reindex"}, wantErr: "no such table"},
		{name: "migrate missing config", args: []string{"migrate", "--config", missing}, wantErr: missing, status: 2},
		{name: "invalid config", env: map[string]string{"LAJSTROM_DATABASE_DRIVER": "mysql"}, args: []string{"migrate", "--config", cfg}, wantErr: `database.driver: "mysql"`, status: 2},
		{name: "serve missing config", args: []string{"serve", "--config", missing}, wantErr: "no such file", status: 2},
		// docs/acceptance.md C-02 and C-03.
		{name: "serve with a path in externalURL", env: map[string]string{"LAJSTROM_SERVER_EXTERNAL_URL": "https://art.test/sub"},
			args: []string{"serve", "--config", cfg}, wantErr: "server.externalURL: must be https://<host>[:port]: no path", status: 2},
		{name: "serve with an http externalURL", env: map[string]string{"LAJSTROM_SERVER_EXTERNAL_URL": "http://art.test"},
			args: []string{"serve", "--config", cfg}, wantErr: "TLS only; http is for the dev stack on a loopback host, with server.insecureHTTP", status: 2},
		{name: "admin user", args: []string{"admin", "user"}, wantOut: "Available Commands:\n  create"},
		{name: "admin token", args: []string{"admin", "token"}, wantOut: "Available Commands:\n  create"},
		{name: "admin repo", args: []string{"admin", "repo"}, wantOut: "Available Commands:\n  create"},
		{name: "admin binding", args: []string{"admin", "binding"}, wantOut: "Available Commands:\n  create"},
		{name: "bad log level", args: []string{"version", "--log-level", "verbose"}, wantErr: `log level "verbose"`},
		{name: "bad log format", args: []string{"version", "--log-format", "xml"}, wantErr: `log format "xml"`},
		{name: "unknown command", args: []string{"frobnicate"}, wantErr: "unknown command"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _, err := run(t, tt.env, tt.args...)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			if want := max(tt.status, 1); err != nil && exitStatus(err) != want {
				t.Errorf("exit status %d, want %d", exitStatus(err), want)
			}
			if !strings.Contains(out, tt.wantOut) {
				t.Errorf("stdout = %q, want it to contain %q", out, tt.wantOut)
			}
		})
	}
}

func TestVersionJSON(t *testing.T) {
	out, _, err := run(t, nil, "version", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	for _, k := range []string{"version", "commit", "date"} {
		if _, ok := got[k]; !ok {
			t.Errorf("version --json has no %q: %s", k, out)
		}
	}
}

// TestMigrate runs migrate three times on a new SQLite file: the first run applies every
// migration, the next ones report the current version, and the file has every table.
func TestMigrate(t *testing.T) {
	dir := t.TempDir()
	cfg := writeConfig(t, dir)
	versions, err := index.MigrationVersions(index.DialectSQLite)
	if err != nil {
		t.Fatal(err)
	}
	var first strings.Builder
	for _, v := range versions {
		fmt.Fprintf(&first, "applied %04d\n", v)
	}
	current := fmt.Sprintf("schema is current at version %04d\n", versions[len(versions)-1])
	for _, want := range []string{first.String(), current, current} {
		out, _, err := run(t, nil, "migrate", "--config", cfg)
		if err != nil {
			t.Fatal(err)
		}
		if out != want {
			t.Errorf("migrate printed %q, want %q", out, want)
		}
	}

	st, err := sqlite.Open(t.Context(), "file:"+filepath.Join(dir, "lajstrom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := t.Context()
	for _, tt := range []struct {
		table string
		err   error
	}{
		{"schema_migrations", second(st.AppliedMigrations(ctx))},
		{"repos", second(st.ListRepos(ctx, ""))},
		{"users", second(st.ListUsers(ctx))},
		{"tokens", second(st.ListTokens(ctx, nil, nil))},
		{"bindings", second(st.ListBindings(ctx, ""))},
		{"keys", second(st.ListKeys(ctx, nil))},
		{"sessions", second(st.GetSessionByID(ctx, 1))},
		{"audit", second(st.ListAudit(ctx, index.AuditFilter{}))},
		{"packages", second(st.ListPackages(ctx, 1, ""))},
		{"versions", second(st.ListVersions(ctx, 1))},
		{"files", second(st.ListFiles(ctx, 1))},
		{"index_cache", second(st.GetIndexCache(ctx, 1, "s", "k"))},
		{"oci_repos", second(st.ListOCIRepos(ctx, 1))},
	} {
		if tt.err != nil && !errors.Is(tt.err, index.ErrNotFound) {
			t.Errorf("table %s: %v", tt.table, tt.err)
		}
	}
}

// second returns the error of a two-valued call.
func second[T any](_ T, err error) error { return err }

// TestReady checks the readiness check of serve: nil while the database answers, and an error
// without the driver's message when it does not.
func TestReady(t *testing.T) {
	st, err := openStore(t.Context(), config.Database{Driver: "sqlite", DSN: "file:" + filepath.Join(t.TempDir(), "x.db")})
	if err != nil {
		t.Fatal(err)
	}
	check := ready(slog.New(slog.DiscardHandler), "sqlite", st, newRegistryHealth(nil, slog.New(slog.DiscardHandler)), &replica{})
	if err := check(t.Context()); err != nil {
		t.Errorf("ready with an open database: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := check(t.Context()); err == nil || err.Error() != "index database is unreachable" {
		t.Errorf("ready with a closed database: %v", err)
	}
}

func TestOpenStoreUnknownDriver(t *testing.T) {
	if _, err := openStore(t.Context(), config.Database{Driver: "mysql", DSN: "x"}); err == nil {
		t.Error("openStore accepted the driver mysql")
	}
}

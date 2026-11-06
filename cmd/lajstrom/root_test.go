package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestCommands(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "lajstrom.yaml")
	if err := os.WriteFile(cfg, []byte("server:\n  listen: \":8080\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	tests := []struct {
		name    string
		env     map[string]string
		args    []string
		wantErr string // substring; "" means success
		wantOut string // substring of stdout
	}{
		{name: "version", args: []string{"version"}, wantOut: "lajstrom dev (unknown, unknown)\n"},
		{name: "migrate", args: []string{"migrate", "--config", cfg}, wantErr: "not implemented, see issue #10"},
		{name: "reindex", args: []string{"reindex", "--config", cfg}, wantErr: "not implemented, see issue #29"},
		{name: "config from env", env: map[string]string{"LAJSTROM_CONFIG": cfg}, args: []string{"migrate"}, wantErr: "see issue #10"},
		{name: "migrate missing config", args: []string{"migrate", "--config", missing}, wantErr: missing},
		{name: "invalid config", env: map[string]string{"LAJSTROM_DATABASE_DRIVER": "mysql"}, args: []string{"migrate", "--config", cfg}, wantErr: `database.driver: "mysql"`},
		{name: "serve missing config", args: []string{"serve", "--config", missing}, wantErr: "no such file"},
		{name: "admin user", args: []string{"admin", "user", "create", "alice"}, wantErr: "not implemented, see issue #47"},
		{name: "admin token", args: []string{"admin", "token"}, wantErr: "see issue #47"},
		{name: "admin repo", args: []string{"admin", "repo"}, wantErr: "see issue #14"},
		{name: "admin binding", args: []string{"admin", "binding"}, wantErr: "see issue #47"},
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

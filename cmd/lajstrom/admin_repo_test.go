package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// writeRepoConfig writes a configuration whose index database is a SQLite file in dir, with
// one registry, local, and returns its path and the database's DSN.
func writeRepoConfig(t *testing.T, dir string) (cfg, dsn string) {
	t.Helper()
	cfg, dsn = filepath.Join(dir, "lajstrom.yaml"), "file:"+filepath.Join(dir, "lajstrom.db")
	body := "server:\n  listen: \":8080\"\n  externalURL: https://art.example.com\ndatabase:\n  driver: sqlite\n  dsn: \"" + dsn + "\"\n" +
		"registries:\n  - name: local\n    url: \"http://localhost:5000\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, dsn
}

// step is one command of a scripted test, run in order.
type step struct {
	args    []string
	wantErr string // substring; "" means success
	wantOut string // substring of stdout
}

// runSteps runs steps with --config cfg appended.
func runSteps(t *testing.T, cfg string, steps []step) {
	t.Helper()
	for _, s := range steps {
		out, _, err := run(t, nil, append(s.args, "--config", cfg)...)
		switch {
		case s.wantErr == "" && err != nil:
			t.Fatalf("%v: %v", s.args, err)
		case s.wantErr != "" && (err == nil || !strings.Contains(err.Error(), s.wantErr)):
			t.Fatalf("%v: error = %v, want it to contain %q", s.args, err, s.wantErr)
		}
		if !strings.Contains(out, s.wantOut) {
			t.Errorf("%v: stdout = %q, want it to contain %q", s.args, out, s.wantOut)
		}
	}
}

// TestAdminRepo runs admin repo create, list and delete against a migrated SQLite file: create,
// list shows it, delete without --force refuses a repo with a version, with --force deletes it.
func TestAdminRepo(t *testing.T) {
	cfg, dsn := writeRepoConfig(t, t.TempDir())
	runSteps(t, cfg, []step{
		{args: []string{"migrate"}, wantOut: "applied 0001"},
		{args: []string{"admin", "repo", "create", "raw/scratch", "--registry", "local"},
			wantOut: `{"format":"raw","name":"scratch","registry":"local","kind":"hosted","anonymousRead":false,"policy":null,"createdAt":"`},
		{args: []string{"admin", "repo", "create", "raw/scratch", "--registry", "local"}, wantErr: "repo raw/scratch already exists"},
		{args: []string{"admin", "repo", "create", "raw/docs", "--registry", "local", "--anonymous-read", "--policy", `{ "maxFileSize": 1 }`},
			wantOut: `"anonymousRead":true,"policy":{"maxFileSize":1}`},
		{args: []string{"admin", "repo", "create", "raw/bad", "--registry", "zot"}, wantErr: `registry: "zot" is not a configured registry`},
		{args: []string{"admin", "repo", "create", "raw/bad", "--registry", "local", "--policy", "[]"}, wantErr: "policy: must be a JSON object"},
		{args: []string{"admin", "repo", "create", "nope/bad", "--registry", "local"}, wantErr: `format: "nope" is not a format this server has`},
		{args: []string{"admin", "repo", "create", "raw/bad"}, wantErr: "registry: must not be empty"},
		{args: []string{"admin", "repo", "create", "raw/bad", "--kind", "group", "--group", `{"members": ["scratch"]}`},
			wantErr: "kind: format raw has no group repos"},
		{args: []string{"admin", "repo", "create", "raw/bad", "--registry", "local", "--group", `{"members": ["scratch"]}`},
			wantErr: "group: only a group repo has one, not a hosted repo"},
		{args: []string{"admin", "repo", "create", "raw/bad", "--registry", "local", "--kind", "proxy", "--upstream", `{"name": "npmjs"}`},
			wantErr: "kind: format raw has no proxy repos"},
		{args: []string{"admin", "repo", "create", "raw/bad", "--registry", "local", "--upstream", `{"name": "npmjs"}`},
			wantErr: "upstream: only a proxy repo has one, not a hosted repo"},
		{args: []string{"admin", "repo", "create", "scratch", "--registry", "local"}, wantErr: "want <format>/<name>"},
		{args: []string{"admin", "repo", "list"}, wantOut: "FORMAT  NAME     REGISTRY  ANONYMOUS-READ  CREATED\nraw     docs     local     true            "},
		{args: []string{"admin", "repo", "list", "--format", "raw"}, wantOut: "\nraw     scratch  local     false           "},
		{args: []string{"admin", "repo", "list", "--format", "nope"}, wantErr: `format: "nope"`},
	})

	addCLIVersion(t, dsn, "raw", "scratch")
	runSteps(t, cfg, []step{
		{args: []string{"admin", "repo", "delete", "raw/scratch"}, wantErr: "repo raw/scratch has 1 version; delete them or pass force=true"},
		{args: []string{"admin", "repo", "delete", "raw/scratch", "--force"}},
		{args: []string{"admin", "repo", "delete", "raw/scratch"}, wantErr: "repo raw/scratch does not exist"},
		{args: []string{"admin", "repo", "delete", "raw/docs"}},
	})

	out, _, err := run(t, nil, "admin", "repo", "list", "--json", "--config", cfg)
	if err != nil || out != "{\"repos\":[]}\n" {
		t.Errorf("list --json = %q, %v; want no repos", out, err)
	}
	wantCLIAudit(t, dsn, "repo.delete", 2)
	wantCLIAudit(t, dsn, "repo.create", 2)
}

// TestAdminRepoApt creates an apt repo: the binary imports the deb plugin (#39).
func TestAdminRepoApt(t *testing.T) {
	cfg, _ := writeRepoConfig(t, t.TempDir())
	runSteps(t, cfg, []step{
		{args: []string{"migrate"}, wantOut: "applied 0001"},
		{args: []string{"admin", "repo", "create", "apt/ubuntu", "--registry", "local"}, wantOut: `{"format":"apt","name":"ubuntu","registry":"local"`},
		{args: []string{"admin", "repo", "list", "--format", "apt"}, wantOut: "\napt     ubuntu  local"},
	})
}

// TestAdminRepoGroup creates an npm group: the binary's npm plugin has group repos (#311); a
// member is not deleted while the group lists it.
func TestAdminRepoGroup(t *testing.T) {
	cfg, _ := writeRepoConfig(t, t.TempDir())
	runSteps(t, cfg, []step{
		{args: []string{"migrate"}, wantOut: "applied 0001"},
		{args: []string{"admin", "repo", "create", "npm/internal", "--registry", "local"}, wantOut: `"kind":"hosted"`},
		{args: []string{"admin", "repo", "create", "npm/all", "--kind", "group", "--group", `{"members": ["internal"]}`},
			wantOut: `{"format":"npm","name":"all","kind":"group","anonymousRead":false,"group":{"members":["internal"],"merge":[]},`},
		{args: []string{"admin", "repo", "list", "--format", "npm"}, wantOut: "\nnpm     all       -         false"},
		{args: []string{"admin", "repo", "delete", "npm/internal"}, wantErr: "repo npm/internal is a member of the group(s) npm/all"},
		{args: []string{"admin", "repo", "delete", "npm/all"}},
		{args: []string{"admin", "repo", "delete", "npm/internal"}},
	})
}

// addCLIVersion adds a package with one version to a repo of the database at dsn.
func addCLIVersion(t *testing.T, dsn, formatName, name string) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repo, err := st.GetRepo(ctx, formatName, name)
	if err != nil {
		t.Fatal(err)
	}
	pkg := index.Package{RepoID: repo.ID, Name: "readme.md", NameOCI: "readme.md"}
	if err := st.UpsertPackage(ctx, &pkg); err != nil {
		t.Fatal(err)
	}
	v := index.Version{PackageID: pkg.ID, Version: "0123456789abcdef", Tag: "0123456789abcdef",
		Digest: digest.FromString("readme"), ArtifactType: "application/vnd.lajstrom.raw.v1"}
	if err := st.UpsertVersion(ctx, &v); err != nil {
		t.Fatal(err)
	}
}

// wantCLIAudit checks that the database at dsn has n audit events of action, each by the
// anonymous principal from the CLI.
func wantCLIAudit(t *testing.T, dsn, action string, n int) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	evs, err := st.ListAudit(ctx, index.AuditFilter{Action: action})
	if err != nil || len(evs) != n {
		t.Fatalf("%s events = %+v, %v; want %d", action, evs, err, n)
	}
	host, _ := os.Hostname()
	for _, ev := range evs {
		var details map[string]any
		if err := json.Unmarshal(ev.Details, &details); err != nil || details["source"] != api.SourceCLI ||
			details["host"] != host || ev.Principal != "anonymous" || ev.IP != "" {
			t.Errorf("%s event = %+v, details %s", action, ev, ev.Details)
		}
	}
}

// admin does not migrate: on a database without the schema it names the command that does.
func TestAdminRepoWithoutMigrate(t *testing.T) {
	cfg, _ := writeRepoConfig(t, t.TempDir())
	if _, _, err := run(t, nil, "admin", "repo", "list", "--config", cfg); err == nil || !strings.Contains(err.Error(), "not migrated: "+migrateHint) {
		t.Errorf("list before migrate: %v, want the hint to migrate", err)
	}
}

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index/reindex"
)

// TestReindex runs reindex against a migrated SQLite file whose one registry does not answer:
// the flags are checked, a run over no repo succeeds, a repo on the silent registry is a run
// that is done with the listing in its errors and nothing removed, and --json prints the
// report.
func TestReindex(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "lajstrom.yaml")
	body := "server:\n  listen: \":8080\"\ndatabase:\n  driver: sqlite\n  dsn: \"file:" + filepath.Join(dir, "lajstrom.db") + "\"\n" +
		"registries:\n  - name: local\n    url: \"http://127.0.0.1:1\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	runSteps(t, cfg, []step{
		{args: []string{"migrate"}, wantOut: "applied 0001"},
		{args: []string{"reindex"}, wantOut: "REPO  MANIFESTS"},
		{args: []string{"reindex", "--repo", "raw"}, wantErr: `--repo "raw" is not <format>/<name>`},
		{args: []string{"reindex", "--repo", "raw/missing"}, wantErr: "repo raw/missing does not exist"},
		{args: []string{"admin", "repo", "create", "raw/assets", "--registry", "local"}},
		{args: []string{"reindex", "--repo", "raw/assets", "--report"}, wantOut: "raw/assets: error :"},
	})
	out, _, err := run(t, nil, "reindex", "--json", "--config", cfg)
	if err != nil {
		t.Fatal(err)
	}
	var rep reindex.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("--json: %v in %q", err, out)
	}
	if len(rep.Repos) != 1 || rep.Repos[0].Repo != "raw/assets" || len(rep.Repos[0].Errors) != 1 ||
		!strings.Contains(rep.Repos[0].Errors[0].Error, "list the OCI repositories") || rep.Repos[0].Removed != 0 {
		t.Errorf("report: %+v", rep)
	}
}

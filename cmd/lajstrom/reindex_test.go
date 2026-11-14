package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
)

// TestWriteReportRepairedReferrers: a sha256-<hex> index that ingest pushed again for the run
// (#628) is a count of the table and, with --report, a line with the index and how many it added.
func TestWriteReportRepairedReferrers(t *testing.T) {
	subject, idx := digest.FromString("subject"), digest.FromString("index")
	rep := &reindex.Report{Repos: []reindex.RepoReport{{Repo: "raw/assets", RepairedReferrers: []reindex.RepairedIndex{{
		Repository: "notes", Subject: subject, Index: idx, Added: []digest.Digest{digest.FromString("a"), digest.FromString("b")}}}}}}
	var out strings.Builder
	if err := writeReport(&out, rep, true); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if fields := strings.Fields(lines[1]); !strings.Contains(lines[0], "REFERRERS  REPAIRED") || len(fields) != 17 || fields[10] != "1" {
		t.Errorf("the table: %q", lines[:2])
	}
	if want := "raw/assets: repaired referrers index notes:sha256-" + subject.Encoded() + " " + idx.String() + ", added 2"; lines[2] != want {
		t.Errorf("the list: %q, want %q", lines[2], want)
	}
}

// TestReindexAttach runs reindex --attach against a registry that does not answer: it needs
// --repo, a file that names an OCI repository, and names below the repo's path; then the run
// reads each one, whose tag list is an error of the report.
func TestReindexAttach(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := write("lajstrom.yaml", "server:\n  listen: \":8080\"\n  externalURL: https://art.example.com\ndatabase:\n  driver: sqlite\n  dsn: \"file:"+
		filepath.Join(dir, "lajstrom.db")+"\"\nregistries:\n  - name: local\n    url: \"http://127.0.0.1:1\"\n")
	good := write("good.txt", "# raw/assets on the old registry\nraw/assets/docs/readme.md\n\n  raw/assets/bin/tool  \n")
	runSteps(t, cfg, []step{
		{args: []string{"migrate"}, wantOut: "applied 0001"},
		{args: []string{"admin", "repo", "create", "raw/assets", "--registry", "local"}},
		{args: []string{"reindex", "--attach", good}, wantErr: "--attach needs --repo"},
		{args: []string{"reindex", "--repo", "raw/assets", "--attach", filepath.Join(dir, "missing.txt")}, wantErr: "--attach: open "},
		{args: []string{"reindex", "--repo", "raw/assets", "--attach", write("empty.txt", "# none\n\n")}, wantErr: "names no OCI repository"},
		{args: []string{"reindex", "--repo", "raw/assets", "--attach", write("other.txt", "raw/other/x\n")},
			wantErr: `"raw/other/x" is not an OCI repository of raw/assets`},
		{args: []string{"reindex", "--repo", "raw/assets", "--attach", good, "--report"},
			wantOut: "raw/assets: error bin/tool: list the tags: "},
	})
}

// TestReindex runs reindex against a migrated SQLite file whose one registry does not answer:
// the flags are checked, a run over no repo succeeds, a repo on the silent registry is a run
// that is done with the listing in its errors and nothing removed, and --json prints the
// report.
func TestReindex(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "lajstrom.yaml")
	body := "server:\n  listen: \":8080\"\n  externalURL: https://art.example.com\ndatabase:\n  driver: sqlite\n  dsn: \"file:" + filepath.Join(dir, "lajstrom.db") + "\"\n" +
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

// envReindexProcess names the configuration of reindex in the child process of TestReindexHangup.
const envReindexProcess = "LAJSTROM_TEST_REINDEX_CONFIG"

// TestReindexProcess is reindex in the child process of TestReindexHangup, with the
// configuration of LAJSTROM_TEST_REINDEX_CONFIG; without it, it is skipped.
func TestReindexProcess(t *testing.T) {
	cfg := os.Getenv(envReindexProcess)
	if cfg == "" {
		t.Skip("the child process of TestReindexHangup")
	}
	cmd := newRootCmd(func(string) (string, bool) { return "", false })
	cmd.SetArgs([]string{"reindex", "--config", cfg})
	_ = cmd.ExecuteContext(context.Background()) // a drained run is an error of its own
}

// TestReindexHangup checks that reindex drains on SIGHUP, which closing its terminal or an SSH
// session sends, as it does on SIGTERM: the run row ends, not left running to block the next
// run of its scope until it goes stale (#751). The registry answers no request of reindex, so
// the run waits on it until the signal. Before reindex starts it answers 404 at once: admin repo
// create asks it for its capabilities (#663), and must neither wait for its probe's timeout nor
// count as reindex's first request, which would send the signal before reindex handles it.
func TestReindexHangup(t *testing.T) {
	waiting := make(chan struct{}, 1)
	var hang atomic.Bool
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hang.Load() {
			http.NotFound(w, r)
			return
		}
		select {
		case waiting <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer reg.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "lajstrom.yaml")
	body := "server:\n  listen: \":8080\"\n  externalURL: https://art.example.com\ndatabase:\n  driver: sqlite\n  dsn: \"file:" + filepath.Join(dir, "lajstrom.db") + "\"\n" +
		"registries:\n  - name: local\n    url: \"" + reg.URL + "\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	runSteps(t, cfg, []step{
		{args: []string{"migrate"}, wantOut: "applied 0001"},
		{args: []string{"admin", "repo", "create", "raw/assets", "--registry", "local"}},
	})
	hang.Store(true)
	cmd := exec.Command(os.Args[0], "-test.run=^TestReindexProcess$") //nolint:gosec // the test binary
	cmd.Env = append(os.Environ(), envReindexProcess+"="+cfg)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	select {
	case <-waiting:
	case err := <-exited:
		t.Fatalf("reindex ended before the registry: %v; its output:\n%s", err, out.String())
	case <-time.After(20 * time.Second):
		t.Fatalf("reindex has not asked the registry after 20 s; its output:\n%s", out.String())
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil { // TestReindexProcess ignores the error of the drained run: a kill is left
			t.Errorf("reindex after SIGHUP: %v, want it drained, not killed", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("reindex has not ended 30 s after SIGHUP; its output:\n%s", out.String())
	}
	run, err := openIndex(t, dir).GetReindexRun(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if run.State == index.ReindexRunning || run.FinishedAt == nil {
		t.Errorf("the run after SIGHUP: %s, finished %v, want it ended; its output:\n%s", run.State, run.FinishedAt, out.String())
	}
}

// TestWriteReportSplit: a split package is a count of the table and, with --report, a line with
// both forms and the tags at each (#609).
func TestWriteReportSplit(t *testing.T) {
	rep := &reindex.Report{Repos: []reindex.RepoReport{{Repo: "raw/assets", Split: []reindex.Split{{Package: "netlify/blobs",
		Indexed: "netlify/blob_73", IndexedTags: []string{"3fc4ccfe745870e2"}, Other: "netlify/blobs",
		OtherTags: []string{"7692c3ad3540bb80", "9a0364b9e99bb480"}}}}}}
	var out strings.Builder
	if err := writeReport(&out, rep, true); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if fields := strings.Fields(lines[1]); !strings.Contains(lines[0], "DRIFT  SPLIT  NOREPO  ERRORS") || len(fields) != 17 || fields[14] != "1" {
		t.Errorf("the table: %q", lines[:2])
	}
	want := "raw/assets: split netlify/blobs at netlify/blob_73 [3fc4ccfe745870e2] and at netlify/blobs " +
		"[7692c3ad3540bb80 9a0364b9e99bb480]"
	if lines[2] != want {
		t.Errorf("the list: %q, want %q", lines[2], want)
	}
}

// TestWriteReportRepositoryUnknown checks the CLI's table and --report line of a version whose
// OCI repository the registry does not know (#567): the NOREPO column, and the request of the
// API that deletes it.
func TestWriteReportRepositoryUnknown(t *testing.T) {
	rep := &reindex.Report{Repos: []reindex.RepoReport{{Repo: "raw/assets", RepositoryUnknown: []reindex.RepositoryUnknown{{
		Repository: "docs/gone.txt", Package: "docs/gone.txt", Version: "0123456789abcdef", Tag: "0123456789abcdef",
		Digest: "sha256:4e07408562bedb8b60ce05c1decfe3ad16b72230967de01f640b7e4729b49fce",
		Delete: "DELETE /api/v1/repos/raw/assets/packages/docs%2Fgone.txt/versions/0123456789abcdef"}}}}}
	var out strings.Builder
	if err := writeReport(&out, rep, true); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if fields := strings.Fields(lines[1]); len(fields) != 17 || fields[15] != "1" || fields[16] != "0" {
		t.Errorf("the table: %q", lines[:2])
	}
	want := "raw/assets: repository unknown docs/gone.txt:0123456789abcdef " +
		"sha256:4e07408562bedb8b60ce05c1decfe3ad16b72230967de01f640b7e4729b49fce, delete with: " +
		"DELETE /api/v1/repos/raw/assets/packages/docs%2Fgone.txt/versions/0123456789abcdef"
	if lines[2] != want {
		t.Errorf("the list: %q, want %q", lines[2], want)
	}
}

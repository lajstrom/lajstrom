package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// fakePrefetchAPI is POST …/npm/legacy/prefetch as admin repo prefetch sends it: a name is
// fetched the first time and held after, but the names of broken, which fail with 502 until
// they are mended; a batch of index i in refuse is answered with that status.
type fakePrefetchAPI struct {
	mu      sync.Mutex
	held    map[string]bool
	broken  map[string]bool
	refuse  map[int]int
	batches []api.PrefetchRequest
}

func (f *fakePrefetchAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos/npm/legacy/prefetch" || r.Header.Get("Authorization") != "Bearer ljs_pat_secret" {
		http.Error(w, `{"error": {"code": "NOT_FOUND", "message": "no"}}`, http.StatusNotFound)
		return
	}
	var req api.PrefetchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.batches = append(f.batches, req)
	if status := f.refuse[len(f.batches)-1]; status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error": {"code": "FORBIDDEN", "message": "manage on npm/legacy"}}`))
		return
	}
	var out api.PrefetchAnswer
	for _, n := range req.Names {
		switch {
		case f.broken[n]:
			out.Results = append(out.Results, proxy.PrefetchResult{Name: n, Version: "1.0.0", Status: proxy.PrefetchFailed,
				HTTPStatus: http.StatusBadGateway, Message: "checksum"})
		case f.held[n]:
			out.Results = append(out.Results, proxy.PrefetchResult{Name: n, Version: "1.0.0", Status: "held"})
		default:
			f.held[n] = true
			out.Results = append(out.Results, proxy.PrefetchResult{Name: n, Version: "1.0.0", Status: "fetched"})
		}
	}
	_ = json.NewEncoder(w).Encode(out)
}

// sent returns the names of the batches, one string per batch.
func (f *fakePrefetchAPI) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, b := range f.batches {
		out = append(out, strings.Join(b.Names, " "))
	}
	f.batches = nil
	return out
}

// readReport reads a report of admin repo prefetch.
func readReport(t *testing.T, path string) prefetchReport {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	must(t, err)
	var rep prefetchReport
	must(t, json.Unmarshal(b, &rep))
	return rep
}

// newPrefetchRun serves f and returns the environment of admin repo prefetch against it, with
// a token in a file, a names file of five names with comments and a repeat, and the path of a
// report.
func newPrefetchRun(t *testing.T, f *fakePrefetchAPI) (env map[string]string, names, report string) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	must(t, os.WriteFile(token, []byte("ljs_pat_secret\n"), 0o600))
	names = filepath.Join(dir, "names")
	must(t, os.WriteFile(names, []byte("# the private scope\n@acme/a\n@acme/b  # two\n\nlib\nbad\n@acme/a\ntool\n"), 0o600))
	return map[string]string{envRetentionURL: srv.URL, envRetentionTokenFile: token}, names, filepath.Join(dir, "report.json")
}

// TestAdminPrefetch: the names of the file, once each and without comments, go in batches of
// --batch, with --all-versions; a line per version and the counts are printed, and --report has
// the results; a failure exits 1, and --failed with the report sends its failed names again, and
// exits 0 once nothing fails.
func TestAdminPrefetch(t *testing.T) {
	f := &fakePrefetchAPI{held: map[string]bool{"lib": true}, broken: map[string]bool{"bad": true}}
	env, names, report := newPrefetchRun(t, f)

	out, _, err := run(t, env, "admin", "repo", "prefetch", "npm/legacy", "--from-file", names, "--batch", "2", "--all-versions",
		"--report", report)
	if exitStatus(err) != 1 {
		t.Fatalf("first run: %v, want exit status 1; out %s", err, out)
	}
	if d := cmp.Diff([]string{"@acme/a @acme/b", "lib bad", "tool"}, f.sent()); d != "" {
		t.Errorf("batches (-want +got)\n%s", d)
	}
	for _, want := range []string{"fetched  @acme/a 1.0.0\n", "held     lib 1.0.0\n", "failed   bad 1.0.0: 502 checksum\n",
		"npm/legacy: 5 names: 3 fetched, 1 held, 0 shared, 1 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q, want %q", out, want)
		}
	}
	rep := readReport(t, report)
	if rep.Repo != "npm/legacy" || !rep.AllVersions || rep.Names != 5 || len(rep.Results) != 5 || rep.Counts[proxy.PrefetchFailed] != 1 {
		t.Errorf("report %+v", rep)
	}

	f.broken = nil
	out, _, err = run(t, env, "admin", "repo", "prefetch", "npm/legacy", "--from-file", report, "--failed", "--report", report)
	if err != nil || !slices.Equal(f.sent(), []string{"bad"}) || !strings.Contains(out, "1 fetched") {
		t.Errorf("--failed: %v, out %s", err, out)
	}
	if rep := readReport(t, report); rep.Names != 1 || rep.Counts[proxy.PrefetchFailed] != 0 {
		t.Errorf("the report of --failed: %+v", rep)
	}
}

// TestAdminPrefetchRefused: a batch the server refuses fails its names and stops the run: the
// names after it are not attempted, and the report says why of each.
func TestAdminPrefetchRefused(t *testing.T) {
	f := &fakePrefetchAPI{held: map[string]bool{}, refuse: map[int]int{1: http.StatusForbidden}}
	env, names, report := newPrefetchRun(t, f)
	out, _, err := run(t, env, "admin", "repo", "prefetch", "npm/legacy", "--from-file", names, "--batch", "2", "--report", report)
	if exitStatus(err) != 1 || len(f.sent()) != 2 {
		t.Errorf("a refused batch: %v, want exit status 1 after 2 requests; out %s", err, out)
	}
	rep := readReport(t, report)
	want := []string{"", "", "403 Forbidden: FORBIDDEN: manage on npm/legacy", "403 Forbidden: FORBIDDEN: manage on npm/legacy",
		"not attempted: an earlier request failed"}
	for i, r := range rep.Results {
		if !strings.Contains(r.Message, want[i]) || (want[i] != "") != (r.Status == proxy.PrefetchFailed) {
			t.Errorf("result %d: %+v, want the message %q", i, r, want[i])
		}
	}
}

// TestAdminPrefetchUsage: a usage error exits 2 before a request is sent.
func TestAdminPrefetchUsage(t *testing.T) {
	f := &fakePrefetchAPI{held: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	env := map[string]string{envRetentionURL: srv.URL, envRetentionToken: "ljs_pat_secret"} //nolint:gosec // a token of the test
	names, empty := filepath.Join(dir, "names"), filepath.Join(dir, "empty")
	must(t, os.WriteFile(names, []byte("a\n"), 0o600))
	must(t, os.WriteFile(empty, []byte("# nothing\n\n"), 0o600))
	for _, tt := range []struct {
		env  map[string]string
		args []string
		want string
	}{
		{env, []string{}, "one argument"},
		{env, []string{"npm", "--from-file", names}, "<format>/<name>"},
		{env, []string{"npm/legacy"}, "--from-file: name the file"},
		{env, []string{"npm/legacy", "--from-file", empty}, "no name in it"},
		{env, []string{"npm/legacy", "--from-file", filepath.Join(dir, "none")}, "no such file"},
		{env, []string{"npm/legacy", "--from-file", names, "--failed"}, "not a report"},
		{env, []string{"npm/legacy", "--from-file", names, "--batch", "101"}, "--batch: 101"},
		{env, []string{"npm/legacy", "--from-file", names, "--timeout", "0s"}, "--timeout"},
		{map[string]string{envRetentionToken: env[envRetentionToken]}, []string{"npm/legacy", "--from-file", names}, "not an http or https URL"},
	} {
		_, _, err := run(t, tt.env, append([]string{"admin", "repo", "prefetch"}, tt.args...)...)
		if exitStatus(err) != 2 || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: %v, want exit status 2 with %q", tt.args, err, tt.want)
		}
	}
	if n := len(f.sent()); n != 0 {
		t.Errorf("%d requests, want none", n)
	}
}

// must fails the test on err.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fakeOCIPrefetchAPI is POST …/oci/hub/prefetch as admin repo prefetch sends it: an image is an
// index and a manifest per platform, fetched the first time and held after, but the images of
// busy, whose manifests fail with 503 and a Retry-After until they are let through.
type fakeOCIPrefetchAPI struct {
	mu      sync.Mutex
	held    map[string]bool
	busy    map[string]bool
	batches []api.PrefetchRequest
}

func (f *fakeOCIPrefetchAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos/oci/hub/prefetch" {
		http.Error(w, `{"error": {"code": "NOT_FOUND", "message": "no"}}`, http.StatusNotFound)
		return
	}
	var req api.PrefetchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Names) > 0 || req.AllVersions {
		http.Error(w, `{"error": {"code": "INVALID", "message": "body"}}`, http.StatusBadRequest)
		return
	}
	f.batches = append(f.batches, req)
	var out api.PrefetchAnswer
	for _, n := range req.Images {
		status := "fetched"
		if f.held[n] {
			status = "held"
		}
		out.Results = append(out.Results, proxy.PrefetchResult{Name: n, Kind: proxy.PrefetchIndex, Digest: digest.Digest("sha256:" + strings.Repeat("a", 64)),
			Status: status})
		for _, p := range req.Platforms {
			res := proxy.PrefetchResult{Name: n, Kind: proxy.PrefetchManifest, Platform: p, Digest: digest.Digest("sha256:" + strings.Repeat("b", 64)),
				Status: status}
			if f.busy[n] {
				res.Status, res.HTTPStatus, res.Message, res.RetryAfter = proxy.PrefetchFailed, http.StatusServiceUnavailable, "upstream hub answered 429", 60
			} else if status == "fetched" {
				res.Bytes = 1024
			}
			out.Results = append(out.Results, res)
		}
		if !f.busy[n] {
			f.held[n] = true
		}
	}
	_ = json.NewEncoder(w).Encode(out)
}

// sent returns the batches, one string per batch: the images, then the platforms after a |.
func (f *fakeOCIPrefetchAPI) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, b := range f.batches {
		out = append(out, strings.Join(b.Images, " ")+" | "+strings.Join(b.Platforms, ","))
	}
	f.batches = nil
	return out
}

// TestAdminPrefetchOCI: of an oci repo, --host keeps the references of Docker Hub and strips its
// registry, a line's platforms win over --platform, and the images go in batches of the same
// platforms; a line per result, the bytes and a 503's wait are printed; --failed sends a failed
// image again with the platforms the report gives it (docs/spec/proxy-and-group.md §11.10).
func TestAdminPrefetchOCI(t *testing.T) {
	f := &fakeOCIPrefetchAPI{held: map[string]bool{"nginx:1.27": true}, busy: map[string]bool{"bitnami/redis:7.4": true}}
	env, _, report := newPrefetchRun(t, &fakePrefetchAPI{})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	env[envRetentionURL] = srv.URL
	images := filepath.Join(t.TempDir(), "images")
	must(t, os.WriteFile(images, []byte(`# kubectl get pods -A -o jsonpath='{..image}'
docker.io/library/nginx:1.27
nginx:1.27
registry.k8s.io/pause:3.10   linux/amd64
index.docker.io/bitnami/redis:7.4 linux/arm64
docker.io/library/nginx:1.27
busybox@sha256:`+strings.Repeat("c", 64)+`
`), 0o600))

	out, _, err := run(t, env, "admin", "repo", "prefetch", "oci/hub", "--from-file", images, "--host", "docker.io",
		"--platform", "linux/amd64", "--platform", "linux/arm64", "--batch", "2", "--report", report)
	if exitStatus(err) != 1 {
		t.Fatalf("first run: %v, want exit status 1; out %s", err, out)
	}
	want := []string{"library/nginx:1.27 nginx:1.27 | linux/amd64,linux/arm64", "busybox@sha256:" + strings.Repeat("c", 64) + " | linux/amd64,linux/arm64",
		"bitnami/redis:7.4 | linux/arm64"}
	if d := cmp.Diff(want, f.sent()); d != "" {
		t.Errorf("batches (-want +got)\n%s", d)
	}
	for _, w := range []string{
		"fetched  library/nginx:1.27 manifest linux/amd64 sha256:" + strings.Repeat("b", 64) + " (1024 bytes)\n",
		"held     nginx:1.27 index sha256:" + strings.Repeat("a", 64) + "\n",
		"failed   bitnami/redis:7.4 manifest linux/arm64 sha256:" + strings.Repeat("b", 64) + ": 503 upstream hub answered 429 (retry after 60s)\n",
		"oci/hub: 4 images: 7 fetched, 3 held, 0 shared, 1 failed",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output %q, want %q", out, w)
		}
	}
	rep := readReport(t, report)
	if d := cmp.Diff([]string{"registry.k8s.io/pause:3.10"}, rep.Skipped); d != "" || rep.Names != 4 ||
		strings.Join(rep.Platforms["bitnami/redis:7.4"], ",") != "linux/arm64" {
		t.Errorf("report %+v; skipped (-want +got)\n%s", rep, d)
	}

	f.busy = nil
	out, _, err = run(t, env, "admin", "repo", "prefetch", "oci/hub", "--from-file", report, "--failed", "--host", "docker.io",
		"--platform", "linux/amd64", "--report", report)
	if err != nil || !slices.Equal(f.sent(), []string{"bitnami/redis:7.4 | linux/arm64"}) || !strings.Contains(out, "oci/hub: 1 images: 2 fetched") {
		t.Errorf("--failed: %v, out %s", err, out)
	}
}

// TestAdminPrefetchOCIUsage: the usage errors of an oci repo's run exit 2 before a request.
func TestAdminPrefetchOCIUsage(t *testing.T) {
	f := &fakeOCIPrefetchAPI{held: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	env := map[string]string{envRetentionURL: srv.URL, envRetentionToken: "ljs_pat_secret"} //nolint:gosec // a token of the test
	file := func(name, content string) string {
		path := filepath.Join(dir, name)
		must(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	images, three, badPlatform, other := file("images", "nginx:1.27\n"), file("three", "nginx:1.27 linux/amd64 x\n"),
		file("bad", "nginx:1.27 linux\n"), file("other", "ghcr.io/a/b:1\n")
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"oci/hub", "--from-file", images, "--all-versions"}, "--all-versions: an oci repo"},
		{[]string{"npm/legacy", "--from-file", images, "--platform", "linux/amd64"}, "--platform and --host are for an oci repo"},
		{[]string{"npm/legacy", "--from-file", images, "--host", "docker.io"}, "--platform and --host are for an oci repo"},
		{[]string{"oci/hub", "--from-file", images, "--platform", "linux"}, `--platform: "linux" is not`},
		{[]string{"oci/hub", "--from-file", three}, "line 1: 3 fields"},
		{[]string{"oci/hub", "--from-file", badPlatform}, `line 1: "linux" is not`},
		{[]string{"oci/hub", "--from-file", other, "--host", "docker.io"}, "no reference of docker.io in it; 1 of other registries"},
	} {
		_, _, err := run(t, env, append([]string{"admin", "repo", "prefetch"}, tt.args...)...)
		if exitStatus(err) != 2 || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: %v, want exit status 2 with %q", tt.args, err, tt.want)
		}
	}
	if n := len(f.sent()); n != 0 {
		t.Errorf("%d requests, want none", n)
	}
}

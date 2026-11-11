package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/format"
)

// retentionNow is the server's clock in the tests of admin retention.
var retentionNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// fakeRetentionAPI is the management API as admin retention reads it: npm/internal with the
// packages of versions, two pages of packages, and the batches it was sent.
type fakeRetentionAPI struct {
	versions map[string][]api.VersionEntry // newest first
	status   map[string]string             // the status of a version's delete; "" is deleted
	mu       sync.Mutex
	batches  []api.BatchDeleteRequest
	auths    []string
	queries  []string
}

// listed is a version of one variant, the variant "", published days ago, with dist-tags.
func listed(v string, days int, distTags ...string) api.VersionEntry {
	at := retentionNow.Add(-time.Duration(days) * 24 * time.Hour)
	e := api.VersionEntry{Version: v, PublishedAt: at, LastModified: at, Pointers: []format.Pointer{},
		Variants: []api.VariantEntry{{Tag: v, Digest: "sha256:" + strings.Repeat(v[:1], 64), PublishedAt: at}}}
	for _, t := range distTags {
		e.Pointers = append(e.Pointers, format.Pointer{Kind: "dist-tag", Name: t})
	}
	return e
}

func (f *fakeRetentionAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.queries = append(f.queries, r.URL.RawQuery)
	w.Header().Set("Date", retentionNow.Format(http.TimeFormat))
	const base = "/api/v1/repos/npm/internal/"
	switch rest := strings.TrimPrefix(r.URL.EscapedPath(), base); {
	case r.Method == http.MethodGet && rest == "packages" && r.URL.Query().Get("cursor") == "":
		_ = json.NewEncoder(w).Encode(api.PackageList{Packages: []api.PackageEntry{{Name: "@acme/widgets"}}, Next: "page2"})
	case r.Method == http.MethodGet && rest == "packages":
		_ = json.NewEncoder(w).Encode(api.PackageList{Packages: []api.PackageEntry{{Name: "left-pad"}}})
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "packages/") && strings.HasSuffix(rest, "/versions"):
		name := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(rest, "packages/"), "/versions"), "%2F", "/")
		_ = json.NewEncoder(w).Encode(api.VersionList{Versions: f.versions[name]})
	case r.Method == http.MethodPost && rest == "versions:delete":
		var req api.BatchDeleteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.batches = append(f.batches, req)
		res := api.BatchDeleteResult{Job: req.Job}
		for _, it := range req.Items {
			st := f.status[it.Version]
			if st == "" {
				st = "deleted"
			}
			res.Results = append(res.Results, api.BatchItemResult{Package: it.Package, Version: it.Version, Status: st})
		}
		_ = json.NewEncoder(w).Encode(res)
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]map[string]string{"error": {"code": "NOT_FOUND", "message": "repo npm/" + rest + " does not exist"}})
	}
}

// newRetentionServer serves f and returns the environment of admin retention against it, with a
// token in a file.
func newRetentionServer(t *testing.T, f *fakeRetentionAPI) map[string]string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("ljs_pat_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{envRetentionURL: srv.URL, envRetentionTokenFile: file}
}

// TestAdminRetention: the dry run prints what --keep-last 1 leaves, never a version a dist-tag
// names nor the newest, and exits 2; --apply sends those versions, their digests and the cut-off
// in one job and prints what the server did; --max-age keeps the young; a package glob filters,
// and its literal part is the list's prefix. The token goes as a bearer, from the file.
func TestAdminRetention(t *testing.T) {
	f := &fakeRetentionAPI{versions: map[string][]api.VersionEntry{
		"@acme/widgets": {listed("3.0.0", 1), listed("2.0.0", 50, "beta"), listed("1.0.0", 100), listed("0.9.0", 200)},
		"left-pad":      {listed("1.3.0", 400, "latest"), listed("1.2.0", 500)},
	}, status: map[string]string{"0.9.0": "changed"}}
	env := newRetentionServer(t, f)
	args := []string{"admin", "retention", "--repo", "npm/internal", "--keep-last", "1"}

	out, _, err := run(t, env, args...)
	if !errors.Is(err, exitCode(2)) || exitStatus(err) != 2 {
		t.Fatalf("dry run: err %v, want exit status 2; out %s", err, out)
	}
	for _, want := range []string{"npm/internal: 3 of 6 versions in 2 packages to delete (a dry run",
		"delete @acme/widgets 1.0.0  published 2026-08-03T12:00:00Z  rank 3 > keep-last 1",
		"delete @acme/widgets 0.9.0", "delete left-pad 1.2.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output %q, want %q", out, want)
		}
	}
	for _, kept := range []string{"3.0.0", "2.0.0", "1.3.0"} {
		if strings.Contains(out, " "+kept+" ") {
			t.Errorf("the dry run would delete %s: %s", kept, out)
		}
	}
	if len(f.batches) != 0 || f.auths[0] != "Bearer ljs_pat_secret" {
		t.Fatalf("a dry run sent %+v; Authorization %q", f.batches, f.auths[0])
	}

	wantRetentionApply(t, f, env, append(args, "--apply"))
	wantRetentionJSON(t, f, env)
}

// wantRetentionApply runs args, admin retention --apply against f, and checks the batch it sent
// and what it printed.
func wantRetentionApply(t *testing.T, f *fakeRetentionAPI, env map[string]string, args []string) {
	t.Helper()
	out, _, err := run(t, env, args...)
	if err != nil {
		t.Fatalf("--apply: %v; %s", err, out)
	}
	if len(f.batches) != 1 {
		t.Fatalf("batches %+v", f.batches)
	}
	b := f.batches[0]
	if !strings.HasPrefix(b.Job, "retention-20261111T120000Z-") || b.Cutoff != "2026-11-11T12:00:00Z" || b.DryRun || len(b.Items) != 3 ||
		b.Items[0].Package != "@acme/widgets" || b.Items[0].Version != "1.0.0" || b.Items[0].Digests[0] != "sha256:"+strings.Repeat("1", 64) {
		t.Errorf("batch %+v", b)
	}
	for _, want := range []string{"sent to the server as job " + b.Job, "deleted   @acme/widgets 1.0.0", "changed   @acme/widgets 0.9.0",
		"deleted 2, changed 1, protected 0, young 0, gone 0, error 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("--apply output %q, want %q", out, want)
		}
	}

}

// wantRetentionJSON runs a dry run with --max-age, a glob and --output json against f, and
// checks the report.
func wantRetentionJSON(t *testing.T, f *fakeRetentionAPI, env map[string]string) {
	t.Helper()
	f.batches = nil
	out, _, err := run(t, env, "admin", "retention", "--repo", "npm/internal", "--keep-last", "1", "--max-age", "150d",
		"--package", "@acme/*", "--output", "json")
	if exitStatus(err) != 2 {
		t.Fatalf("json dry run: %v", err)
	}
	var rep retentionReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json: %v: %s", err, out)
	}
	if rep.Packages != 1 || len(rep.Delete) != 1 || rep.Delete[0].Version != "0.9.0" || rep.Delete[0].Rank != 4 ||
		rep.Delete[0].AgeSeconds != 200*24*3600 || !rep.Cutoff.Equal(retentionNow.Add(-150*24*time.Hour)) || !rep.DryRun {
		t.Errorf("report %+v", rep)
	}
	if q := f.queries[len(f.queries)-3]; !strings.Contains(q, "prefix=%40acme%2F") {
		t.Errorf("the packages were listed with %q, want the glob's prefix", q)
	}
}

// TestAdminRetentionNewest: the newest version goes only with --delete-newest; an item the
// server failed is exit status 1; nothing to delete is 0.
func TestAdminRetentionNewest(t *testing.T) {
	f := &fakeRetentionAPI{versions: map[string][]api.VersionEntry{"@acme/widgets": {listed("2.0.0", 300)}}, status: map[string]string{}}
	env := newRetentionServer(t, f)
	f.versions["left-pad"] = nil
	args := []string{"admin", "retention", "--repo", "npm/internal", "--max-age", "30d"}
	if out, _, err := run(t, env, args...); err != nil || !strings.Contains(out, "0 of 1 versions") {
		t.Errorf("the newest alone: %v, %s", err, out)
	}
	f.status["2.0.0"] = "error"
	_, _, err := run(t, env, append(args, "--delete-newest", "--apply")...)
	if err == nil || exitStatus(err) != 1 || !strings.Contains(err.Error(), "failed to delete @acme/widgets 2.0.0") {
		t.Errorf("an item that failed: %v", err)
	}
}

// TestAdminRetentionArgs: what the command refuses before it asks the server.
func TestAdminRetentionArgs(t *testing.T) {
	f := &fakeRetentionAPI{}
	env := newRetentionServer(t, f)
	for _, tt := range []struct {
		env  map[string]string
		args []string
		want string
	}{
		{env, []string{"--repo", "npm/internal"}, "name a rule"},
		{env, []string{"--repo", "npm", "--keep-last", "1"}, "not <format>/<name>"},
		{env, []string{"--repo", "npm/internal", "--keep-last", "-1"}, "negative"},
		{env, []string{"--repo", "npm/internal", "--max-age", "soon"}, "--max-age"},
		{env, []string{"--repo", "npm/internal", "--keep-last", "1", "--package", "["}, "not a glob"},
		{env, []string{"--repo", "npm/internal", "--keep-last", "1", "--output", "yaml"}, "--output"},
		{map[string]string{envRetentionTokenFile: env[envRetentionTokenFile]}, []string{"--repo", "npm/internal", "--keep-last", "1"}, "not an http or https URL"},
		{map[string]string{envRetentionURL: env[envRetentionURL]}, []string{"--repo", "npm/internal", "--keep-last", "1"}, "the token: none"},
		{env, []string{"--repo", "npm/none", "--keep-last", "1"}, "404 Not Found: NOT_FOUND: repo npm/"},
	} {
		_, _, err := run(t, tt.env, append([]string{"admin", "retention"}, tt.args...)...)
		if err == nil || exitStatus(err) != 1 || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: %v, want exit status 1 with %q", tt.args, err, tt.want)
		}
	}
	_, _, err := run(t, map[string]string{envRetentionURL: env[envRetentionURL], envRetentionToken: "ljs_pat_env"}, "admin", "retention", //nolint:gosec // a token of the test
		"--repo", "npm/internal", "--keep-last", "1")
	if err != nil || f.auths[len(f.auths)-1] != "Bearer ljs_pat_env" {
		t.Errorf("the token of %s: %v, %q", envRetentionToken, err, f.auths[len(f.auths)-1])
	}
}

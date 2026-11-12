package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/format"
)

// retentionNow is the server's clock in the tests of admin retention.
var retentionNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// fakeRetentionAPI is the management API as admin retention reads it: npm/internal with the
// packages of versions, two pages of packages, and the batches it was sent. A batch takes
// perItem for each of its versions, as the server deletes them one after another, and the
// batch of index i (from 0) of slow takes slow[i] instead; one of failed answers that status.
// arrived, when set, is called with the index of each batch as it arrives.
type fakeRetentionAPI struct {
	versions map[string][]api.VersionEntry // newest first
	status   map[string]string             // the status of a version's delete; "" is deleted
	perItem  time.Duration
	slow     map[int]time.Duration
	failed   map[int]int
	arrived  func(int)
	mu       sync.Mutex
	batches  []api.BatchDeleteRequest
	auths    []string
	queries  []string
}

// sleep waits d, or until the request's client has gone away.
func sleep(r *http.Request, d time.Duration) {
	select {
	case <-time.After(d):
	case <-r.Context().Done():
	}
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
		f.deleteBatch(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]map[string]string{"error": {"code": "NOT_FOUND", "message": "repo npm/" + rest + " does not exist"}})
	}
}

// deleteBatch answers a batch, with f.mu held: after perItem for each version, or slow, the
// status of failed, or a result per version.
func (f *fakeRetentionAPI) deleteBatch(w http.ResponseWriter, r *http.Request) {
	var req api.BatchDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.batches = append(f.batches, req)
	n := len(f.batches) - 1
	if f.arrived != nil {
		f.arrived(n)
	}
	d, ok := f.slow[n]
	if !ok {
		d = time.Duration(len(req.Items)) * f.perItem
	}
	sleep(r, d)
	if code := f.failed[n]; code != 0 {
		w.WriteHeader(code)
		return
	}
	res := api.BatchDeleteResult{Job: req.Job}
	for _, it := range req.Items {
		st := f.status[it.Version]
		if st == "" {
			st = "deleted"
		}
		res.Results = append(res.Results, api.BatchItemResult{Package: it.Package, Version: it.Version, Status: st})
	}
	_ = json.NewEncoder(w).Encode(res)
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

// manyVersions returns n versions 1.0.<i> of one variant, the newest first, a day apart.
func manyVersions(n int) []api.VersionEntry {
	var vs []api.VersionEntry
	for i := n; i > 0; i-- {
		vs = append(vs, listed(fmt.Sprintf("1.0.%d", i), n-i+1))
	}
	return vs
}

// applyRetention runs admin retention --keep-last 1 --apply on f as the command does, within
// ctx, with the bound of a request that lists shortened to list and that of each version of a
// batch to item, and returns what it printed, as JSON when asJSON.
func applyRetention(ctx context.Context, t *testing.T, f *fakeRetentionAPI, list, item time.Duration, asJSON bool) (string, error) {
	t.Helper()
	env := newRetentionServer(t, f)
	c, err := newRetentionClient(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, "", "")
	if err != nil {
		t.Fatal(err)
	}
	c.timeout, c.itemTimeout = list, item
	one := 1
	var out bytes.Buffer
	r := &retention{client: c, format: "npm", name: "internal", rules: retentionRules{KeepLast: &one}, out: &out, json: asJSON}
	err = r.run(ctx, true)
	return out.String(), err
}

// TestAdminRetentionSlowBatches checks --apply against a server that takes 30 ms for each
// version (#712): a batch of 10 takes 300 ms, three times the bound of a request that lists,
// and is answered, for a batch is bounded per version; the 24 versions go in batches of 10, 10
// and 4, and the results of every batch are printed.
func TestAdminRetentionSlowBatches(t *testing.T) {
	f := &fakeRetentionAPI{versions: map[string][]api.VersionEntry{"@acme/widgets": manyVersions(25)}, status: map[string]string{},
		perItem: 30 * time.Millisecond}
	out, err := applyRetention(t.Context(), t, f, 100*time.Millisecond, 100*time.Millisecond, false)
	if err != nil {
		t.Fatalf("--apply: %v; %s", err, out)
	}
	var sizes []int
	for _, b := range f.batches {
		sizes = append(sizes, len(b.Items))
	}
	if !slices.Equal(sizes, []int{10, 10, 4}) {
		t.Errorf("batches of %v versions, want 10, 10 and 4", sizes)
	}
	if n := strings.Count(out, "\ndeleted   @acme/widgets 1.0."); n != 24 || !strings.Contains(out, "deleted 24, changed 0") {
		t.Errorf("%d results printed, want 24 and the counts:\n%s", n, out)
	}
}

// TestAdminRetentionStopped checks an --apply that stops at its second batch (#712): the server
// fails it, it has no answer within the bound of its versions, or the run is interrupted (SIGINT,
// SIGTERM and SIGHUP end its context, TestRetentionContext). The results of the first batch are printed with their counts,
// or are in the JSON document with the error; the error names the batch and what was not sent;
// the exit status is 1.
func TestAdminRetentionStopped(t *testing.T) {
	for _, tt := range []struct {
		name   string
		fake   func(f *fakeRetentionAPI, interrupt func())
		want   string
		asJSON bool
	}{
		{"the server fails the batch", func(f *fakeRetentionAPI, _ func()) { f.failed = map[int]int{1: http.StatusBadGateway} },
			"502 Bad Gateway", false},
		{"no answer within the bound", func(f *fakeRetentionAPI, _ func()) { f.slow = map[int]time.Duration{1: time.Minute} },
			"context deadline exceeded", false},
		{"interrupted", func(f *fakeRetentionAPI, interrupt func()) {
			f.slow = map[int]time.Duration{1: time.Minute}
			f.arrived = func(n int) {
				if n == 1 {
					interrupt()
				}
			}
		}, "context canceled", false},
		{"JSON", func(f *fakeRetentionAPI, _ func()) { f.failed = map[int]int{1: http.StatusBadGateway} }, "502 Bad Gateway", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, interrupt := context.WithCancel(t.Context())
			defer interrupt()
			f := &fakeRetentionAPI{versions: map[string][]api.VersionEntry{"@acme/widgets": manyVersions(25)}, status: map[string]string{}}
			tt.fake(f, interrupt)
			out, err := applyRetention(ctx, t, f, time.Minute, 10*time.Millisecond, tt.asJSON)
			if err == nil || exitStatus(err) != 1 {
				t.Fatalf("--apply: %v, want exit status 1; %s", err, out)
			}
			for _, want := range []string{"the batch of 10 versions from @acme/widgets 1.0.14", tt.want, "the 4 versions after it were not sent"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q, want %q", err, want)
				}
			}
			// Under the lock: the handler of the second batch may not have seen the client go yet.
			f.mu.Lock()
			sent := len(f.batches)
			f.mu.Unlock()
			if sent != 2 {
				t.Errorf("%d batches sent, want 2", sent)
			}
			if tt.asJSON {
				var rep retentionReport
				if err := json.Unmarshal([]byte(out), &rep); err != nil || len(rep.Results) != 10 || rep.Error == "" {
					t.Errorf("report %+v, %v; want the 10 results of the first batch and the error", rep, err)
				}
				return
			}
			if n := strings.Count(out, "\ndeleted   @acme/widgets 1.0."); n != 10 || !strings.Contains(out, "deleted 10, changed 0") {
				t.Errorf("%d results printed, want the 10 of the first batch and the counts:\n%s", n, out)
			}
		})
	}
}

// TestRetentionContext checks that each of SIGINT, SIGTERM and SIGHUP (a closed terminal, #763)
// ends the context of a retention run instead of killing the process.
func TestRetentionContext(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, stop := retentionContext(t.Context())
			defer stop()
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("%v did not end the context", sig)
			}
		})
	}
}

// TestRetentionBatches checks the batches of --apply: 10 versions at most, fewer when their
// JSON passes 48 KiB, and a version larger than that alone.
func TestRetentionBatches(t *testing.T) {
	item := func(variants int) api.BatchDeleteItem {
		it := api.BatchDeleteItem{Package: "numpy", Version: "2.3.0"}
		for range variants {
			it.Digests = append(it.Digests, "sha256:"+strings.Repeat("a", 64))
		}
		return it
	}
	for _, tt := range []struct {
		name  string
		items []api.BatchDeleteItem
		want  []int
	}{
		{"none", nil, nil},
		{"25 versions of one variant", slices.Repeat([]api.BatchDeleteItem{item(1)}, 25), []int{10, 10, 5}},
		{"5 versions of 300 variants", slices.Repeat([]api.BatchDeleteItem{item(300)}, 5), []int{2, 2, 1}},
		{"a version past the bound", []api.BatchDeleteItem{item(1), item(700), item(1)}, []int{1, 1, 1}},
	} {
		var got []int
		for _, b := range retentionBatches(tt.items) {
			got = append(got, len(b))
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: batches of %v, want %v", tt.name, got, tt.want)
		}
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

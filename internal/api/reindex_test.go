package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
)

// fakeReindexer records the requests it gets and starts runs in the index, as the worker does,
// without running them.
type fakeReindexer struct {
	st   index.Store
	reqs []reindex.Request
}

func (f *fakeReindexer) Start(ctx context.Context, req reindex.Request) (*index.ReindexRun, error) {
	f.reqs = append(f.reqs, req)
	run := index.ReindexRun{Source: req.Source, Principal: req.Principal, Replica: "api-test 1",
		Options: index.ReindexOptions{Prune: req.Prune, Verify: req.Verify}, StartedAt: testNow}
	if req.Repo != nil {
		run.RepoID = &req.Repo.ID
	}
	if err := f.st.StartReindexRun(ctx, &run, testNow.Add(-time.Minute)); err != nil {
		return nil, err
	}
	return &run, nil
}

// TestReindexEndpoints checks POST and GET …/reindex: 202 with the run and the request the
// Reindexer got, the optional body, 409 naming the run that holds the lock, the latest run with
// the repo's part of the report, a run of every repo, and the 404s.
func TestReindexEndpoints(t *testing.T) {
	ctx := t.Context()
	fake := &fakeReindexer{}
	f := newFixture(t, Options{Reindex: fake})
	fake.st = f.st
	repo := f.create(t, "raw", "assets")
	other := f.create(t, "raw", "other")
	admin := auth.Principal{Kind: auth.KindUser, Name: "admin"}

	wantError(t, f.do(http.MethodGet, "/api/v1/repos/raw/assets/reindex", "", admin), http.StatusNotFound, codeNotFound,
		"no reindex has covered raw/assets")
	wantError(t, f.do(http.MethodPost, "/api/v1/repos/raw/missing/reindex", "", admin), http.StatusNotFound, codeNotFound,
		"repo raw/missing does not exist")
	wantError(t, f.do(http.MethodPost, "/api/v1/repos/raw/assets/reindex", `{"prune": true, "force": true}`, admin),
		http.StatusBadRequest, codeInvalid)

	var started ReindexRun
	decodeJSON(t, f.do(http.MethodPost, "/api/v1/repos/raw/assets/reindex", "", admin), http.StatusAccepted, &started)
	same(t, "the run started", ReindexRun{ID: started.ID, State: index.ReindexRunning, Source: SourceAPI, Principal: "user:admin",
		Replica: "api-test 1", StartedAt: testNow, HeartbeatAt: testNow}, started)
	same(t, "the request", []reindex.Request{{Repo: repo, Source: SourceAPI, Principal: "user:admin"}}, fake.reqs)

	wantError(t, f.do(http.MethodPost, "/api/v1/repos/raw/assets/reindex", `{"prune": true}`, admin), http.StatusConflict,
		codeConflict, "a reindex is already running on api-test 1", "(run 1)")
	var status ReindexRun
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/raw/assets/reindex", "", admin), http.StatusOK, &status)
	same(t, "GET while it runs", started, status)

	finished := testNow.Add(time.Minute)
	report := reindex.Report{Repos: []reindex.RepoReport{
		{Repo: "raw/assets", Manifests: 3, Added: 3, Foreign: []reindex.Finding{}, OrphanReferrers: []reindex.Finding{},
			Drift: []reindex.Drift{}, Errors: []reindex.Finding{}},
	}}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.st.GetReindexRun(ctx, started.ID)
	if err != nil {
		t.Fatal(err)
	}
	run.State, run.FinishedAt, run.Report = index.ReindexDone, &finished, raw
	if err := f.st.FinishReindexRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/raw/assets/reindex", "", admin), http.StatusOK, &status)
	want := started
	want.State, want.FinishedAt, want.Report = index.ReindexDone, &finished, &report.Repos[0]
	same(t, "GET after the run", want, status)

	decodeJSON(t, f.do(http.MethodPost, "/api/v1/repos/raw/other/reindex", `{"prune": true, "verify": true}`, admin),
		http.StatusAccepted, &started)
	if !started.Prune || !started.Verify || len(fake.reqs) != 3 || fake.reqs[2].Repo.ID != other.ID { // the 409 asked too
		t.Errorf("the body: run %+v, requests %+v", started, fake.reqs)
	}

	all := index.ReindexRun{Source: "cli", Principal: "anonymous", Replica: "cli 2", StartedAt: testNow.Add(2 * time.Minute)}
	if err := f.st.StartReindexRun(ctx, &all, testNow.Add(-time.Hour)); err == nil {
		t.Fatal("a run of every repo while raw/other runs started")
	}
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/raw/assets/reindex", "", admin), http.StatusOK, &status)
	if status.ID != want.ID {
		t.Errorf("raw/assets: the latest run is %d, want %d", status.ID, want.ID)
	}
}

// TestReindexChunkedBody checks the optional body of POST …/reindex sent chunked, without a
// Content-Length: an empty one is no body, the defaults; one with the object is read.
func TestReindexChunkedBody(t *testing.T) {
	fake := &fakeReindexer{}
	f := newFixture(t, Options{Reindex: fake})
	fake.st = f.st
	f.create(t, "raw", "assets")
	f.create(t, "raw", "other")
	admin := auth.Principal{Kind: auth.KindUser, Name: "admin"}
	for _, tt := range []struct {
		repo, body string
		prune      bool
	}{
		{repo: "assets", body: ""},
		{repo: "other", body: `{"prune": true}`, prune: true},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/repos/raw/"+tt.repo+"/reindex", io.NopCloser(strings.NewReader(tt.body)))
		r.ContentLength, r.TransferEncoding = -1, []string{"chunked"}
		r = r.WithContext(auth.WithPrincipal(r.Context(), f.asStored(admin)))
		rr := httptest.NewRecorder()
		f.h.ServeHTTP(rr, r)
		var run ReindexRun
		decodeJSON(t, rr, http.StatusAccepted, &run)
		if run.Prune != tt.prune {
			t.Errorf("raw/%s, chunked body %q: prune %v, want %v", tt.repo, tt.body, run.Prune, tt.prune)
		}
	}
}

// TestReindexAllRepos checks that GET …/reindex answers a run of every repo that covered the
// repo, and that a repo it did not reach has no report.
func TestReindexAllRepos(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t, Options{Reindex: &fakeReindexer{}})
	f.create(t, "raw", "assets")
	f.create(t, "raw", "late")
	admin := auth.Principal{Kind: auth.KindUser, Name: "admin"}
	run := index.ReindexRun{Source: "cli", Principal: "anonymous", Replica: "cli 2", StartedAt: testNow}
	if err := f.st.StartReindexRun(ctx, &run, testNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	part := reindex.RepoReport{Repo: "raw/assets", Unchanged: 2, Foreign: []reindex.Finding{}, OrphanReferrers: []reindex.Finding{},
		Drift: []reindex.Drift{}, Errors: []reindex.Finding{}}
	raw, err := json.Marshal(reindex.Report{Repos: []reindex.RepoReport{part}})
	if err != nil {
		t.Fatal(err)
	}
	run.State, run.Report, run.Error = index.ReindexFailed, raw, "context canceled"
	if err := f.st.FinishReindexRun(ctx, &run); err != nil {
		t.Fatal(err)
	}
	var got ReindexRun
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/raw/assets/reindex", "", admin), http.StatusOK, &got)
	if !got.AllRepos || got.State != index.ReindexFailed || got.Error != "context canceled" || got.Report == nil ||
		!cmp.Equal(*got.Report, part) {
		t.Errorf("raw/assets: %+v", got)
	}
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/raw/late/reindex", "", admin), http.StatusOK, &got)
	if !got.AllRepos || got.Report != nil {
		t.Errorf("raw/late, not reached: %+v", got)
	}
}

// same fails the test when got differs from want.
func same(t *testing.T, what string, want, got any) {
	t.Helper()
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("%s: (-want +got)\n%s", what, d)
	}
}

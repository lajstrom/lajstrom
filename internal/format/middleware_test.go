package format_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
)

// testRepos are fake/scratch and a repo of another format with a name fake does not have.
func testRepos() []index.Repo {
	return []index.Repo{
		{ID: 1, Format: "fake", Name: "scratch"},
		{ID: 2, Format: "pypi", Name: "other"},
	}
}

// newMux mounts on a new ServeMux, through the Router of plugin fake: GET /{repo}/twice, which
// reads RepoFrom twice, and GET /static, which has no {repo}.
func newMux(t *testing.T, deps format.Deps) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	r := format.NewRouter(mux, "fake", deps)
	r.HandleFunc("GET /{repo}/twice", func(w http.ResponseWriter, req *http.Request) {
		first, ok1 := format.RepoFrom(req.Context())
		second, ok2 := format.RepoFrom(req.Context())
		if !ok1 || !ok2 || first.ID != second.ID {
			t.Errorf("RepoFrom = %v %v, %v %v", first, ok1, second, ok2)
		}
		_, _ = io.WriteString(w, first.Format+"/"+first.Name)
	})
	r.Handle("GET /static", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if repo, ok := format.RepoFrom(req.Context()); ok {
			t.Errorf("RepoFrom = %v without {repo}", repo)
		}
		_, _ = io.WriteString(w, "static")
	}))
	return mux
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

func TestRepoMiddleware(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		status  int
		body    string
		lookups int64
	}{
		{name: "repo", path: "/fake/scratch/twice", status: 200, body: "fake/scratch", lookups: 1},
		{name: "missing repo", path: "/fake/missing/twice", status: 404, body: "index: not found: repository fake/missing\n", lookups: 1},
		{name: "repo of another format", path: "/fake/other/twice", status: 404, body: "index: not found: repository fake/other\n", lookups: 1},
		{name: "no {repo} no lookup", path: "/fake/static", status: 200, body: "static"},
		{name: "outside the prefix", path: "/scratch/twice", status: 404, body: "404 page not found\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := &formattest.Index{Repos: testRepos()}
			rr := get(newMux(t, format.Deps{Index: idx}), tt.path)
			if rr.Code != tt.status || rr.Body.String() != tt.body {
				t.Errorf("GET %s = %d %q, want %d %q", tt.path, rr.Code, rr.Body, tt.status, tt.body)
			}
			if n := idx.GetRepoCalls.Load(); n != tt.lookups {
				t.Errorf("GetRepo calls = %d, want %d", n, tt.lookups)
			}
		})
	}
}

func TestRepoMiddlewareLooksUpEveryRequest(t *testing.T) {
	idx := &formattest.Index{Repos: testRepos()}
	mux := newMux(t, format.Deps{Index: idx})
	for i := int64(1); i <= 2; i++ {
		if rr := get(mux, "/fake/scratch/twice"); rr.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, rr.Code)
		}
		if n := idx.GetRepoCalls.Load(); n != i {
			t.Errorf("after request %d: GetRepo calls = %d, want %d", i, n, i)
		}
	}
}

// otherFormat is a store that answers GetRepo with a repo of format pypi, whatever is asked.
type otherFormat struct{ index.Store }

func (otherFormat) GetRepo(_ context.Context, _, name string) (*index.Repo, error) {
	return &index.Repo{ID: 7, Format: "pypi", Name: name}, nil
}

func TestRepoMiddlewareChecksFormat(t *testing.T) {
	rr := get(newMux(t, format.Deps{Index: otherFormat{}}), "/fake/scratch/twice")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a repo of another format", rr.Code)
	}
}

func TestRepoMiddlewareLookupError(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	idx := &formattest.Index{Repos: testRepos(), GetRepoErr: errors.New("index: database is unreachable")}
	mux := newMux(t, format.Deps{Index: idx, Log: log})

	req := httptest.NewRequest(http.MethodGet, "/fake/scratch/twice", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpodW50ZXIy")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError || rr.Body.String() != "Internal Server Error\n" {
		t.Errorf("response = %d %q, want 500 without the cause", rr.Code, rr.Body)
	}
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("want one JSON log line: %v\n%s", err, &buf)
	}
	want := map[string]any{"level": "ERROR", "msg": "repo lookup failed", "repo": "scratch", "error": "index: database is unreachable"}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("log %s = %v, want %v", k, line[k], v)
		}
	}
	for _, secret := range []string{"Authorization", "dXNlcjpodW50ZXIy"} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("log contains %q: %s", secret, &buf)
		}
	}
}

func TestRepoContext(t *testing.T) {
	if repo, ok := format.RepoFrom(context.Background()); ok {
		t.Errorf("RepoFrom(empty context) = %v, true", repo)
	}
	want := index.Repo{ID: 3, Format: "fake", Name: "scratch"}
	got, ok := format.RepoFrom(format.WithRepo(context.Background(), want))
	if !ok || got.ID != want.ID || got.Format != want.Format || got.Name != want.Name {
		t.Errorf("RepoFrom(WithRepo(%v)) = %v, %v", want, got, ok)
	}
}

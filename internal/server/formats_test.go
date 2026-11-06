package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
)

// newFakeIndex returns an index with fake/scratch and pypi/other: a repo of the fake format and
// one of another format.
func newFakeIndex() *formattest.Index {
	return &formattest.Index{Repos: []index.Repo{
		{ID: 1, Format: "fake", Name: "scratch"},
		{ID: 2, Format: "pypi", Name: "other"},
	}}
}

func TestFormatRoutes(t *testing.T) {
	const notFound = "404 page not found\n" // the ServeMux's own 404: no plugin handled the path
	tests := []struct {
		name      string
		method    string
		path      string
		status    int
		body      string // exact body; "" skips the check
		lookups   int64  // GetRepo calls
		challenge bool   // WWW-Authenticate: Basic realm="lajstrom"
	}{
		{name: "repo", path: "/fake/scratch/ping", status: 200, body: "pong fake/scratch\n", lookups: 1},
		{name: "repo HEAD", method: http.MethodHead, path: "/fake/scratch/ping", status: 200, lookups: 1},
		{name: "missing repo", path: "/fake/missing/ping", status: 404, body: "index: not found: repository fake/missing\n", lookups: 1},
		{name: "repo of another format", path: "/fake/other/ping", status: 404, body: "index: not found: repository fake/other\n", lookups: 1},
		{name: "format root", path: "/fake/ping", status: 200, body: "pong\n"},
		{name: "api not shadowed", path: "/api/v1/repos", status: 404, body: notFound},
		{name: "v2 not shadowed", path: "/v2/", status: 404, body: notFound},
		{name: "system path not shadowed", path: "/-/metrics", status: 404, body: notFound},
		{name: "system endpoint", path: "/-/healthz", status: 200, body: "ok\n"},
		{name: "unknown format", path: "/pypi/other/ping", status: 404, body: notFound},
		{name: "wrong method", method: http.MethodPost, path: "/fake/scratch/ping", status: 405},
		{name: "fail invalid", path: "/fake/scratch/fail/invalid", status: 400, body: "formattest: fail invalid: format: invalid\n", lookups: 1},
		{name: "fail unauthenticated", path: "/fake/scratch/fail/unauthenticated", status: 401, lookups: 1, challenge: true},
		{name: "fail forbidden", path: "/fake/scratch/fail/forbidden", status: 403, lookups: 1},
		{name: "fail notfound", path: "/fake/scratch/fail/notfound", status: 404, lookups: 1},
		{name: "fail conflict", path: "/fake/scratch/fail/conflict", status: 409, lookups: 1},
		{name: "fail unsupported", path: "/fake/scratch/fail/unsupported", status: 501, body: "Not Implemented\n", lookups: 1},
		{name: "fail unauthorized", path: "/fake/scratch/fail/unauthorized", status: 502, body: "Bad Gateway\n", lookups: 1},
		{name: "fail other", path: "/fake/scratch/fail/other", status: 500, body: "Internal Server Error\n", lookups: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := newFakeIndex()
			s, _ := newTestServer(t, Options{Plugins: []format.Plugin{&formattest.Fake{}}, Deps: format.Deps{Index: idx}})
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, httptest.NewRequest(method, tt.path, nil))
			if rr.Code != tt.status {
				t.Errorf("status = %d, want %d", rr.Code, tt.status)
			}
			if tt.body != "" && rr.Body.String() != tt.body {
				t.Errorf("body = %q, want %q", rr.Body, tt.body)
			}
			if n := idx.GetRepoCalls.Load(); n != tt.lookups {
				t.Errorf("GetRepo calls = %d, want %d", n, tt.lookups)
			}
			want := ""
			if tt.challenge {
				want = `Basic realm="lajstrom"`
			}
			if got := rr.Header().Get("WWW-Authenticate"); got != want {
				t.Errorf("WWW-Authenticate = %q, want %q", got, want)
			}
		})
	}
}

func TestFormatMountedLog(t *testing.T) {
	_, buf := newTestServer(t, Options{Plugins: []format.Plugin{&formattest.Fake{}}, Deps: format.Deps{Index: newFakeIndex()}})
	l := findLog(t, buf, "format mounted")
	if l["format"] != "fake" || l["artifactType"] != "application/vnd.lajstrom.fake.v1" {
		t.Errorf("log line = %v", l)
	}
}

// The plugin's logger carries the format attribute once, and a failed repo lookup logs neither
// the request's headers nor answers the cause.
func TestFormatLookupErrorLog(t *testing.T) {
	idx := newFakeIndex()
	idx.GetRepoErr = errors.New("index: database is unreachable")
	s, buf := newTestServer(t, Options{Plugins: []format.Plugin{&formattest.Fake{}}, Deps: format.Deps{Index: idx}})
	req := httptest.NewRequest(http.MethodGet, "/fake/scratch/ping", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpodW50ZXIy")
	req.Header.Set("X-Request-Id", "req-lookup")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError || rr.Body.String() != "Internal Server Error\n" {
		t.Errorf("response = %d %q, want 500 without the cause", rr.Code, rr.Body)
	}
	l := findLog(t, buf, "repo lookup failed")
	want := map[string]any{"level": "ERROR", "format": "fake", "repo": "scratch", "error": "index: database is unreachable"}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("log %s = %v, want %v", k, l[k], v)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Count(line, `"format":`) > 1 {
			t.Errorf("format attribute repeated: %s", line)
		}
	}
	for _, secret := range []string{"Authorization", "dXNlcjpodW50ZXIy"} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("log contains %q:\n%s", secret, buf)
		}
	}
}

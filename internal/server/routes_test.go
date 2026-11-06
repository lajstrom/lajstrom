package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/version"
)

func TestSystemEndpoints(t *testing.T) {
	notReady := func(context.Context) error { return errors.New("index: database is unreachable") }
	ready := func(context.Context) error { return nil }
	tests := []struct {
		name     string
		method   string
		path     string
		ready    func(context.Context) error
		status   int
		ctype    string
		body     string // exact body; "" skips the check
		contains string // substring of the body; "" skips the check
	}{
		{name: "healthz", path: "/-/healthz", status: 200, ctype: "text/plain; charset=utf-8", body: "ok\n"},
		{name: "healthz HEAD", method: http.MethodHead, path: "/-/healthz", status: 200},
		{name: "readyz without check", path: "/-/readyz", status: 200, body: "ok\n"},
		{name: "readyz ready", path: "/-/readyz", ready: ready, status: 200, body: "ok\n"},
		{name: "readyz not ready", path: "/-/readyz", ready: notReady, status: 503, ctype: "text/plain; charset=utf-8", body: "index: database is unreachable\n"},
		{name: "version", path: "/-/version", status: 200, ctype: "application/json", contains: `"version":`},
		{name: "unknown system path", path: "/-/metrics", status: 404},
		{name: "unknown path", path: "/raw/scratch/file.txt", status: 404},
		{name: "wrong method", method: http.MethodPost, path: "/-/healthz", status: 405},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestServer(t, Options{Ready: tt.ready})
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, httptest.NewRequest(method, tt.path, nil))
			if rr.Code != tt.status {
				t.Errorf("status = %d, want %d", rr.Code, tt.status)
			}
			if ct := rr.Header().Get("Content-Type"); tt.ctype != "" && ct != tt.ctype {
				t.Errorf("Content-Type = %q, want %q", ct, tt.ctype)
			}
			if tt.body != "" && rr.Body.String() != tt.body {
				t.Errorf("body = %q, want %q", rr.Body, tt.body)
			}
			if tt.contains != "" && !strings.Contains(rr.Body.String(), tt.contains) {
				t.Errorf("body = %q, want it to contain %q", rr.Body, tt.contains)
			}
		})
	}
}

func TestVersionJSON(t *testing.T) {
	saved := [3]string{version.Version, version.Commit, version.Date}
	t.Cleanup(func() { version.Version, version.Commit, version.Date = saved[0], saved[1], saved[2] })
	version.Version, version.Commit, version.Date = "v0.1.0", "28bf6ca", "2026-11-06T12:00:00Z"

	s, _ := newTestServer(t, Options{})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/-/version", nil))

	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %q: %v", rr.Body, err)
	}
	want := map[string]string{"version": "v0.1.0", "commit": "28bf6ca", "date": "2026-11-06T12:00:00Z"}
	if len(got) != len(want) {
		t.Errorf("fields = %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAPIMount checks Options.API: mounted at /api/, it gets every path below it, behind the
// middleware (the request id is set); nil leaves /api/ to the ServeMux's own plain-text 404.
func TestAPIMount(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Api-Saw", r.Method+" "+r.URL.Path+" "+RequestID(r.Context()))
		_, _ = io.WriteString(w, "api\n")
	})
	tests := []struct {
		name   string
		api    http.Handler
		method string
		path   string
		status int
		body   string // exact body
		saw    string // X-Api-Saw: what the API handler saw; "" when it was not called
		ctype  string // "" skips the check
	}{
		{name: "repos", api: api, method: http.MethodGet, path: "/api/v1/repos", status: 200, body: "api\n", saw: "GET /api/v1/repos req-1"},
		{name: "any method and path", api: api, method: http.MethodPut, path: "/api/v2/x", status: 200, body: "api\n", saw: "PUT /api/v2/x req-1"},
		{name: "system endpoints stay", api: api, method: http.MethodGet, path: "/-/healthz", status: 200, body: "ok\n"},
		{name: "no API", method: http.MethodGet, path: "/api/v1/repos", status: 404, body: "404 page not found\n", ctype: "text/plain; charset=utf-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestServer(t, Options{API: tt.api})
			r := httptest.NewRequest(tt.method, tt.path, nil)
			r.Header.Set(requestIDHeader, "req-1")
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, r)
			if rr.Code != tt.status {
				t.Errorf("status = %d, want %d", rr.Code, tt.status)
			}
			if rr.Body.String() != tt.body {
				t.Errorf("body = %q, want %q", rr.Body, tt.body)
			}
			if saw := rr.Header().Get("X-Api-Saw"); saw != tt.saw {
				t.Errorf("the API saw %q, want %q", saw, tt.saw)
			}
			if ct := rr.Header().Get("Content-Type"); tt.ctype != "" && ct != tt.ctype {
				t.Errorf("Content-Type = %q, want %q", ct, tt.ctype)
			}
		})
	}
}

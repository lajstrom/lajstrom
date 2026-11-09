package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCrossOrigin(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		path     string
		header   map[string]string
		refused  bool
		wantBody string // a part of the refusal's body
	}{
		{name: "a form of another site to the API", method: http.MethodPost, path: "/api/v1/users",
			header: map[string]string{"Sec-Fetch-Site": "cross-site", "Content-Type": "text/plain"}, refused: true, wantBody: `"code":"FORBIDDEN"`},
		{name: "another origin by the Origin header", method: http.MethodPost, path: "/api/v1/users",
			header: map[string]string{"Origin": "https://evil.example"}, refused: true, wantBody: `"code":"FORBIDDEN"`},
		{name: "an upload on /v2 from another site", method: http.MethodPost, path: "/v2/oci/images/app/blobs/uploads/",
			header: map[string]string{"Sec-Fetch-Site": "same-site"}, refused: true, wantBody: `"code":"DENIED"`},
		{name: "a frontend upload from another site", method: http.MethodPost, path: "/test/upload",
			header: map[string]string{"Sec-Fetch-Site": "cross-site"}, refused: true, wantBody: "cross-origin request refused"},
		{name: "a DELETE from another site", method: http.MethodDelete, path: "/api/v1/users/alice",
			header: map[string]string{"Sec-Fetch-Site": "cross-site"}, refused: true, wantBody: `"code":"FORBIDDEN"`},
		{name: "the same origin", method: http.MethodPost, path: "/api/v1/users",
			header: map[string]string{"Sec-Fetch-Site": "same-origin"}},
		{name: "the same host by the Origin header", method: http.MethodPost, path: "/api/v1/users",
			header: map[string]string{"Origin": "http://example.com"}},
		{name: "a client that is not a browser", method: http.MethodPost, path: "/api/v1/users"},
		{name: "a read from another site", method: http.MethodGet, path: "/api/v1/users",
			header: map[string]string{"Sec-Fetch-Site": "cross-site"}},
	}
	var reached bool
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	})
	s, _ := newTestServer(t, Options{API: h, Facade: h})
	s.mux.Handle("POST /test/upload", h)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(`{"name":"evil","admin":true}`))
			for k, v := range tt.header {
				req.Header.Set(k, v)
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			switch {
			case tt.refused && (rr.Code != http.StatusForbidden || reached):
				t.Fatalf("status %d, handler reached %v; want 403 before the handler", rr.Code, reached)
			case tt.refused && !strings.Contains(rr.Body.String(), tt.wantBody):
				t.Errorf("body %q, want it to contain %q", rr.Body, tt.wantBody)
			case !tt.refused && (rr.Code != http.StatusNoContent || !reached):
				t.Fatalf("status %d, handler reached %v; want the handler's 204", rr.Code, reached)
			}
			checkSecurityHeaders(t, rr.Header())
		})
	}
}

// wantSecurityHeaders are the headers of every answer of the main listener (#63).
var wantSecurityHeaders = map[string]string{
	"X-Content-Type-Options":  "nosniff",
	"Content-Security-Policy": "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
	"Referrer-Policy":         "no-referrer",
}

// checkSecurityHeaders fails the test unless h has the security headers and no Server header.
func checkSecurityHeaders(t *testing.T, h http.Header) {
	t.Helper()
	for k, want := range wantSecurityHeaders {
		if got := h.Values(k); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want [%q]", k, got, want)
		}
	}
	if v := h.Values("Server"); len(v) != 0 {
		t.Errorf("Server = %q, want none", v)
	}
}

func TestSecurityHeaders(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"FORBIDDEN","message":"credentials are needed"}}`)
	})
	facade := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "blob")
	})
	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{"system endpoint, plain text", http.MethodGet, "/-/healthz", http.StatusOK},
		{"system endpoint, JSON", http.MethodGet, "/-/version", http.StatusOK},
		{"management API error", http.MethodGet, "/api/v1/whoami", http.StatusUnauthorized},
		{"facade blob", http.MethodGet, "/v2/oci/images/app/blobs/sha256:00", http.StatusOK},
		{"HEAD", http.MethodHead, "/-/healthz", http.StatusOK},
		{"a file a writer uploaded as HTML", http.MethodGet, "/test/html", http.StatusOK},
		{"ServeMux 404", http.MethodGet, "/nothing/here", http.StatusNotFound},
		{"ServeMux redirect of an unclean path, an HTML body", http.MethodGet, "/raw/../-/healthz", http.StatusTemporaryRedirect},
		{"panic: the 500 of recovery", http.MethodGet, "/test/panic", http.StatusInternalServerError},
	}
	s, _ := newTestServer(t, Options{API: api, Facade: facade})
	s.mux.HandleFunc("GET /test/html", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<script>alert(1)</script>")
	})
	s.mux.HandleFunc("GET /test/panic", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src *")
		panic("boom")
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, httptest.NewRequest(tt.method, tt.path, nil))
			if rr.Code != tt.status {
				t.Fatalf("status %d, want %d", rr.Code, tt.status)
			}
			checkSecurityHeaders(t, rr.Header())
		})
	}
}

// TestSecurityHeadersOnTheWire checks the answer as net/http writes it to a connection: the
// security headers are there, and there is no Server header naming a product or a version.
func TestSecurityHeadersOnTheWire(t *testing.T) {
	s, _ := newTestServer(t, Options{})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	for _, path := range []string{"/-/healthz", "/-/version", "/nothing"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		t.Run(path, func(t *testing.T) { checkSecurityHeaders(t, resp.Header) })
	}
}

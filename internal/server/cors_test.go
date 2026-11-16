package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const uiOrigin = "https://ui.example.com"

// corsServer returns a server whose API answers 204, or 404 to OPTIONS as the management API
// does, with the origins of server.corsOrigins; it records whether the API was reached.
func corsServer(t *testing.T, origins ...string) (*Server, *bool) {
	t.Helper()
	var reached bool
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	s, _ := newTestServer(t, Options{API: api, Facade: api, CORSOrigins: origins})
	s.mux.Handle("/raw/", api)
	return s, &reached
}

// TestCORS checks the answers of docs/spec/http-routing.md §4 (#949) to a listed origin and to
// one that is not: Access-Control-Allow-Origin on /api/ and /-/version with the listed origin, a
// preflight answered 204 before the route, never Access-Control-Allow-Credentials, Vary: Origin
// on every answer of those paths, and nothing for an origin not listed (X-13), on the frontends
// and on /v2, or without origins.
func TestCORS(t *testing.T) {
	preflight := map[string]string{"Access-Control-Request-Method": "DELETE", "Access-Control-Request-Headers": "authorization,dpop"}
	tests := []struct {
		name    string
		origins []string
		method  string
		path    string
		origin  string
		header  map[string]string
		status  int
		reached bool
		allow   bool   // Access-Control-Allow-Origin is the origin
		vary    bool   // Vary: Origin
		expose  string // Access-Control-Expose-Headers
	}{
		{name: "a read of the API from the listed origin", origins: []string{uiOrigin}, method: http.MethodGet,
			path: "/api/v1/repos", origin: uiOrigin, status: http.StatusNoContent, reached: true, allow: true, vary: true,
			expose: "Date, Retry-After, WWW-Authenticate, X-Request-Id"},
		{name: "the build from the listed origin", origins: []string{uiOrigin}, method: http.MethodGet,
			path: "/-/version", origin: uiOrigin, status: http.StatusOK, allow: true, vary: true, expose: "Date, Retry-After, WWW-Authenticate, X-Request-Id"},
		{name: "a preflight from the listed origin", origins: []string{"http://localhost:5173", uiOrigin}, method: http.MethodOptions,
			path: "/api/v1/users/alice/tokens/7", origin: uiOrigin, header: preflight, status: http.StatusNoContent, allow: true, vary: true},
		{name: "a preflight of a path that is no endpoint", origins: []string{uiOrigin}, method: http.MethodOptions,
			path: "/api/v2/nothing", origin: uiOrigin, header: preflight, status: http.StatusNoContent, allow: true, vary: true},
		{name: "an OPTIONS from the listed origin that is no preflight", origins: []string{uiOrigin}, method: http.MethodOptions,
			path: "/api/v1/info", origin: uiOrigin, status: http.StatusNotFound, reached: true, allow: true, vary: true,
			expose: "Date, Retry-After, WWW-Authenticate, X-Request-Id"},
		{name: "a preflight from an origin not listed", origins: []string{uiOrigin}, method: http.MethodOptions,
			path: "/api/v1/repos", origin: "https://evil.test", header: preflight, status: http.StatusNotFound, reached: true, vary: true},
		{name: "a preflight from an origin that differs in its port", origins: []string{uiOrigin}, method: http.MethodOptions,
			path: "/api/v1/info", origin: uiOrigin + ":443", header: preflight, status: http.StatusNotFound, reached: true, vary: true},
		{name: "a read without an origin", origins: []string{uiOrigin}, method: http.MethodGet,
			path: "/api/v1/repos", status: http.StatusNoContent, reached: true, vary: true},
		{name: "a preflight without origins", method: http.MethodOptions,
			path: "/api/v1/repos", origin: "https://evil.test", header: preflight, status: http.StatusNotFound, reached: true},
		{name: "a preflight of the listed origin without origins", method: http.MethodOptions,
			path: "/api/v1/info", origin: uiOrigin, header: preflight, status: http.StatusNotFound, reached: true},
		{name: "a frontend from the listed origin", origins: []string{uiOrigin}, method: http.MethodGet,
			path: "/raw/assets/x", origin: uiOrigin, status: http.StatusNoContent, reached: true},
		{name: "a preflight of a frontend from the listed origin", origins: []string{uiOrigin}, method: http.MethodOptions,
			path: "/raw/assets/x", origin: uiOrigin, header: preflight, status: http.StatusNotFound, reached: true},
		{name: "/v2 from the listed origin", origins: []string{uiOrigin}, method: http.MethodGet,
			path: "/v2/", origin: uiOrigin, status: http.StatusNoContent, reached: true},
		{name: "the health check from the listed origin", origins: []string{uiOrigin}, method: http.MethodGet,
			path: "/-/healthz", origin: uiOrigin, status: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, reached := corsServer(t, tt.origins...)
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			for k, v := range tt.header {
				req.Header.Set(k, v)
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			h := rr.Header()
			if rr.Code != tt.status || *reached != tt.reached {
				t.Fatalf("status %d, route reached %v; want %d, %v", rr.Code, *reached, tt.status, tt.reached)
			}
			wantAllow := ""
			if tt.allow {
				wantAllow = tt.origin
			}
			if got := h.Get("Access-Control-Allow-Origin"); got != wantAllow {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, wantAllow)
			}
			if got := strings.Join(h.Values("Vary"), ", "); (got == "Origin") != tt.vary {
				t.Errorf("Vary = %q, want Origin: %v", got, tt.vary)
			}
			if got := h.Get("Access-Control-Expose-Headers"); got != tt.expose {
				t.Errorf("Access-Control-Expose-Headers = %q, want %q", got, tt.expose)
			}
			isPreflight := tt.allow && tt.header != nil
			for k, want := range map[string]string{
				"Access-Control-Allow-Methods": "GET, HEAD, POST, PUT, PATCH, DELETE",
				"Access-Control-Allow-Headers": "Authorization, Content-Type, DPoP",
				"Access-Control-Max-Age":       "600",
			} {
				if !isPreflight {
					want = ""
				}
				if got := h.Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			if v := h.Values("Access-Control-Allow-Credentials"); len(v) != 0 {
				t.Errorf("Access-Control-Allow-Credentials = %q, want none", v)
			}
			checkSecurityHeaders(t, h)
		})
	}
}

// TestCORSCrossOrigin checks the cross-origin refusal with a listed origin (#949): its unsafe
// requests pass on /api/, as the server's own; on a frontend and on /v2 they are refused as
// another site's, and an origin not listed is refused on /api/ too. The refusal of a listed
// origin on /api/ has Access-Control-Allow-Origin, so that the UI reads it.
func TestCORSCrossOrigin(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		path    string
		header  map[string]string
		refused bool
		allow   bool
	}{
		{name: "a POST of the API from the listed origin", method: http.MethodPost, path: "/api/v1/repos",
			header: map[string]string{"Origin": uiOrigin, "Sec-Fetch-Site": "cross-site"}, allow: true},
		{name: "a DELETE of the API from the listed origin, an older browser", method: http.MethodDelete, path: "/api/v1/users/alice",
			header: map[string]string{"Origin": uiOrigin}, allow: true},
		{name: "a POST of the API from an origin not listed", method: http.MethodPost, path: "/api/v1/repos",
			header: map[string]string{"Origin": "https://evil.test", "Sec-Fetch-Site": "cross-site"}, refused: true},
		{name: "a POST of the API from another site without an origin", method: http.MethodPost, path: "/api/v1/repos",
			header: map[string]string{"Sec-Fetch-Site": "cross-site"}, refused: true},
		{name: "a frontend upload from the listed origin", method: http.MethodPut, path: "/raw/assets/x",
			header: map[string]string{"Origin": uiOrigin, "Sec-Fetch-Site": "cross-site"}, refused: true},
		{name: "an upload on /v2 from the listed origin", method: http.MethodPost, path: "/v2/oci/images/app/blobs/uploads/",
			header: map[string]string{"Origin": uiOrigin, "Sec-Fetch-Site": "cross-site"}, refused: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, reached := corsServer(t, uiOrigin)
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(`{}`))
			for k, v := range tt.header {
				req.Header.Set(k, v)
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			switch {
			case tt.refused && (rr.Code != http.StatusForbidden || *reached):
				t.Fatalf("status %d, route reached %v; want 403 before the route", rr.Code, *reached)
			case !tt.refused && (rr.Code != http.StatusNoContent || !*reached):
				t.Fatalf("status %d, route reached %v; want the route's 204", rr.Code, *reached)
			}
			wantAllow := ""
			if tt.allow {
				wantAllow = uiOrigin
			}
			if got := rr.Header().Get("Access-Control-Allow-Origin"); got != wantAllow {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, wantAllow)
			}
		})
	}
}

// TestCORSRefusal checks that the 401 of the authentication middleware has the CORS headers of a
// listed origin: a UI whose token was revoked reads the 401 and signs out.
func TestCORSRefusal(t *testing.T) {
	s, _, _ := authServer(t, usersReader{})
	s2, err := New(Options{Listen: "127.0.0.1:0", API: s.opts.API, Authenticator: s.opts.Authenticator, Log: s.log,
		CORSOrigins: []string{uiOrigin}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	req.Header.Set("Origin", uiOrigin)
	req.Header.Set("Authorization", "Bearer "+revokedPAT)
	rr := httptest.NewRecorder()
	s2.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized || rr.Header().Get("Access-Control-Allow-Origin") != uiOrigin {
		t.Errorf("status %d, Access-Control-Allow-Origin %q; want 401 with %q", rr.Code, rr.Header().Get("Access-Control-Allow-Origin"), uiOrigin)
	}
}

// TestCORSOriginsRefused checks that New refuses an origin that net/http cannot trust.
func TestCORSOriginsRefused(t *testing.T) {
	if _, err := New(Options{Listen: "127.0.0.1:0", CORSOrigins: []string{"https://ui.example.com/ui"}}); err == nil ||
		!strings.Contains(err.Error(), "CORSOrigins") {
		t.Errorf("New: %v, want an error naming CORSOrigins", err)
	}
}

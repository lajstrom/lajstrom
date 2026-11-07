package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFacadeMount checks Options.Facade: mounted at /v2/, it gets every path below it, behind
// the middleware; the ServeMux redirects /v2 to /v2/; nil leaves /v2/ to the ServeMux's plain-text 404.
func TestFacadeMount(t *testing.T) {
	facade := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Facade-Saw", r.Method+" "+r.URL.Path+" "+RequestID(r.Context()))
		_, _ = io.WriteString(w, "facade\n")
	})
	tests := []struct {
		name   string
		facade http.Handler
		method string
		path   string
		status int
		saw    string // X-Facade-Saw; "" when the facade was not called
	}{
		{name: "base", facade: facade, method: http.MethodGet, path: "/v2/", status: 200, saw: "GET /v2/ req-1"},
		{name: "upload", facade: facade, method: http.MethodPatch, path: "/v2/oci/images/a/blobs/uploads/0f", status: 200,
			saw: "PATCH /v2/oci/images/a/blobs/uploads/0f req-1"},
		{name: "without the slash", facade: facade, method: http.MethodGet, path: "/v2", status: 0}, // a redirect to /v2/
		{name: "no facade", method: http.MethodGet, path: "/v2/", status: 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestServer(t, Options{Facade: tt.facade})
			r := httptest.NewRequest(tt.method, tt.path, nil)
			r.Header.Set(requestIDHeader, "req-1")
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, r)
			switch {
			case tt.status == 0 && (rr.Code/100 != 3 || rr.Header().Get("Location") != "/v2/"):
				t.Errorf("status = %d, Location %q; want a redirect to /v2/", rr.Code, rr.Header().Get("Location"))
			case tt.status != 0 && rr.Code != tt.status:
				t.Errorf("status = %d, want %d", rr.Code, tt.status)
			}
			if saw := rr.Header().Get("X-Facade-Saw"); saw != tt.saw {
				t.Errorf("the facade saw %q, want %q", saw, tt.saw)
			}
		})
	}
}

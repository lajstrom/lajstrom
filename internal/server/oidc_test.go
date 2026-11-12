package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// TestOIDCRoutes: /-/oidc/ is mounted from Options.OIDC, and its requests go on as anonymous
// with their Authorization header untouched, refused credentials included
// (docs/spec/http-routing.md §7); without Options.OIDC the path is 404.
func TestOIDCRoutes(t *testing.T) {
	var got auth.Principal
	var called bool
	login := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called, got = true, auth.PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusFound)
	})
	s, _ := newTestServer(t, Options{OIDC: login, Authenticator: auth.NewAuthenticator(auth.Options{Index: usersReader{}})})
	for _, authorization := range []string{"", basic("alice", "wrong"), "Bearer " + revokedPAT} {
		called = false
		r := httptest.NewRequest(http.MethodGet, "/-/oidc/login", nil)
		if authorization != "" {
			r.Header.Set("Authorization", authorization)
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, r)
		if rr.Code != http.StatusFound || !called || got != auth.Anonymous {
			t.Errorf("Authorization %q: %d, called %v as %v; want the handler as anonymous", authorization, rr.Code, called, got)
		}
	}
	off, _ := newTestServer(t, Options{})
	rr := httptest.NewRecorder()
	off.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/-/oidc/login", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("without OIDC: %d, want 404", rr.Code)
	}
}

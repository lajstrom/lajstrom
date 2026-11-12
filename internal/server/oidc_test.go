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
	type visit struct {
		reached bool
		as      auth.Principal
		header  string
	}
	var last visit
	login := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = visit{reached: true, as: auth.PrincipalFrom(r.Context()), header: r.Header.Get("Authorization")}
		w.WriteHeader(http.StatusFound)
	})
	authn := auth.NewAuthenticator(auth.Options{Index: usersReader{}})
	on, _ := newTestServer(t, Options{OIDC: login, Authenticator: authn})
	off, _ := newTestServer(t, Options{Authenticator: authn})
	status := func(s *Server, header string) int {
		req := httptest.NewRequest(http.MethodGet, "/-/oidc/login", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w.Code
	}
	for _, header := range []string{"", basic("alice", "wrong"), "Bearer " + revokedPAT} {
		last = visit{}
		code := status(on, header)
		if want := (visit{reached: true, as: auth.Anonymous, header: header}); code != http.StatusFound || last != want {
			t.Errorf("Authorization %q: status %d, the handler saw %+v; want 302 and %+v", header, code, last, want)
		}
	}
	if code := status(off, ""); code != http.StatusNotFound {
		t.Errorf("without Options.OIDC: status %d, want 404", code)
	}
}

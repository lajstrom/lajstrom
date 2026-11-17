package server

import (
	"context"
	"net/http"
	"regexp"
)

// passwordPath is the path of POST /api/v1/users/{user}/password, whose body decides whether the
// request takes a credential: with currentPassword it is the user's own change, whose credential
// is that password and whose Authorization is ignored; without, an admin's new password
// (docs/spec/rbac.md §1, A user's own password; §3).
var passwordPath = regexp.MustCompile(`^/api/v1/users/[^/]+/password$`)

// deferredKey is the context key that marks a request whose authentication the middleware left
// to its handler (deferAuthentication).
type deferredKey struct{}

// deferAuthentication passes r on to next as anonymous, its header untouched, with the
// authentication of its credentials left to the handler, which runs it with Authenticate when the
// body asks for it, or never: POST /api/v1/users/{user}/password (passwordPath).
func (s *Server) deferAuthentication(w http.ResponseWriter, r *http.Request, next http.Handler) {
	next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deferredKey{}, true)))
}

// Authenticate runs the authentication that the middleware left to the handler of r
// (deferAuthentication), as the middleware runs it on every other route (authenticateHeader): it
// returns r with the principal and the credential of its Authorization (auth.WithPrincipal,
// auth.WithCredential), anonymous without one, or false when it has answered r on w itself:
// refused credentials 401, a password whose user must change it 403, a throttle 429, a failure 500
// or 503. A request whose authentication was not left to its handler, or was run already, is
// returned as it is. The management API calls it (api.Options.Authenticate).
func (s *Server) Authenticate(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if deferred, _ := r.Context().Value(deferredKey{}).(bool); !deferred || s.opts.Authenticator == nil {
		return r, true
	}
	var out *http.Request
	r = r.WithContext(context.WithValue(r.Context(), deferredKey{}, false)) // once
	s.authenticateHeader(w, r, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { out = r }))
	return out, out != nil
}

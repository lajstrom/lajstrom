package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// TestAuthenticateDeferred: POST /api/v1/users/{user}/password goes on as anonymous with its
// Authorization untouched, refused, wrong or a password that must change included, since its body
// may be the user's own change, whose credential is in the body (docs/spec/rbac.md §1, A user's
// own password); the handler authenticates it with Authenticate when the body is an admin's new
// password, as the middleware does on every other route, which answers a refusal itself. Any other
// request on the path is authenticated by the middleware.
func TestAuthenticateDeferred(t *testing.T) {
	const path = "/api/v1/users/alice/password"
	for _, tc := range []struct {
		name          string
		r             index.Store
		method, body  string
		authorization string
		status        int
		principal     string // of the handler, "" when it was not called
	}{
		{"the own change, a wrong password", usersReader{}, http.MethodPost, `{"currentPassword": "x"}`, basic("alice", "wrong"), 204, "anonymous"},
		{"the own change, a revoked token", usersReader{}, http.MethodPost, `{"currentPassword": "x"}`, "Bearer " + revokedPAT, 204, "anonymous"},
		{"the own change, must change", mustChangeReader{}, http.MethodPost, `{"currentPassword": "x"}`, basic("alice", alicePassword), 204,
			"anonymous"},
		{"an admin's, no credential", usersReader{}, http.MethodPost, `{}`, "", 204, "anonymous"},
		{"an admin's, a password", usersReader{}, http.MethodPost, `{}`, basic("alice", alicePassword), 204, "user:" + aliceUID},
		{"an admin's, a token", usersReader{}, http.MethodPost, `{}`, "Bearer " + alicePAT, 204, "user:" + aliceUID},
		{"an admin's, a wrong password", usersReader{}, http.MethodPost, `{}`, basic("alice", "wrong"), 401, ""},
		{"an admin's, a revoked token", usersReader{}, http.MethodPost, `{}`, "Bearer " + revokedPAT, 401, ""},
		{"an admin's, must change", mustChangeReader{}, http.MethodPost, `{}`, basic("alice", alicePassword), 403, ""},
		{"another method", usersReader{}, http.MethodPut, `{"currentPassword": "x"}`, basic("alice", "wrong"), 401, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				seen string
				s    *Server
			)
			api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(b), "currentPassword") {
					var ok bool
					if r, ok = s.Authenticate(w, r); !ok {
						return
					}
				}
				seen = auth.PrincipalFrom(r.Context()).String()
				w.WriteHeader(http.StatusNoContent)
			})
			s, _ = newTestServer(t, Options{API: api, Authenticator: auth.NewAuthenticator(auth.Options{Index: tc.r})})
			r := httptest.NewRequest(tc.method, path, strings.NewReader(tc.body))
			if tc.authorization != "" {
				r.Header.Set("Authorization", tc.authorization)
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, r)
			if rr.Code != tc.status || seen != tc.principal {
				t.Errorf("%d, the handler saw %q; want %d, %q (%s)", rr.Code, seen, tc.status, tc.principal, rr.Body)
			}
			if tc.status == http.StatusForbidden && !strings.Contains(rr.Body.String(), `"code":"FORBIDDEN"`) {
				t.Errorf("the 403: %s", rr.Body)
			}
		})
	}
	// A request whose authentication the middleware did not leave to its handler is taken as it is.
	s, _, _ := authServer(t, usersReader{})
	r := httptest.NewRequest(http.MethodPost, path, nil)
	if got, ok := s.Authenticate(httptest.NewRecorder(), r); !ok || got != r {
		t.Errorf("Authenticate without the middleware = %v, %v", got, ok)
	}
}

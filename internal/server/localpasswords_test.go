package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// TestAuthenticatePasswordsOff: with auth.localPasswords false a password is 401 on every route
// that takes one, Basic credentials and the form of POST /v2/token, in the route's format, with
// the message that passwords are off; a personal access token in Basic credentials is its user
// (docs/spec/rbac.md §1, Local passwords).
func TestAuthenticatePasswordsOff(t *testing.T) {
	s, sw, _ := authServerWith(t, usersReader{}, auth.Options{PasswordsOff: true})
	const msg = "passwords are off on this server: use a personal access token, or log in through the IdP with lajstrom login"
	for _, tt := range []struct {
		name, path, body string
	}{
		{"elsewhere", "/raw/scratch/file", msg + "\n"},
		{"the API", "/api/v1/whoami", `{"error":{"code":"UNAUTHENTICATED","message":"` + msg + `"}}` + "\n"},
		{"/v2/token", "/v2/token", `{"errors":[{"code":"UNAUTHORIZED","message":"` + msg + `"}]}` + "\n"},
	} {
		for _, secret := range []string{alicePassword, "a wrong password"} {
			if rr := serveAs(s, tt.path, basic("alice", secret)); rr.Code != http.StatusUnauthorized || rr.Body.String() != tt.body ||
				rr.Header().Get("WWW-Authenticate") != basicChallenge {
				t.Errorf("%s, Basic: %d %q (%s), want 401 %q", tt.name, rr.Code, rr.Body, rr.Header().Get("WWW-Authenticate"), tt.body)
			}
		}
	}
	if rr := postForm(s, passwordForm("alice", alicePassword), ""); rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), msg) {
		t.Errorf("the form of POST /v2/token: %d %s, want 401 saying passwords are off", rr.Code, rr.Body)
	}
	*sw = seen{}
	if rr := serveAs(s, "/test/x", basic("alice", alicePAT)); rr.Code != http.StatusNoContent || sw.principal.Name != aliceUID {
		t.Errorf("a personal access token in Basic credentials: %d as %v, want alice", rr.Code, sw.principal)
	}
}

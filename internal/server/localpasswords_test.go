package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
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

// mustChangeReader is usersReader whose alice must change her password (docs/spec/rbac.md §1,
// Change at first use).
type mustChangeReader struct{ usersReader }

func (m mustChangeReader) GetUser(ctx context.Context, name string) (*index.User, error) {
	u, err := m.usersReader.GetUser(ctx, name)
	if err == nil {
		u.MustChange = true
	}
	return u, err
}

// TestAuthenticateMustChange: a right password whose user must change it is 403 on every route
// that takes it, Basic credentials and the form of POST /v2/token, in the route's format, without
// a challenge, saying how to change it; a wrong one is 401 as always; the user's personal access
// token keeps working (docs/spec/rbac.md §1, Change at first use).
func TestAuthenticateMustChange(t *testing.T) {
	s, sw, _ := authServerWith(t, mustChangeReader{}, auth.Options{})
	msg := auth.MsgMustChange
	for _, tt := range []struct {
		name, path, body string
	}{
		{"elsewhere", "/raw/scratch/file", msg + "\n"},
		{"the API", "/api/v1/whoami", `{"error":{"code":"FORBIDDEN","message":"` + msg + `"}}` + "\n"},
		{"/v2/token", "/v2/token", `{"errors":[{"code":"DENIED","message":"` + msg + `"}]}` + "\n"},
	} {
		if rr := serveAs(s, tt.path, basic("alice", alicePassword)); rr.Code != http.StatusForbidden || rr.Body.String() != tt.body ||
			rr.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("%s, Basic: %d %q (%s), want 403 %q without a challenge", tt.name, rr.Code, rr.Body, rr.Header().Get("WWW-Authenticate"),
				tt.body)
		}
		if rr := serveAs(s, tt.path, basic("alice", "a wrong password")); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s, a wrong password: %d, want 401", tt.name, rr.Code)
		}
	}
	if rr := postForm(s, passwordForm("alice", alicePassword), ""); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), msg) {
		t.Errorf("the form of POST /v2/token: %d %s, want 403 saying the password must be changed", rr.Code, rr.Body)
	}
	*sw = seen{}
	if rr := serveAs(s, "/test/x", basic("alice", alicePAT)); rr.Code != http.StatusNoContent || sw.principal.Name != aliceUID {
		t.Errorf("a personal access token in Basic credentials: %d as %v, want alice", rr.Code, sw.principal)
	}
}

// TestAuthenticateLockoutMessage: while auth.lockout.duration is set, every 401 that refuses a
// user name and a password says what locks a password and for how long, Basic credentials on
// every route and the form of POST /v2/token, the same as npm login's and the sign-in's; a token
// refused as a bearer keeps the message of a refused credential (docs/spec/rbac.md §1, Local
// passwords).
func TestAuthenticateLockoutMessage(t *testing.T) {
	s, _, _ := authServerWith(t, usersReader{}, auth.Options{Lockout: 30 * time.Minute})
	const msg = "the user name or the password was refused: 10 wrong passwords in a row lock a password for 30m"
	for _, tt := range []struct {
		name, path, user, body string
	}{
		{"elsewhere", "/raw/scratch/file", "mallory", msg + "\n"},
		{"a malformed name", "/raw/scratch/file", "Not A Name", msg + "\n"},
		{"the API", "/api/v1/whoami", "mallory", `{"error":{"code":"UNAUTHENTICATED","message":"` + msg + `"}}` + "\n"},
		{"/v2/token", "/v2/token", "mallory", `{"errors":[{"code":"UNAUTHORIZED","message":"` + msg + `"}]}` + "\n"},
	} {
		if rr := serveAs(s, tt.path, basic(tt.user, "a password")); rr.Code != http.StatusUnauthorized || rr.Body.String() != tt.body {
			t.Errorf("%s: %d %q, want 401 %q", tt.name, rr.Code, rr.Body, tt.body)
		}
	}
	if rr := postForm(s, passwordForm("mallory", "a password"), ""); rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), msg) {
		t.Errorf("the form of POST /v2/token: %d %s, want 401 saying what locks a password", rr.Code, rr.Body)
	}
	if rr := serveAs(s, "/raw/scratch/file", "Bearer "+revokedPAT); rr.Code != http.StatusUnauthorized || rr.Body.String() != msgBadCredentials+"\n" {
		t.Errorf("a revoked bearer: %d %q, want 401 %q", rr.Code, rr.Body, msgBadCredentials)
	}
}

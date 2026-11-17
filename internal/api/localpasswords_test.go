package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/dpop/dpoptest"
)

// TestPasswordsOff: with auth.localPasswords false (docs/spec/rbac.md §1, Local passwords), info
// leaves password out of authModes, a password of POST /api/v1/users and of POST …/password is
// 400 naming the setting, a service account is made as before, and a sign-in with a password is
// 401 saying that passwords are off, the right password too.
func TestPasswordsOff(t *testing.T) {
	for _, tc := range []struct {
		oidc  bool
		modes []string
	}{{false, []string{"token"}}, {true, []string{"token", "oidc"}}} {
		info := testInfo
		info.OIDC, info.PasswordsOff = tc.oidc, true
		var got Info
		decodeJSON(t, newFixture(t, Options{Info: info}).do(http.MethodGet, "/api/v1/info", "", auth.Anonymous), http.StatusOK, &got)
		if !slices.Equal(got.AuthModes, tc.modes) {
			t.Errorf("OIDC %v: authModes %q, want %q", tc.oidc, got.AuthModes, tc.modes)
		}
	}

	f := loginFixture(t, Options{PasswordsOff: true})
	f.addUsers(t, "admin")
	wantError(t, f.do(http.MethodPost, "/api/v1/users", `{"name": "bob", "password": "bob's password"}`, admin),
		http.StatusBadRequest, codeInvalid, "auth.localPasswords: false")
	wantError(t, f.do(http.MethodPost, "/api/v1/users/alice/password", `{"password": "a new password"}`, admin),
		http.StatusBadRequest, codeInvalid, "auth.localPasswords: false")
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users", `{"name": "robot", "kind": "service", "owner": "user:alice"}`, admin),
		http.StatusCreated)
	if _, err := f.st.GetUser(context.Background(), "bob"); err == nil {
		t.Error("bob was made")
	}
	wantError(t, f.signIn(`{"username": "alice", "password": "`+loginPassword+`"}`, loginProof(t, dpoptest.NewKey(t)),
		auth.Principal{}, auth.Credential{}), http.StatusUnauthorized, codeUnauthenticated, "passwords are off on this server")
}

// TestLoginMustChange: a sign-in with the right password of a user that must change it is 403,
// the one 403 of a sign-in, saying how, and makes no token; a wrong password is 401
// (docs/spec/rbac.md §1, Change at first use; §10).
func TestLoginMustChange(t *testing.T) {
	f := loginFixture(t, Options{})
	alice, err := f.st.GetUser(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	alice.MustChange = true
	if err := f.st.UpdateUser(context.Background(), alice); err != nil {
		t.Fatal(err)
	}
	key := dpoptest.NewKey(t)
	wantError(t, f.signIn(`{"username": "alice", "password": "`+loginPassword+`"}`, loginProof(t, key), auth.Principal{}, auth.Credential{}),
		http.StatusForbidden, codeForbidden, auth.MsgMustChange)
	wantError(t, f.signIn(`{"username": "alice", "password": "a wrong password"}`, loginProof(t, key), auth.Principal{}, auth.Credential{}),
		http.StatusUnauthorized, codeUnauthenticated, "the user name or the password is wrong")
	if evs := auditOf(t, f.st, auth.ActionTokenCreate); len(evs) != 0 {
		t.Errorf("%d token.create rows, want none", len(evs))
	}
}

// TestLoginLockoutMessage: while auth.lockout.duration is set, a refused sign-in with a password
// says what locks a password and for how long, whatever the reason (docs/spec/rbac.md §1, §10).
func TestLoginLockoutMessage(t *testing.T) {
	f := loginFixture(t, Options{})
	f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log, DPoP: loginVerifier(f.st),
		Passwords: auth.NewAuthenticator(auth.Options{Index: f.st, Lockout: 30 * time.Minute})}))
	key := dpoptest.NewKey(t)
	for _, body := range []string{`{"username": "alice", "password": "wrong password"}`, `{"username": "nobody", "password": "a password"}`,
		`{"username": "oidc-bob", "password": "a password"}`, `{"username": "Alice", "password": "a password"}`} {
		wantError(t, f.signIn(body, loginProof(t, key), auth.Principal{}, auth.Credential{}), http.StatusUnauthorized, codeUnauthenticated,
			"the user name or the password was refused: 10 wrong passwords in a row lock a password for 30m")
	}
}

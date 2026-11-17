package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

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

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// TestOwnPasswordThroughServer runs POST …/password behind the server's middleware, which leaves
// its authentication to the handler (Options.Authenticate, server.Server.Authenticate): the own
// change of a user whose password must change, sent with that password in Basic credentials too,
// is 204, its Authorization ignored; the admin's body with that password is 403, with a wrong one
// 401, and with an admin's password 204 (docs/spec/rbac.md §1, A user's own password).
func TestOwnPasswordThroughServer(t *testing.T) {
	f := newFixture(t, Options{})
	users := &auth.Users{Index: f.st, PasswordPolicy: auth.PasswordPolicy{ChangeAtFirstUse: true}}
	for _, name := range []string{"admin", "alice"} {
		if _, err := users.Create(context.Background(), name, name+"-password", name == "admin", auth.Origin{Source: SourceCLI}); err != nil {
			t.Fatal(err)
		}
	}
	no := false
	if _, err := users.Change(context.Background(), "admin", auth.UserChange{MustChange: &no}, auth.Origin{}); err != nil {
		t.Fatal(err)
	}
	authn := auth.NewAuthenticator(auth.Options{Index: f.st, Users: users})
	var srv *server.Server
	h := New(Options{Repos: f.repos, Log: f.repos.Log, Passwords: authn, Users: users,
		Authenticate: func(w http.ResponseWriter, r *http.Request) (*http.Request, bool) { return srv.Authenticate(w, r) }})
	srv, err := server.New(server.Options{Listen: "127.0.0.1:0", API: h, Log: f.repos.Log, Authenticator: authn})
	if err != nil {
		t.Fatal(err)
	}
	send := func(user, password, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/users/alice/password", strings.NewReader(body))
		r.SetBasicAuth(user, password)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, r)
		return rr
	}
	wantError(t, send("alice", "alice-password", `{"password": "set by alice"}`), http.StatusForbidden, codeForbidden, auth.MsgMustChange)
	wantError(t, send("admin", "a wrong password", `{"password": "set by the admin"}`), http.StatusUnauthorized, codeUnauthenticated)
	wantStatus(t, send("alice", "alice-password", `{"currentPassword": "alice-password", "password": "alice's own password"}`),
		http.StatusNoContent)
	if must, n := mustChangeOf(t, f.st, "alice"); must || n != 1 {
		t.Errorf("alice after her change: mustChange %v, history %d; want false, 1", must, n)
	}
	wantStatus(t, send("admin", "admin-password", `{"password": "set by the admin"}`), http.StatusNoContent)
	if must, _ := mustChangeOf(t, f.st, "alice"); !must {
		t.Error("alice need not change the admin's new password under changeAtFirstUse")
	}
}

// ownFixture is loginFixture whose authenticator has its users service, under policy, for the
// user's own change (docs/spec/rbac.md §1, A user's own password), and the users admin and
// robot, a service account.
func ownFixture(t *testing.T, policy auth.PasswordPolicy) *fixture {
	t.Helper()
	f := loginFixture(t, Options{})
	users := &auth.Users{Index: f.st, Log: f.repos.Log, Now: func() time.Time { return testNow }, PasswordPolicy: policy}
	f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log, DPoP: loginVerifier(f.st), PasswordPolicy: policy,
		Passwords: auth.NewAuthenticator(auth.Options{Index: f.st, Log: f.repos.Log, Users: users})}))
	f.addUsers(t, "admin")
	if err := f.st.CreateUser(context.Background(), &index.User{Name: "robot", Kind: auth.KindService, Owner: "group:ops"}); err != nil {
		t.Fatal(err)
	}
	return f
}

// mustChangeOf returns the mustChange of the user called name, and the length of its history.
func mustChangeOf(t *testing.T, st index.Store, name string) (bool, int) {
	t.Helper()
	u, err := st.GetUser(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u.MustChange, len(u.PasswordHistory)
}

// TestOwnPassword: POST …/password with currentPassword is the user's own change, with no other
// credential: 204, under mustChange too, which it clears, with a user.password row by the user;
// by its uid too; the new password then signs in. A wrong current password, of a user or of a
// name no user has, of a uid of no user, of a service account, is 401 with the message of every
// refused password; a rule or one of the last four 400 (docs/spec/rbac.md §1; http-routing.md
// §4).
func TestOwnPassword(t *testing.T) {
	f := ownFixture(t, auth.PasswordPolicy{MinLength: 12})
	yes := true
	if _, err := (&auth.Users{Index: f.st}).Change(context.Background(), "alice", auth.UserChange{MustChange: &yes}, auth.Origin{}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, path, body string
		status           int
		code             string
		msg              string
	}{
		{"a wrong password", "/api/v1/users/alice/password", `{"currentPassword": "wrong", "password": "a new password 1"}`, 401,
			codeUnauthenticated, "the user name or the password is wrong"},
		{"a name no user has", "/api/v1/users/nobody/password", `{"currentPassword": "` + loginPassword + `", "password": "a new password 1"}`, 401,
			codeUnauthenticated, "the user name or the password is wrong"},
		{"a uid of no user", "/api/v1/users/01a1e902-0536-7149-ba36-c3af6ce59e32/password",
			`{"currentPassword": "` + loginPassword + `", "password": "a new password 1"}`, 401, codeUnauthenticated, "the user name or the password is wrong"},
		{"a service account", "/api/v1/users/robot/password", `{"currentPassword": "` + loginPassword + `", "password": "a new password 1"}`, 401,
			codeUnauthenticated, "the user name or the password is wrong"},
		{"a rule", "/api/v1/users/alice/password", `{"currentPassword": "` + loginPassword + `", "password": "short pw 1"}`, 400,
			codeInvalid, "password: must be at least 12 characters (auth.passwords.minLength)"},
		{"the current one", "/api/v1/users/alice/password", `{"currentPassword": "` + loginPassword + `", "password": "` + loginPassword + `"}`, 400,
			codeInvalid, "password: was used before"},
	} {
		wantError(t, f.do(http.MethodPost, tt.path, tt.body, auth.Principal{}), tt.status, tt.code, tt.msg)
	}
	if must, n := mustChangeOf(t, f.st, "alice"); !must || n != 0 {
		t.Fatalf("alice after the refused changes: mustChange %v, history %d; want true, 0", must, n)
	}
	alice, err := f.st.GetUser(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users/"+alice.UID+"/password",
		`{"currentPassword": "`+loginPassword+`", "password": "a new password 1"}`, auth.Principal{}), http.StatusNoContent)
	if must, n := mustChangeOf(t, f.st, "alice"); must || n != 1 {
		t.Errorf("alice after her change: mustChange %v, history %d; want false, 1", must, n)
	}
	rows := auditOf(t, f.st, auth.ActionUserPassword)
	if len(rows) != 1 || rows[0].Principal != "user:"+alice.UID || rows[0].Subject != "user:"+alice.UID || rows[0].details["source"] != "api" {
		t.Errorf("the user.password rows: %+v, want one by alice herself", rows)
	}
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users/alice/password",
		`{"currentPassword": "a new password 1", "password": "a newer password 2"}`, admin), http.StatusNoContent)
}

// TestOwnPasswordAnswers: a throttled account or address is 429 with Retry-After; a server that
// checks no passwords 501; passwords off 400 naming the setting.
func TestOwnPasswordAnswers(t *testing.T) {
	const body = `{"currentPassword": "x", "password": "a new password"}`
	f := loginFixture(t, Options{Passwords: throttledPasswords{}})
	rr := f.do(http.MethodPost, "/api/v1/users/alice/password", body, auth.Principal{})
	wantError(t, rr, http.StatusTooManyRequests, codeUnavailable, "too many failed logins")
	if got := rr.Header().Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want 3", got)
	}
	f = newFixture(t, Options{})
	wantError(t, f.do(http.MethodPost, "/api/v1/users/alice/password", body, auth.Principal{}), http.StatusNotImplemented, codeUnavailable,
		"checks no passwords")
	f = loginFixture(t, Options{PasswordsOff: true})
	users := &auth.Users{Index: f.st, PasswordsOff: true}
	f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log, PasswordsOff: true,
		Passwords: auth.NewAuthenticator(auth.Options{Index: f.st, PasswordsOff: true, Users: users})}))
	wantError(t, f.do(http.MethodPost, "/api/v1/users/alice/password", `{"currentPassword": "`+loginPassword+`", "password": "a new password"}`,
		auth.Principal{}), http.StatusBadRequest, codeInvalid, "auth.localPasswords: false")
}

// TestPatchMustChange: PATCH mustChange sets and clears it, the resource answers mustChange: true
// while it is set and leaves it out otherwise, each change a user.mustchange row and a new
// revision; a user without a password is 400 (docs/spec/rbac.md §1, Change at first use).
func TestPatchMustChange(t *testing.T) {
	f := ownFixture(t, auth.PasswordPolicy{})
	var u auth.User
	rr := f.do(http.MethodPatch, "/api/v1/users/alice", `{"mustChange": true}`, admin)
	decodeJSON(t, rr, http.StatusOK, &u)
	if !u.MustChange {
		t.Errorf("PATCH mustChange true answered %+v", u)
	}
	etag := rr.Header().Get("ETag")
	rr = f.do(http.MethodGet, "/api/v1/users/alice", "", admin)
	if body := rr.Body.String(); !strings.Contains(body, `"mustChange":true`) || rr.Header().Get("ETag") != etag {
		t.Errorf("GET after PATCH: %s, ETag %q; want mustChange true, ETag %q", body, rr.Header().Get("ETag"), etag)
	}
	rr = f.do(http.MethodPatch, "/api/v1/users/alice", `{"mustChange": false}`, admin)
	if body := rr.Body.String(); rr.Code != http.StatusOK || strings.Contains(body, "mustChange") || rr.Header().Get("ETag") == etag {
		t.Errorf("PATCH mustChange false: %d %s, ETag %q; want no mustChange and a new ETag", rr.Code, body, rr.Header().Get("ETag"))
	}
	wantError(t, f.do(http.MethodPatch, "/api/v1/users/robot", `{"mustChange": true}`, admin), http.StatusBadRequest, codeInvalid,
		"mustChange: user robot has no password")
	if rows := auditOf(t, f.st, auth.ActionUserMustChange); len(rows) != 2 || rows[0].details["mustChange"] != false ||
		rows[1].details["mustChange"] != true {
		t.Errorf("the user.mustchange rows: %+v, want true then false", rows)
	}
}

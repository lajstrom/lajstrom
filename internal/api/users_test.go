package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// TestUsersAPI goes through a user's life over the API: created, read by name and by uid,
// renamed and disabled in one PATCH, its password set, deleted, and its name taken again by a
// user with another uid. Each change is one audit event.
func TestUsersAPI(t *testing.T) {
	f := newFixture(t, Options{})
	var u auth.User
	rr := f.do(http.MethodPost, "/api/v1/users", `{"name": "alice", "password": "correct horse", "admin": false}`, admin)
	decodeJSON(t, rr, http.StatusCreated, &u)
	if loc := rr.Header().Get("Location"); loc != "/api/v1/users/alice" {
		t.Errorf("Location = %q, want /api/v1/users/alice", loc)
	}
	wantJSON(t, "the created user", u, `{"uid": "`+u.UID+`", "name": "alice", "kind": "local", "disabled": false, "createdAt": "2026-01-02T12:00:00Z"}`)
	for _, target := range []string{"/api/v1/users/alice", "/api/v1/users/" + u.UID} {
		var got auth.User
		decodeJSON(t, f.do(http.MethodGet, target, "", admin), http.StatusOK, &got)
		if got != u {
			t.Errorf("GET %s = %+v, want %+v", target, got, u)
		}
	}

	var patched auth.User
	decodeJSON(t, f.do(http.MethodPatch, "/api/v1/users/"+u.UID, `{"name": "alice2", "disabled": true}`, admin), http.StatusOK, &patched)
	if patched.UID != u.UID || patched.Name != "alice2" || !patched.Disabled {
		t.Errorf("PATCH = %+v, want alice2, disabled, uid %s", patched, u.UID)
	}
	wantError(t, f.do(http.MethodGet, "/api/v1/users/alice", "", admin), http.StatusNotFound, codeNotFound, "user alice does not exist")

	f.wantPasswordSet(t, "alice2", "battery staple")

	wantStatus(t, f.do(http.MethodDelete, "/api/v1/users/alice2", "", admin), http.StatusNoContent)
	wantError(t, f.do(http.MethodGet, "/api/v1/users/"+u.UID, "", admin), http.StatusNotFound, codeNotFound, u.UID)
	var again auth.User
	decodeJSON(t, f.do(http.MethodPost, "/api/v1/users", `{"name": "alice2", "password": "correct horse"}`, admin), http.StatusCreated, &again)
	if again.UID == u.UID {
		t.Errorf("the name came back with the deleted user's uid %s", u.UID)
	}
	f.wantUserAudit(t, u.UID)
}

// TestUsersAPIList pages through the live users in the order of their names.
func TestUsersAPIList(t *testing.T) {
	f := newFixture(t, Options{})
	f.addUsers(t, "carol", "alice", "bob", "dave", "erin")
	var names []string
	target, pages := "/api/v1/users?limit=2", 0
	for target != "" {
		var list UserList
		decodeJSON(t, f.do(http.MethodGet, target, "", admin), http.StatusOK, &list)
		for _, u := range list.Users {
			names = append(names, u.Name)
		}
		target, pages = "", pages+1
		if list.Next != "" {
			target = "/api/v1/users?limit=2&cursor=" + list.Next
		}
	}
	wantJSON(t, "the names of the pages", names, `["alice", "bob", "carol", "dave", "erin"]`)
	if pages != 3 {
		t.Errorf("%d pages of 2 for 5 users, want 3", pages)
	}
	var all UserList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/users", "", admin), http.StatusOK, &all)
	if len(all.Users) != 5 || all.Next != "" {
		t.Errorf("one page by default: %d users, next %q", len(all.Users), all.Next)
	}
}

// TestUsersAPIRefused checks the answers to what the API refuses.
func TestUsersAPIRefused(t *testing.T) {
	f := newFixture(t, Options{})
	f.addUsers(t, "bob")
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users", `{"name": "root", "password": "correct horse", "admin": true}`, admin), http.StatusCreated)
	root, err := f.st.GetUser(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, method, target, body string
		status                     int
		code, msg                  string
	}{
		{"a name that breaks the rule", "POST", "/api/v1/users", `{"name": "Alice", "password": "correct horse"}`, 400, codeInvalid, "name"},
		{"a name of uid form", "POST", "/api/v1/users", `{"name": "` + root.UID + `", "password": "correct horse"}`, 400, codeInvalid, "name"},
		{"a short password", "POST", "/api/v1/users", `{"name": "carol", "password": "short"}`, 400, codeInvalid, "password"},
		{"an unknown field", "POST", "/api/v1/users", `{"name": "carol", "password": "correct horse", "uid": "x"}`, 400, codeInvalid, "uid"},
		{"a taken name", "POST", "/api/v1/users", `{"name": "bob", "password": "correct horse"}`, 409, codeConflict, "user bob already exists"},
		{"a rename to a taken name", "PATCH", "/api/v1/users/root", `{"name": "bob"}`, 409, codeConflict, "user bob already exists"},
		{"an empty patch", "PATCH", "/api/v1/users/bob", `{}`, 400, codeInvalid, "nothing to change"},
		{"a patch of the uid", "PATCH", "/api/v1/users/bob", `{"uid": "x"}`, 400, codeInvalid, "uid"},
		{"an unknown user", "GET", "/api/v1/users/dave", "", 404, codeNotFound, "user dave does not exist"},
		{"a patch of an unknown user", "PATCH", "/api/v1/users/dave", `{"disabled": true}`, 404, codeNotFound, "user dave does not exist"},
		{"an unknown uid", "DELETE", "/api/v1/users/01a1c000-0000-7000-8000-000000000000", "", 404, codeNotFound, "does not exist"},
		{"the last admin disabled", "PATCH", "/api/v1/users/root", `{"disabled": true}`, 409, codeConflict, "admin"},
		{"the last admin deleted", "DELETE", "/api/v1/users/root", "", 409, codeConflict, "admin"},
		{"a limit of 0", "GET", "/api/v1/users?limit=0", "", 400, codeInvalid, "limit"},
		{"a limit over the bound", "GET", "/api/v1/users?limit=1001", "", 400, codeInvalid, "limit"},
		{"a cursor that is not one", "GET", "/api/v1/users?cursor=***", "", 400, codeInvalid, "cursor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantError(t, f.do(tt.method, tt.target, tt.body, admin), tt.status, tt.code, tt.msg)
		})
	}
}

// TestUsersAPIAuthorization checks that users are the admin role's (docs/spec/http-routing.md
// §4): anonymous is 401, a user without it 403, also for a user that does not exist, and a
// credential that may not manage (a scoped personal access token, a deploy token) 403.
func TestUsersAPIAuthorization(t *testing.T) {
	scoped := auth.Credential{Kind: auth.CredPAT, TokenID: 1, UserID: 1, Scopes: []string{"read", "write"}}
	deploy := auth.Credential{Kind: auth.CredDeploy, TokenID: 20, Repo: "raw/scratch", Scopes: []string{"read", "write"}}
	tests := []struct {
		name           string
		authz          bool
		p              auth.Principal
		c              auth.Credential
		method, target string
		body           string
		status         int
	}{
		{"admin lists", true, admin, auth.Credential{Kind: auth.CredPassword}, "GET", "/api/v1/users", "", 200},
		{"admin creates", true, admin, auth.Credential{Kind: auth.CredPassword}, "POST", "/api/v1/users", `{"name": "carol", "password": "correct horse"}`, 201},
		{"anonymous lists", true, auth.Anonymous, auth.Credential{}, "GET", "/api/v1/users", "", 401},
		{"anonymous reads", true, auth.Anonymous, auth.Credential{}, "GET", "/api/v1/users/alice", "", 401},
		{"a user lists", true, alice, auth.Credential{Kind: auth.CredPassword}, "GET", "/api/v1/users", "", 403},
		{"a user reads itself", true, alice, auth.Credential{Kind: auth.CredPassword}, "GET", "/api/v1/users/alice", "", 403},
		{"a user sets its password", true, alice, auth.Credential{Kind: auth.CredPassword}, "POST", "/api/v1/users/alice/password", `{"password": "battery staple"}`, 403},
		{"a user reads one that does not exist", true, bob, auth.Credential{Kind: auth.CredPassword}, "GET", "/api/v1/users/dave", "", 403},
		{"a user creates", true, bob, auth.Credential{Kind: auth.CredPassword}, "POST", "/api/v1/users", `{"name": "x"}`, 403},
		{"a scoped pat", false, alice, scoped, "GET", "/api/v1/users", "", 403},
		{"a deploy token", false, auth.Principal{Kind: auth.KindToken, Name: "20"}, deploy, "DELETE", "/api/v1/users/alice", "", 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := Options{}
			if tt.authz {
				o.Auth = fakeAuth{grants: grants}
			}
			f := newFixture(t, o)
			f.addUsers(t, "admin", "alice", "bob")
			wantStatus(t, f.doAs(tt.method, tt.target, tt.body, tt.p, tt.c), tt.status)
		})
	}
}

// wantPasswordSet sets the password of the user called name through the API and checks that
// the stored hash changed to one of password.
func (f *fixture) wantPasswordSet(t *testing.T, name, password string) {
	t.Helper()
	before, err := f.st.GetUser(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users/"+name+"/password", `{"password": "`+password+`"}`, admin), http.StatusNoContent)
	after, err := f.st.GetUser(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := auth.VerifyPassword(after.PasswordHash, password); after.PasswordHash == before.PasswordHash || !ok || err != nil {
		t.Error("POST …/password did not set the new password")
	}
}

// wantUserAudit checks the audit of TestUsersAPI: one event of each change of the user of uid,
// from the api, and two creations.
func (f *fixture) wantUserAudit(t *testing.T, uid string) {
	t.Helper()
	for _, action := range []string{auth.ActionUserRename, auth.ActionUserDisable, auth.ActionUserPassword, auth.ActionUserDelete} {
		if ev := oneAudit(t, f.st, action); ev.Subject != "user:"+uid || ev.details["source"] != SourceAPI {
			t.Errorf("%s: subject %q source %v, want user:%s from the api", action, ev.Subject, ev.details["source"], uid)
		}
	}
	if n := len(auditOf(t, f.st, auth.ActionUserCreate)); n != 2 {
		t.Errorf("%d user.create events, want 2", n)
	}
}

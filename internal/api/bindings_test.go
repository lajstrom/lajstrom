package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// TestBindingsAPI creates bindings by a user's name and by its uid, lists them, a page at a
// time and by principal, and deletes one, with an audit event of each change.
func TestBindingsAPI(t *testing.T) {
	f := newFixture(t, Options{})
	f.addUsers(t, "alice", "bob")
	aliceUID, bobUID := f.uidOf(t, "alice"), f.uidOf(t, "bob")
	var b auth.Binding
	rr := f.do(http.MethodPost, "/api/v1/bindings", `{"principal": "user:alice", "role": "writer", "repoMask": "raw/*"}`, admin)
	decodeJSON(t, rr, http.StatusCreated, &b)
	if loc := rr.Header().Get("Location"); loc != fmt.Sprintf("/api/v1/bindings/%d", b.ID) {
		t.Errorf("Location = %q, want /api/v1/bindings/%d", loc, b.ID)
	}
	wantJSON(t, "the created binding", b, fmt.Sprintf(`{"id": %d, "principal": "user:%s", "name": "alice", "role": "writer",
		"repoMask": "raw/*", "createdAt": "2026-01-02T12:00:00Z"}`, b.ID, aliceUID))
	for _, body := range []string{
		`{"principal": "user:` + aliceUID + `", "role": "reader", "repoMask": "*/payments"}`,
		`{"principal": "user:bob", "role": "maintainer", "repoMask": "pypi/internal"}`,
	} {
		wantStatus(t, f.do(http.MethodPost, "/api/v1/bindings", body, admin), http.StatusCreated)
	}

	var ids []int64
	target := "/api/v1/bindings?limit=2"
	for target != "" {
		var list BindingList
		decodeJSON(t, f.do(http.MethodGet, target, "", admin), http.StatusOK, &list)
		for _, b := range list.Bindings {
			ids = append(ids, b.ID)
		}
		target = ""
		if list.Next != "" {
			target = "/api/v1/bindings?limit=2&cursor=" + list.Next
		}
	}
	if want := []int64{b.ID, b.ID + 1, b.ID + 2}; fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Errorf("the pages hold the ids %v, want %v", ids, want)
	}
	for _, q := range []string{"user:alice", "user:" + aliceUID} {
		var list BindingList
		decodeJSON(t, f.do(http.MethodGet, "/api/v1/bindings?principal="+q, "", admin), http.StatusOK, &list)
		if len(list.Bindings) != 2 || list.Bindings[0].Name != "alice" || list.Bindings[1].Principal != "user:"+aliceUID {
			t.Errorf("?principal=%s: %+v, want alice's two", q, list.Bindings)
		}
	}

	wantStatus(t, f.do(http.MethodDelete, fmt.Sprintf("/api/v1/bindings/%d", b.ID+2), "", admin), http.StatusNoContent)
	var bobs BindingList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/bindings?principal=user:bob", "", admin), http.StatusOK, &bobs)
	if len(bobs.Bindings) != 0 {
		t.Errorf("bob's bindings after the delete: %+v", bobs.Bindings)
	}
	if ev := oneAudit(t, f.st, auth.ActionBindingDelete); ev.Subject != "user:"+bobUID || ev.details["source"] != SourceAPI {
		t.Errorf("binding.delete: subject %q source %v, want user:%s from the api", ev.Subject, ev.details["source"], bobUID)
	}
	if n := len(auditOf(t, f.st, auth.ActionBindingCreate)); n != 3 {
		t.Errorf("%d binding.create events, want 3", n)
	}
}

// TestBindingsAPIRefused checks the answers to what the bindings API refuses.
func TestBindingsAPIRefused(t *testing.T) {
	f := newFixture(t, Options{})
	f.addUsers(t, "alice")
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users", `{"name": "root", "password": "correct horse", "admin": true}`, admin), http.StatusCreated)
	var root BindingList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/bindings?principal=user:root", "", admin), http.StatusOK, &root)
	if len(root.Bindings) != 1 {
		t.Fatalf("root's bindings: %+v, want its admin binding", root.Bindings)
	}
	tests := []struct {
		name, method, target, body string
		status                     int
		code, msg                  string
	}{
		{"a user that does not exist", "POST", "/api/v1/bindings", `{"principal": "user:dave", "role": "reader", "repoMask": "raw/*"}`, 400, codeInvalid, "user dave does not exist"},
		{"a deploy token", "POST", "/api/v1/bindings", `{"principal": "token:3", "role": "reader", "repoMask": "raw/*"}`, 400, codeInvalid, "principal"},
		{"a role that is not one", "POST", "/api/v1/bindings", `{"principal": "user:alice", "role": "owner", "repoMask": "raw/*"}`, 400, codeInvalid, "role"},
		{"an admin binding on a repo", "POST", "/api/v1/bindings", `{"principal": "user:alice", "role": "admin", "repoMask": "raw/*"}`, 400, codeInvalid, "*/*"},
		{"a mask of an unknown format", "POST", "/api/v1/bindings", `{"principal": "user:alice", "role": "reader", "repoMask": "npm/*"}`, 400, codeInvalid, "repoMask"},
		{"an unknown field", "POST", "/api/v1/bindings", `{"principal": "user:alice", "role": "reader", "mask": "raw/*"}`, 400, codeInvalid, "mask"},
		{"the same binding again", "POST", "/api/v1/bindings", `{"principal": "user:root", "role": "admin", "repoMask": "*/*"}`, 409, codeConflict, "exists already"},
		{"the last admin binding", "DELETE", fmt.Sprintf("/api/v1/bindings/%d", root.Bindings[0].ID), "", 409, codeConflict, "last admin binding"},
		{"a binding that does not exist", "DELETE", "/api/v1/bindings/999", "", 404, codeNotFound, "999"},
		{"an id that is not one", "DELETE", "/api/v1/bindings/x", "", 400, codeInvalid, "id"},
		{"a cursor that is not one", "GET", "/api/v1/bindings?cursor=eA", "", 400, codeInvalid, "cursor"},
		{"a principal that is not one", "GET", "/api/v1/bindings?principal=alice", "", 400, codeInvalid, "principal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantError(t, f.do(tt.method, tt.target, tt.body, admin), tt.status, tt.code, tt.msg)
		})
	}
}

// TestBindingsAPIAuthorization checks that bindings are the admin role's: anonymous is 401,
// anyone else 403, a credential that may not manage 403.
func TestBindingsAPIAuthorization(t *testing.T) {
	scoped := auth.Credential{Kind: auth.CredPAT, TokenID: 1, UserID: 1, Scopes: []string{"read", "write"}}
	body := `{"principal": "user:alice", "role": "reader", "repoMask": "raw/*"}`
	tests := []struct {
		name           string
		authz          bool
		p              auth.Principal
		c              auth.Credential
		method, target string
		body           string
		status         int
	}{
		{"admin lists", true, admin, auth.Credential{Kind: auth.CredPassword}, "GET", "/api/v1/bindings", "", 200},
		{"admin creates", true, admin, auth.Credential{Kind: auth.CredPassword}, "POST", "/api/v1/bindings", body, 201},
		{"anonymous lists", true, auth.Anonymous, auth.Credential{}, "GET", "/api/v1/bindings", "", 401},
		{"a user lists its own", true, alice, auth.Credential{Kind: auth.CredPassword}, "GET", "/api/v1/bindings?principal=user:alice", "", 403},
		{"a user creates", true, alice, auth.Credential{Kind: auth.CredPassword}, "POST", "/api/v1/bindings", body, 403},
		{"a user deletes", true, bob, auth.Credential{Kind: auth.CredPassword}, "DELETE", "/api/v1/bindings/1", "", 403},
		{"a scoped pat", false, admin, scoped, "POST", "/api/v1/bindings", body, 403},
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

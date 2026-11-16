package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// TestServiceAccountsAPI goes through a service account's life over the API (docs/spec/rbac.md
// §1): created with an owner and no password, listed by kind, given a token that never expires
// without a bound; its events in the audit log say principalKind service.
func TestServiceAccountsAPI(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{})
	f.addUsers(t, "alice")
	aliceUID := f.uidOf(t, "alice")
	var robot auth.User
	rr := f.do(http.MethodPost, "/api/v1/users", `{"name": "robot", "kind": "service", "owner": "user:alice"}`, admin)
	decodeJSON(t, rr, http.StatusCreated, &robot)
	if loc := rr.Header().Get("Location"); loc != "/api/v1/users/robot" {
		t.Errorf("Location = %q, want /api/v1/users/robot", loc)
	}
	wantJSON(t, "the service account", robot, `{"uid": "`+robot.UID+`", "name": "robot", "kind": "service", "disabled": false,
		"createdAt": "2026-01-02T12:00:00Z", "owner": "user:`+aliceUID+`", "ownerName": "alice"}`)
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users", `{"name": "deployer", "kind": "service", "owner": "group:ops"}`, admin), http.StatusCreated)

	for kind, want := range map[string]string{"service": `["deployer", "robot"]`, "local": `["alice"]`, "": `["alice", "deployer", "robot"]`} {
		var list UserList
		decodeJSON(t, f.do(http.MethodGet, "/api/v1/users?kind="+kind, "", admin), http.StatusOK, &list)
		names := []string{}
		for _, u := range list.Users {
			names = append(names, u.Name)
		}
		wantJSON(t, "the users of kind "+kind, names, want)
	}

	var tok created
	decodeJSON(t, f.doAs(http.MethodPost, "/api/v1/users/robot/tokens", `{"name": "ci", "scopes": ["read"]}`, admin,
		auth.Credential{Kind: auth.CredPassword}), http.StatusCreated, &tok)
	if tok.ExpiresAt != nil || tok.User != "robot" {
		t.Errorf("robot's token %+v, want robot's, never expiring", tok.Token)
	}

	if err := f.st.AppendAudit(ctx, &index.AuditEvent{Principal: "user:" + robot.UID, Action: "push", Repo: "raw/scratch", Subject: "a.txt"}); err != nil {
		t.Fatal(err)
	}
	var page AuditList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/audit?action=push", "", admin), http.StatusOK, &page)
	if len(page.Events) != 1 || page.Events[0].PrincipalKind != "service" || page.Events[0].PrincipalName != "robot" {
		t.Errorf("robot's push in the audit log: %+v, want principalKind service", page.Events)
	}
	var created AuditList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/audit?action=user.create", "", admin), http.StatusOK, &created)
	if len(created.Events) != 2 || created.Events[0].PrincipalKind != "" {
		t.Errorf("the admin's user.create in the audit log: %+v, want no principalKind", created.Events)
	}
}

// TestServiceAccountsAPIOwner: a disabled owner is the flag ownerDisabled; the owner's deletion
// is 409 until the account has another owner, a user.owner event.
func TestServiceAccountsAPIOwner(t *testing.T) {
	f := newFixture(t, Options{})
	f.addUsers(t, "alice")
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users", `{"name": "robot", "kind": "service", "owner": "user:alice"}`, admin), http.StatusCreated)
	wantStatus(t, f.do(http.MethodPatch, "/api/v1/users/alice", `{"disabled": true}`, admin), http.StatusOK)
	var got auth.User
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/users/robot", "", admin), http.StatusOK, &got)
	if !got.OwnerDisabled || got.Disabled {
		t.Errorf("robot with alice disabled: %+v, want ownerDisabled and robot enabled", got)
	}
	wantError(t, f.do(http.MethodDelete, "/api/v1/users/alice", "", admin), http.StatusConflict, codeConflict, "owns the service accounts robot")
	var moved auth.User
	decodeJSON(t, f.do(http.MethodPatch, "/api/v1/users/robot", `{"owner": "group:ops"}`, admin), http.StatusOK, &moved)
	if moved.Owner != "group:ops" || moved.OwnerName != "" || moved.OwnerDisabled {
		t.Errorf("robot after its owner changed: %+v, want group:ops", moved)
	}
	wantStatus(t, f.do(http.MethodDelete, "/api/v1/users/alice", "", admin), http.StatusNoContent)
	var owner AuditList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/audit?action=user.owner", "", admin), http.StatusOK, &owner)
	if len(owner.Events) != 1 {
		t.Errorf("user.owner in the audit log: %+v, want one", owner.Events)
	}
}

// TestServiceAccountsAPIRefused checks what the API refuses of a service account.
func TestServiceAccountsAPIRefused(t *testing.T) {
	f := newFixture(t, Options{})
	f.addUsers(t, "alice")
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users", `{"name": "robot", "kind": "service", "owner": "user:alice"}`, admin), http.StatusCreated)
	tests := []struct {
		name, method, target, body string
		status                     int
		code, msg                  string
	}{
		{"a password", "POST", "/api/v1/users", `{"name": "bot", "kind": "service", "owner": "user:alice", "password": "correct horse"}`, 400, codeInvalid, "password: a service account has no password"},
		{"admin", "POST", "/api/v1/users", `{"name": "bot", "kind": "service", "owner": "user:alice", "admin": true}`, 400, codeInvalid, "admin: "},
		{"no owner", "POST", "/api/v1/users", `{"name": "bot", "kind": "service"}`, 400, codeInvalid, "owner: "},
		{"an owner that does not exist", "POST", "/api/v1/users", `{"name": "bot", "kind": "service", "owner": "user:nobody"}`, 400, codeInvalid, "owner: user nobody does not exist"},
		{"a service account as owner", "POST", "/api/v1/users", `{"name": "bot", "kind": "service", "owner": "user:robot"}`, 400, codeInvalid, "owner: robot is a service account"},
		{"an owner of a local user", "POST", "/api/v1/users", `{"name": "bob", "password": "correct horse", "owner": "user:alice"}`, 400, codeInvalid, "owner: only a service account"},
		{"an oidc user", "POST", "/api/v1/users", `{"name": "bob", "kind": "oidc"}`, 400, codeInvalid, "kind: "},
		{"a password of a service account", "POST", "/api/v1/users/robot/password", `{"password": "correct horse"}`, 400, codeInvalid, "service account"},
		{"an owner of a local user patched", "PATCH", "/api/v1/users/alice", `{"owner": "group:ops"}`, 400, codeInvalid, "not a service account"},
		{"an owner that is a token", "PATCH", "/api/v1/users/robot", `{"owner": "token:1"}`, 400, codeInvalid, "owner: "},
		{"an unknown kind", "GET", "/api/v1/users?kind=robot", "", 400, codeInvalid, "kind: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantError(t, f.do(tt.method, tt.target, tt.body, admin), tt.status, tt.code, tt.msg)
		})
	}
}

// TestServiceAccountsAPITokenBound: with auth.serviceAccounts.maxTokenTTL, a service account's
// token created without an expiry gets the bound, and one asked to expire later is 400.
func TestServiceAccountsAPITokenBound(t *testing.T) {
	f := newFixture(t, Options{ServiceTokenMaxLifetime: 720 * time.Hour})
	f.addUsers(t, "alice")
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users", `{"name": "robot", "kind": "service", "owner": "user:alice"}`, admin), http.StatusCreated)
	password := auth.Credential{Kind: auth.CredPassword}
	var tok created
	decodeJSON(t, f.doAs(http.MethodPost, "/api/v1/users/robot/tokens", `{"name": "ci"}`, admin, password), http.StatusCreated, &tok)
	if tok.ExpiresAt == nil || !tok.ExpiresAt.Equal(testNow.Add(720*time.Hour)) {
		t.Errorf("robot's token expires at %v, want 720h from now", tok.ExpiresAt)
	}
	wantError(t, f.doAs(http.MethodPost, "/api/v1/users/robot/tokens", `{"name": "long", "expiresIn": "60d"}`, admin, password),
		http.StatusBadRequest, codeInvalid, "auth.serviceAccounts.maxTokenTTL")
	wantError(t, f.doAs(http.MethodPost, "/api/v1/users/robot/tokens", `{"name": "minted"}`, admin,
		auth.Credential{Kind: auth.CredPAT, TokenID: 1}), http.StatusForbidden, codeForbidden, "password")
}

// TestServiceAccountsAPIDisableWithOwner: with auth.serviceAccounts.onOwnerDisabled disable,
// disabling the owner over the API disables its service account, and enabling the owner leaves
// the account disabled.
func TestServiceAccountsAPIDisableWithOwner(t *testing.T) {
	f := newFixture(t, Options{ServiceDisableWithOwner: true})
	f.addUsers(t, "alice")
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users", `{"name": "robot", "kind": "service", "owner": "user:alice"}`, admin), http.StatusCreated)
	wantStatus(t, f.do(http.MethodPatch, "/api/v1/users/alice", `{"disabled": true}`, admin), http.StatusOK)
	wantStatus(t, f.do(http.MethodPatch, "/api/v1/users/alice", `{"disabled": false}`, admin), http.StatusOK)
	var got auth.User
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/users/robot", "", admin), http.StatusOK, &got)
	if !got.Disabled || got.OwnerDisabled {
		t.Errorf("robot after alice disabled and enabled: %+v, want robot disabled, its owner not", got)
	}
}

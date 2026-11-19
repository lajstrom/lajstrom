package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// approvalFixture is the API with the users admin and root, each with the admin role, and alice,
// who reads raw/open; the formats raw and pypi.
func approvalFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, Options{})
	f.addUsers(t, "admin", "root", "alice")
	for _, name := range []string{"admin", "root"} {
		must(t, f.st.CreateBinding(context.Background(), &index.Binding{Principal: "user:" + f.uidOf(t, name), Role: auth.RoleAdmin,
			RepoMask: auth.MaskAll}))
	}
	f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log, Auth: fakeAuth{grants: approvalGrants}}))
	return f
}

// approvalGrants are the grants of approvalFixture: admin and root manage everything, and so does
// robot, a service account when a test makes it one; alice reads raw/open.
var approvalGrants = map[string]bool{"user:admin manage */*": true, "user:root manage */*": true, "user:robot manage */*": true,
	"user:alice read raw/open": true}

var (
	pass = auth.Credential{Kind: auth.CredPassword}
	// freshMFA is the token of a web UI's sign-in a minute old by the tests' clock, with a second
	// factor.
	freshMFA = auth.Credential{Kind: auth.CredPAT, TokenID: 9, SignIn: &auth.SignIn{At: testNow.Add(-time.Minute), AMR: []string{"pwd", "otp"}}}
)

// requestBinding makes, through the service, the request of the binding (principal, role, */*)
// by the user called by with the admin's password.
func (f *fixture) requestBinding(t *testing.T, by, principal, role string) *auth.Approval {
	t.Helper()
	return f.requestBindingWith(t, by, pass, principal, role)
}

// requestBindingWith is requestBinding with the credential c, which the service does not check:
// the API does (auth.Approvals.MayRequest).
func (f *fixture) requestBindingWith(t *testing.T, by string, c auth.Credential, principal, role string) *auth.Approval {
	t.Helper()
	u, err := f.st.GetUser(context.Background(), by)
	must(t, err)
	s := &auth.Approvals{Index: f.st, Now: f.repos.Now, Bindings: &auth.Bindings{Index: f.st, Formats: f.repos.Formats},
		Tokens: &auth.Tokens{Index: f.st, Formats: f.repos.Formats, Now: f.repos.Now}}
	a, err := s.RequestBinding(auth.WithCredential(auth.WithPrincipal(context.Background(), auth.UserPrincipal(u)), c), principal, role,
		auth.MaskAll, Origin{Source: SourceAPI})
	must(t, err)
	return a
}

// TestApprovalList: the requests are listed newest first, a page at a time, of a state, and read
// one by one; another state is 400, an id that is none 404. The admin role first: anyone else is
// 403 on every route of the requests, whatever the id.
func TestApprovalList(t *testing.T) {
	f := approvalFixture(t)
	ids := []int64{f.requestBinding(t, "admin", "user:alice", auth.RoleAdmin).ID,
		f.requestBinding(t, "admin", "group:ops", auth.RoleReader).ID, f.requestBinding(t, "root", "group:dev", auth.RoleWriter).ID}
	var first, last, approved ApprovalList
	decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/approvals?limit=2", "", root, pass), http.StatusOK, &first)
	decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/approvals?limit=2&cursor="+first.Next, "", root, pass), http.StatusOK, &last)
	got := slices.Concat(first.Approvals, last.Approvals)
	if len(got) != 3 || got[0].ID != ids[2] || got[1].ID != ids[1] || got[2].ID != ids[0] || first.Next == "" || last.Next != "" {
		t.Fatalf("the pages: %+v, %+v", first, last)
	}
	decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/approvals?state=approved", "", root, pass), http.StatusOK, &approved)
	if len(approved.Approvals) != 0 {
		t.Errorf("approved: %+v", approved)
	}
	wantStatus(t, f.doAs(http.MethodGet, "/api/v1/approvals?state=waiting", "", root, pass), http.StatusBadRequest)
	var one auth.Approval
	decodeJSON(t, f.doAs(http.MethodGet, fmt.Sprintf("/api/v1/approvals/%d", ids[0]), "", root, pass), http.StatusOK, &one)
	if one.ID != ids[0] || one.Kind != index.ApprovalBinding || one.State != index.ApprovalPending {
		t.Errorf("GET of a request: %+v", one)
	}
	for _, target := range []string{"/api/v1/approvals/999", "/api/v1/approvals/x", "/api/v1/approvals/0"} {
		wantStatus(t, f.doAs(http.MethodGet, target, "", root, pass), http.StatusNotFound)
	}
	for _, tt := range []struct{ method, target string }{{http.MethodGet, "/api/v1/approvals"}, {http.MethodGet, "/api/v1/approvals/999"},
		{http.MethodPost, "/api/v1/approvals/999/approve"}, {http.MethodPost, fmt.Sprintf("/api/v1/approvals/%d/reject", ids[0])}} {
		wantStatus(t, f.doAs(tt.method, tt.target, "", alice, pass), http.StatusForbidden)
	}
}

// TestApprovalDecisions: an approval takes the admin role, then the admin's password or a web UI's
// fresh sign-in (403 for a token, 401 asking for more for a sign-in that is not fresh), both
// before the request is looked up; the requester is 403; an approval makes the change; a request
// decided is 409. A rejection takes the role with any credential, the requester's being a
// withdrawal; each takes its reason as ?changeReason=.
func TestApprovalDecisions(t *testing.T) {
	f := approvalFixture(t)
	a, b := f.requestBinding(t, "admin", "user:alice", auth.RoleAdmin), f.requestBinding(t, "admin", "group:ops", auth.RoleAdmin)
	approve := fmt.Sprintf("/api/v1/approvals/%d/approve", a.ID)
	stale := auth.Credential{Kind: auth.CredPAT, TokenID: 9, SignIn: &auth.SignIn{At: testNow.Add(-time.Hour), AMR: []string{"pwd", "otp"}}}
	for _, tt := range []struct {
		name   string
		target string
		p      auth.Principal
		c      auth.Credential
		status int
		msg    string
	}{
		{"a token, before the lookup", "/api/v1/approvals/999/approve", root, auth.Credential{Kind: auth.CredPAT, TokenID: 7}, 403,
			"not with a token"},
		{"a stale sign-in, before the lookup", "/api/v1/approvals/999/approve", root, stale, 401, "within the last 10 minutes"},
		{"the requester", approve, admin, pass, 403, "another admin does"},
		{"no request", "/api/v1/approvals/999/approve", root, pass, 404, "does not exist"},
		{"a reason out of bounds", approve + "?changeReason=", root, pass, 400, "changeReason: empty"},
	} {
		wantError(t, f.doAs(http.MethodPost, tt.target, "", tt.p, tt.c), tt.status, codeOf(tt.status), tt.msg)
	}
	var done auth.Approval
	decodeJSON(t, f.doAs(http.MethodPost, approve+"?changeReason=CHG-3", "", root, freshMFA), http.StatusOK, &done)
	if done.State != index.ApprovalApproved || done.DecisionReason != "CHG-3" || done.DecidedBy != "user:"+f.uidOf(t, "root") {
		t.Errorf("the approval: %+v", done)
	}
	if bs, _ := f.st.ListBindings(context.Background(), "user:"+f.uidOf(t, "alice")); len(bs) != 1 || bs[0].Role != auth.RoleAdmin {
		t.Errorf("alice's bindings after the approval: %+v", bs)
	}
	wantError(t, f.doAs(http.MethodPost, approve, "", root, pass), http.StatusConflict, codeConflict, "it was decided")
	wantError(t, f.doAs(http.MethodPost, fmt.Sprintf("/api/v1/approvals/%d/reject", a.ID), "", root, pass), http.StatusConflict,
		codeConflict, "it was decided")
	var withdrawn auth.Approval
	decodeJSON(t, f.doAs(http.MethodPost, fmt.Sprintf("/api/v1/approvals/%d/reject?changeReason=withdrawn", b.ID), "", admin,
		auth.Credential{Kind: auth.CredPAT, TokenID: 3}), http.StatusOK, &withdrawn)
	if withdrawn.State != index.ApprovalRejected || withdrawn.DecidedBy != "user:"+f.uidOf(t, "admin") || withdrawn.DecisionReason != "withdrawn" {
		t.Errorf("the withdrawal: %+v", withdrawn)
	}
}

// TestApprovalAdminMode: under auth.admin.requireMFA, an approval needs a fresh sign-in with a
// second factor: an admin's password is 403 naming the setting, a fresh sign-in with one factor
// is asked for more, and one with a second factor approves.
func TestApprovalAdminMode(t *testing.T) {
	f := adminModeFixture(t)
	a := f.requestBinding(t, "root", "user:alice", auth.RoleAdmin)
	ops := auth.Principal{Kind: auth.KindUser, Name: "ops"}
	approve := fmt.Sprintf("/api/v1/approvals/%d/approve", a.ID)
	oneFactor := auth.Credential{Kind: auth.CredPAT, TokenID: 8, SignIn: &auth.SignIn{At: testNow.Add(-time.Minute), AMR: []string{"pwd"}}}
	wantError(t, f.doAs(http.MethodPost, approve, "", ops, pass), http.StatusForbidden, codeForbidden, "auth.admin.requireMFA")
	wantError(t, f.doAs(http.MethodPost, approve, "", ops, oneFactor), http.StatusUnauthorized, codeUnauthenticated, "second factor")
	wantStatus(t, f.doAs(http.MethodPost, approve, "", ops, freshMFA), http.StatusOK)
}

// secondAdmin is f with auth.approval second-admin when second is true, and the info that says it.
func (f *fixture) secondAdmin(second bool) *fixture {
	f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log, SecondAdmin: second,
		Info: ServerInfo{Approval: map[bool]string{true: "second-admin", false: "none"}[second]}, Auth: fakeAuth{grants: approvalGrants}}))
	return f
}

// request posts body to target as admin and checks the 202 of a change that waits for a second
// admin, its Location and its pending request, which it returns.
func (f *fixture) request(t *testing.T, target, body string) CreatedApproval {
	t.Helper()
	rr := f.doAs(http.MethodPost, target, body, admin, pass)
	var a CreatedApproval
	decodeJSON(t, rr, http.StatusAccepted, &a)
	if loc := rr.Header().Get("Location"); loc != fmt.Sprintf("/api/v1/approvals/%d", a.ID) || a.State != index.ApprovalPending {
		t.Errorf("%s: Location %q, request %+v", target, loc, a)
	}
	return a
}

// TestApprovalRequests: with auth.approval second-admin, a binding of the role admin or on */*
// and a deploy token over a mask with write answer 202 with a request and its Location, the token
// with its secret, which authenticates once the request is approved; the other changes are made
// at once. The same change twice is 409.
func TestApprovalRequests(t *testing.T) {
	f := approvalFixture(t).secondAdmin(true)
	bind := f.request(t, "/api/v1/bindings", `{"principal": "user:alice", "role": "admin", "repoMask": "*/*", "changeReason": "CHG-1"}`)
	f.request(t, "/api/v1/bindings", `{"principal": "group:ops", "role": "reader", "repoMask": "*/*"}`)
	tok := f.request(t, "/api/v1/deploy-tokens", `{"repoMask": "raw/*", "name": "fleet", "scopes": ["read", "write"], "expiresIn": "30d"}`)
	if bind.Kind != "binding" || bind.ChangeReason != "CHG-1" || bind.Secret != "" {
		t.Errorf("the binding's request: %+v", bind)
	}
	if tok.Kind != "deploy-token" || !strings.HasPrefix(tok.Secret, auth.DeployPrefix) || strings.Contains(string(tok.Change), "hash") {
		t.Errorf("the token's request: %+v", tok)
	}
	if bs, _ := f.st.ListBindings(context.Background(), ""); len(bs) != 2 {
		t.Errorf("bindings after the requests: %+v, want the admins' only", bs)
	}
	wantStatus(t, f.doAs(http.MethodPost, "/api/v1/bindings", `{"principal": "user:alice", "role": "reader", "repoMask": "raw/*"}`, admin,
		pass), http.StatusCreated)
	wantStatus(t, f.doAs(http.MethodPost, "/api/v1/deploy-tokens", `{"repoMask": "raw/*", "name": "reads", "scopes": ["read"]}`, admin,
		pass), http.StatusCreated)
	wantError(t, f.doAs(http.MethodPost, "/api/v1/bindings", `{"principal": "user:alice", "role": "admin", "repoMask": "*/*"}`, root,
		pass), http.StatusConflict, codeConflict, "waits for a second admin already")
	wantStatus(t, f.doAs(http.MethodPost, "/api/v1/deploy-tokens", `{"repoMask": "raw/*", "name": "x", "scopes": ["write"]}`, admin,
		auth.Credential{Kind: auth.CredPAT, TokenID: 3}), http.StatusForbidden)

	authn := auth.NewAuthenticator(auth.Options{Index: f.st, Now: func() time.Time { return testNow }})
	if _, _, err := authn.AuthenticateBearer(context.Background(), tok.Secret); err == nil {
		t.Error("the secret of a request that waits authenticates")
	}
	var done auth.Approval
	decodeJSON(t, f.doAs(http.MethodPost, fmt.Sprintf("/api/v1/approvals/%d/approve", tok.ID), "", root, pass), http.StatusOK, &done)
	if p, _, err := authn.AuthenticateBearer(context.Background(), tok.Secret); err != nil || "token:"+p.Name != done.Result {
		t.Errorf("the secret of the approved request: %v, %v; want %s", p, err, done.Result)
	}
}

// TestApprovalAdminUser: with auth.approval second-admin, POST /api/v1/users with admin is 400
// naming the setting and POST /api/v1/bindings, and makes neither the user, nor its binding, nor a
// request; a user without admin is made at once. With none, admin makes the user and its admin
// binding at once, as before (#1376).
func TestApprovalAdminUser(t *testing.T) {
	ctx := context.Background()
	body := `{"name": "second", "password": "a long password", "admin": true}`
	f := approvalFixture(t).secondAdmin(true)
	wantError(t, f.doAs(http.MethodPost, "/api/v1/users", body, admin, pass), http.StatusBadRequest, codeInvalid,
		"auth.approval second-admin", "POST /api/v1/bindings")
	if _, err := f.st.GetUser(ctx, "second"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the user after the refusal: %v, want none", err)
	}
	if bs, _ := f.st.ListBindings(ctx, ""); len(bs) != 2 {
		t.Errorf("bindings after the refusal: %+v, want the admins' only", bs)
	}
	var waiting ApprovalList
	decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/approvals", "", root, pass), http.StatusOK, &waiting)
	if len(waiting.Approvals) != 0 {
		t.Errorf("requests after the refusal: %+v, want none", waiting.Approvals)
	}
	wantStatus(t, f.doAs(http.MethodPost, "/api/v1/users", `{"name": "carol", "password": "a long password"}`, admin, pass),
		http.StatusCreated)

	f.secondAdmin(false)
	wantStatus(t, f.doAs(http.MethodPost, "/api/v1/users", body, admin, pass), http.StatusCreated)
	if bs, _ := f.st.ListBindings(ctx, "user:"+f.uidOf(t, "second")); len(bs) != 1 || bs[0].Role != auth.RoleAdmin ||
		bs[0].RepoMask != auth.MaskAll {
		t.Errorf("the bindings of the admin made with none: %+v, want admin on */*", bs)
	}
}

// TestApprovalNone: with auth.approval none, an admin binding is made at once, with a token too,
// and a request made before is still decided.
func TestApprovalNone(t *testing.T) {
	f := approvalFixture(t).secondAdmin(true)
	a := f.request(t, "/api/v1/bindings", `{"principal": "group:ops", "role": "admin", "repoMask": "*/*"}`)
	f.secondAdmin(false)
	wantStatus(t, f.doAs(http.MethodPost, "/api/v1/bindings", `{"principal": "group:dev", "role": "admin", "repoMask": "*/*"}`, admin, pass),
		http.StatusCreated)
	wantStatus(t, f.doAs(http.MethodPost, "/api/v1/bindings", `{"principal": "group:qa", "role": "admin", "repoMask": "*/*"}`, admin,
		auth.Credential{Kind: auth.CredPAT, TokenID: 3}), http.StatusCreated)
	wantStatus(t, f.doAs(http.MethodPost, fmt.Sprintf("/api/v1/approvals/%d/approve", a.ID), "", root, pass), http.StatusOK)
	if bs, _ := f.st.ListBindings(context.Background(), "group:ops"); len(bs) != 1 {
		t.Errorf("the binding of the request of before: %+v", bs)
	}
}

// TestApprovalRequestCredential: with auth.approval second-admin, a binding that waits is
// requested with the admin's password or a web UI's fresh sign-in, as its approval is (#1393):
// a token is 403, the admin's own, one made for another admin and a service account's alike, and
// a sign-in that is not fresh 401 asking for more, each before the reason and with no request and
// no row. A binding that does not wait is made with a token, as before. The approval.request and
// approval.approve rows have the credential of each side. A service account's request, made on a
// release before, is 409 at its approval and stays pending.
func TestApprovalRequestCredential(t *testing.T) {
	ctx := context.Background()
	f := approvalFixture(t).secondAdmin(true)
	robot := index.User{Name: "robot", Kind: auth.KindService, Owner: "user:" + f.uidOf(t, "root")}
	must(t, f.st.CreateUser(ctx, &robot))
	must(t, f.st.CreateBinding(ctx, &index.Binding{Principal: auth.UserPrincipal(&robot).String(), Role: auth.RoleAdmin,
		RepoMask: auth.MaskAll}))
	robotP := auth.Principal{Kind: auth.KindUser, Name: "robot"}
	pat := auth.Credential{Kind: auth.CredPAT, TokenID: 3}
	stale := auth.Credential{Kind: auth.CredPAT, TokenID: 9, SignIn: &auth.SignIn{At: testNow.Add(-time.Hour), AMR: []string{"pwd", "otp"}}}
	adminBinding := `{"principal": "user:alice", "role": "admin", "repoMask": "*/*", "changeReason": "CHG-1"}`
	for _, tt := range []struct {
		name   string
		body   string
		p      auth.Principal
		c      auth.Credential
		status int
		msg    string
	}{
		{"the admin's own token", adminBinding, admin, pat, 403, "not with a token"},
		{"a token made for another admin", adminBinding, root, auth.Credential{Kind: auth.CredPAT, TokenID: 11}, 403, "not with a token"},
		{"a service account's token", adminBinding, robotP, auth.Credential{Kind: auth.CredPAT, TokenID: 12}, 403, "not with a token"},
		{"a token, a binding on */*", `{"principal": "group:ops", "role": "reader", "repoMask": "*/*"}`, admin, pat, 403, "not with a token"},
		{"a token, before the reason", `{"principal": "user:alice", "role": "admin", "repoMask": "*/*", "changeReason": ""}`, admin, pat, 403,
			"not with a token"},
		{"a stale sign-in", adminBinding, admin, stale, 401, "within the last 10 minutes"},
	} {
		wantError(t, f.doAs(http.MethodPost, "/api/v1/bindings", tt.body, tt.p, tt.c), tt.status, codeOf(tt.status), tt.msg)
	}
	if as, err := f.st.ListApprovals(ctx, "", 0, 10); err != nil || len(as) != 0 {
		t.Errorf("requests after the refusals: %+v, %v; want none", as, err)
	}
	if rows := auditOf(t, f.st, auth.ActionApprovalRequest); len(rows) != 0 {
		t.Errorf("approval.request rows after the refusals: %+v", rows)
	}
	wantStatus(t, f.doAs(http.MethodPost, "/api/v1/bindings", `{"principal": "user:alice", "role": "writer", "repoMask": "raw/*"}`, admin,
		pat), http.StatusCreated)

	var byPassword, bySignIn CreatedApproval
	decodeJSON(t, f.doAs(http.MethodPost, "/api/v1/bindings", adminBinding, admin, pass), http.StatusAccepted, &byPassword)
	decodeJSON(t, f.doAs(http.MethodPost, "/api/v1/bindings", `{"principal": "group:ops", "role": "admin", "repoMask": "*/*"}`, root,
		freshMFA), http.StatusAccepted, &bySignIn)
	credentials := map[string]any{}
	for _, row := range auditOf(t, f.st, auth.ActionApprovalRequest) {
		credentials[row.Subject] = row.details["credential"]
	}
	want := map[string]any{fmt.Sprintf("approval:%d", byPassword.ID): "password", fmt.Sprintf("approval:%d", bySignIn.ID): "token:9"}
	if !reflect.DeepEqual(credentials, want) {
		t.Errorf("the credentials of the approval.request rows: %v, want %v", credentials, want)
	}
	wantStatus(t, f.doAs(http.MethodPost, fmt.Sprintf("/api/v1/approvals/%d/approve", byPassword.ID), "", root, freshMFA), http.StatusOK)
	if row := oneAudit(t, f.st, auth.ActionApprovalApprove); row.details["credential"] != "token:9" {
		t.Errorf("approval.approve: %v, want the credential token:9", row.details)
	}

	// A request as a release before #1393 made it, with the service account's token.
	old := f.requestBindingWith(t, "robot", auth.Credential{Kind: auth.CredPAT, TokenID: 12}, "group:dev", auth.RoleAdmin)
	approve := fmt.Sprintf("/api/v1/approvals/%d/approve", old.ID)
	wantError(t, f.doAs(http.MethodPost, approve, "", root, pass), http.StatusConflict, codeConflict,
		"its requester robot is a service account")
	var still auth.Approval
	decodeJSON(t, f.doAs(http.MethodGet, fmt.Sprintf("/api/v1/approvals/%d", old.ID), "", root, pass), http.StatusOK, &still)
	if still.State != index.ApprovalPending {
		t.Errorf("the service account's request after the refusal: %+v, want it pending", still)
	}
}

// TestApprovalAdminPassword: with auth.approval second-admin, an admin's new password for a user
// who holds admin is 400 naming the setting, with no row and the password as it was (#1393):
// another admin's, a disabled admin's, one through a group whose membership has not lapsed, and
// the user's own without currentPassword. A user without admin, by a lapsed membership too, gets
// one as before, and so does an admin's own change with currentPassword. An unlock, mustChange
// and enabling a disabled admin are as before. With none, an admin sets another admin's password.
func TestApprovalAdminPassword(t *testing.T) {
	ctx := context.Background()
	f := approvalFixture(t)
	users := &auth.Users{Index: f.st, Now: f.repos.Now, Log: f.repos.Log}
	cli := auth.Origin{Source: SourceCLI}
	f.addUsers(t, "dora", "carol", "erin")
	for _, name := range []string{"root", "dora", "carol"} {
		must(t, users.SetPassword(ctx, name, name+" password 1", cli))
	}
	must(t, f.st.CreateBinding(ctx, &index.Binding{Principal: "user:" + f.uidOf(t, "dora"), Role: auth.RoleAdmin, RepoMask: auth.MaskAll}))
	must(t, users.Disable(ctx, "dora", cli))
	must(t, f.st.CreateBinding(ctx, &index.Binding{Principal: "group:ops", Role: auth.RoleAdmin, RepoMask: auth.MaskAll}))
	for name, until := range map[string]time.Time{"carol": testNow.Add(time.Hour), "erin": testNow.Add(-time.Minute)} {
		id := index.Identity{UserID: f.idOf(t, name), Issuer: "https://idp.example", Subject: name}
		must(t, f.st.CreateIdentity(ctx, &id))
		must(t, f.st.SetIdentityGroups(ctx, id.ID, []string{"ops"}, testNow.Add(-time.Hour), until))
	}
	serve := func(second bool) {
		f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log, SecondAdmin: second, Users: users,
			Auth: fakeAuth{grants: approvalGrants}, Passwords: auth.NewAuthenticator(auth.Options{Index: f.st, Log: f.repos.Log, Users: users})}))
	}
	serve(true)
	hashes := func() map[string]string {
		m := map[string]string{}
		for _, name := range []string{"root", "dora", "carol"} {
			u, err := f.st.GetUser(ctx, name)
			must(t, err)
			m[name] = u.PasswordHash
		}
		return m
	}
	before, rows := hashes(), len(auditOf(t, f.st, auth.ActionUserPassword))
	for _, tt := range []struct {
		name, user string
		p          auth.Principal
		c          auth.Credential
	}{
		{"another admin's", "root", admin, pass},
		{"another admin's, by its uid", f.uidOf(t, "root"), admin, pass},
		{"a disabled admin's", "dora", admin, pass},
		{"an admin's through a group", "carol", admin, pass},
		{"its own, without currentPassword", "root", root, pass},
		{"with a token made for the user", "root", root, auth.Credential{Kind: auth.CredPAT, TokenID: 11}},
	} {
		wantError(t, f.doAs(http.MethodPost, "/api/v1/users/"+tt.user+"/password", `{"password": "set by another 2"}`, tt.p, tt.c),
			http.StatusBadRequest, codeInvalid, "auth.approval second-admin", "holds admin", "currentPassword",
			"lajstrom admin user set-password")
	}
	if n := len(auditOf(t, f.st, auth.ActionUserPassword)); n != rows {
		t.Errorf("user.password rows after the refusals: %d, want %d", n, rows)
	}
	if after := hashes(); !reflect.DeepEqual(after, before) {
		t.Error("a refusal changed a password")
	}
	for _, name := range []string{"alice", "erin"} {
		wantStatus(t, f.doAs(http.MethodPost, "/api/v1/users/"+name+"/password", `{"password": "set by an admin 3"}`, admin, pass),
			http.StatusNoContent)
	}
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users/root/password", `{"currentPassword": "root password 1", "password": "root's own 4"}`,
		auth.Anonymous), http.StatusNoContent)
	if u, err := f.st.GetUser(ctx, "root"); err != nil || u.PasswordHash == before["root"] {
		t.Errorf("root after its own change: %v, the hash unchanged %v", err, err == nil && u.PasswordHash == before["root"])
	}
	wantStatus(t, f.doAs(http.MethodPost, "/api/v1/users/root/unlock", "", admin, pass), http.StatusNoContent)
	wantStatus(t, f.doAs(http.MethodPatch, "/api/v1/users/root", `{"mustChange": true}`, admin, pass), http.StatusOK)
	wantStatus(t, f.doAs(http.MethodPatch, "/api/v1/users/dora", `{"disabled": false}`, admin, pass), http.StatusOK)

	serve(false)
	wantStatus(t, f.doAs(http.MethodPost, "/api/v1/users/root/password", `{"password": "set by another 5"}`, admin, pass),
		http.StatusNoContent)
}

// idOf returns the id of the user called name.
func (f *fixture) idOf(t *testing.T, name string) int64 {
	t.Helper()
	u, err := f.st.GetUser(context.Background(), name)
	must(t, err)
	return u.ID
}

// TestApprovalInfo: GET /api/v1/info answers auth.approval as approval.
func TestApprovalInfo(t *testing.T) {
	for _, second := range []bool{true, false} {
		f := approvalFixture(t).secondAdmin(second)
		var info map[string]any
		decodeJSON(t, f.do(http.MethodGet, "/api/v1/info", "", auth.Anonymous), http.StatusOK, &info)
		if want := map[bool]string{true: "second-admin", false: "none"}[second]; info["approval"] != want {
			t.Errorf("approval %v, want %s", info["approval"], want)
		}
	}
}

// TestInfoPendingApprovals: GET /api/v1/info answers pendingApprovals to a caller whose
// credential reaches the admin role: the requests pending, not expired, and not its own, whatever
// auth.approval says; to no other caller, an admin's scoped token included (docs/spec/rbac.md
// §13, Telling the second admin; #1336).
func TestInfoPendingApprovals(t *testing.T) {
	f := approvalFixture(t)
	f.requestBinding(t, "admin", "user:alice", auth.RoleAdmin)
	f.requestBinding(t, "admin", "group:ops", auth.RoleReader)
	f.requestBinding(t, "root", "group:dev", auth.RoleWriter)
	rejected := f.requestBinding(t, "root", "group:qa", auth.RoleAdmin)
	wantStatus(t, f.doAs(http.MethodPost, fmt.Sprintf("/api/v1/approvals/%d/reject", rejected.ID), "", root, pass), http.StatusOK)
	must(t, f.st.CreateApproval(context.Background(), &index.Approval{Kind: index.ApprovalBinding, Subject: "group:old admin */*",
		Change: []byte(`{"principal":"group:old","role":"admin","repoMask":"*/*"}`), RequestedBy: "user:" + f.uidOf(t, "admin"),
		RequestedAt: testNow.Add(-8 * 24 * time.Hour), ExpiresAt: testNow.Add(-time.Hour)})) // expired, not yet marked
	scoped := auth.Credential{Kind: auth.CredPAT, TokenID: 1, Scopes: []string{"read", "write"}}
	for _, tt := range []struct {
		name string
		p    auth.Principal
		c    auth.Credential
		want int // -1: absent
	}{
		{"root, of admin's two", root, pass, 2},
		{"admin, of root's one", admin, pass, 1},
		{"the admin's scoped token", admin, scoped, -1},
		{"a user without the role", alice, pass, -1},
		{"anonymous", auth.Anonymous, auth.Credential{}, -1},
	} {
		var got map[string]any
		decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/info", "", tt.p, tt.c), http.StatusOK, &got)
		n, ok := got["pendingApprovals"]
		switch {
		case tt.want < 0 && ok:
			t.Errorf("%s: pendingApprovals %v, want none", tt.name, n)
		case tt.want >= 0 && n != float64(tt.want):
			t.Errorf("%s: pendingApprovals %v, want %d", tt.name, n, tt.want)
		}
	}
	f.secondAdmin(false)
	var got Info
	decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/info", "", root, pass), http.StatusOK, &got)
	if got.PendingApprovals == nil || *got.PendingApprovals != 2 {
		t.Errorf("with auth.approval none: pendingApprovals %v, want the 2 still pending", got.PendingApprovals)
	}
}

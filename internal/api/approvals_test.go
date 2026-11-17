package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
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
	f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log,
		Auth: fakeAuth{grants: map[string]bool{"user:admin manage */*": true, "user:root manage */*": true, "user:alice read raw/open": true}}}))
	return f
}

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
	u, err := f.st.GetUser(context.Background(), by)
	must(t, err)
	s := &auth.Approvals{Index: f.st, Now: f.repos.Now, Bindings: &auth.Bindings{Index: f.st, Formats: f.repos.Formats},
		Tokens: &auth.Tokens{Index: f.st, Formats: f.repos.Formats, Now: f.repos.Now}}
	a, err := s.RequestBinding(auth.WithCredential(auth.WithPrincipal(context.Background(), auth.UserPrincipal(u)), pass), principal, role,
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

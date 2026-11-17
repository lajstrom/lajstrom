package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestAdminMode is the evaluation of docs/spec/rbac.md §12: under auth.admin.requireMFA an admin
// binding, a user's own or a group's, counts only for a sign-in with a second factor; a sign-in
// without one, a password, a personal access token and a service account's token keep the other
// bindings, on the system and on every repo. Off, the admin role is as before.
func TestAdminMode(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	id, alice, st := groupFixture(t, now.Add(time.Hour))
	if err := st.SetIdentityGroups(ctx, id.ID, []string{"admins", "dev"}, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	addRepo(t, st, "raw", "public", true)
	bind(t, st, "user:root", RoleAdmin, "*/*")
	bind(t, st, "user:ops", RoleAdmin, "*/*")
	bind(t, st, "user:ops", RoleMaintainer, "*/*")
	on := NewRBAC(RBACOptions{Index: st, AnonymousRead: true, AdminMFA: true})
	off := NewRBAC(RBACOptions{Index: st, AnonymousRead: true})
	mfa := Credential{Kind: CredPAT, TokenID: 30, SignIn: &SignIn{At: now, AMR: []string{"pwd", "otp"}}}
	noAuthTime := Credential{Kind: CredPAT, TokenID: 31, SignIn: &SignIn{AMR: []string{"mfa"}}}
	oneFactor := Credential{Kind: CredPAT, TokenID: 32, SignIn: &SignIn{At: now, AMR: []string{"pwd"}}}
	gold := Credential{Kind: CredPAT, TokenID: 33, SignIn: &SignIn{At: now, AMR: []string{"pwd"}, ACR: "gold", MFA: MFA{ACR: []string{"gold"}}}}
	ua := UserPrincipal(alice)
	sys := System + "/" + System
	for _, tc := range []struct {
		name string
		r    *RBAC
		p    Principal
		c    Credential
		a    Action
		repo string
		want bool
	}{
		{"a sign-in with a second factor: the system", on, user("root"), mfa, ActionManage, sys, true},
		{"a sign-in with a second factor: every repo", on, user("root"), mfa, ActionDelete, "pypi/internal", true},
		{"the acr of auth.oidc.mfa", on, user("root"), gold, ActionManage, sys, true},
		{"no freshness: a sign-in without auth_time", on, user("root"), noAuthTime, ActionManage, sys, true},
		{"a sign-in with one factor: not the system", on, user("root"), oneFactor, ActionManage, sys, false},
		{"a sign-in with one factor: no private repo", on, user("root"), oneFactor, ActionRead, "raw/private", false},
		{"a sign-in with one factor: what anonymous reads", on, user("root"), oneFactor, ActionRead, "raw/public", true},
		{"a password", on, user("root"), password, ActionManage, sys, false},
		{"a personal access token", on, user("root"), patAll, ActionWrite, "raw/private", false},
		{"the command line's none", on, user("root"), Credential{}, ActionManage, sys, false},
		{"another binding counts", on, user("ops"), patAll, ActionWrite, "pypi/internal", true},
		{"another binding is no admin", on, user("ops"), patAll, ActionManage, sys, false},
		{"a group's admin, with a second factor", on, ua, mfa, ActionManage, sys, true},
		{"a group's admin, with a password", on, ua, password, ActionManage, sys, false},
		{"a group's other binding", on, ua, password, ActionRead, "raw/private", true},
		{"off: a password", off, user("root"), password, ActionManage, sys, true},
		{"off: a group's admin", off, ua, patAll, ActionDelete, "pypi/internal", true},
	} {
		got, err := tc.r.Allowed(WithEvaluationScope(WithCredential(ctx, tc.c)), tc.p, tc.a, strings.Split(tc.repo, "/")[0],
			strings.Split(tc.repo, "/")[1])
		if err != nil || got != tc.want {
			t.Errorf("%s: Allowed(%s, %s, %s) = %v, %v; want %v", tc.name, tc.p, tc.a, tc.repo, got, err, tc.want)
		}
	}
	checkAdminLeftOut(t, on, off, ua)
}

// checkAdminLeftOut checks RBAC.AdminLeftOut on the index of TestAdminMode: the principals with an
// admin binding, their own or a group's, that the credential does not reach under the mode.
func checkAdminLeftOut(t *testing.T, on, off *RBAC, ua Principal) {
	t.Helper()
	mfa := Credential{Kind: CredPAT, SignIn: &SignIn{At: time.Now(), AMR: []string{"mfa"}}}
	for _, tc := range []struct {
		name string
		r    *RBAC
		p    Principal
		c    Credential
		want bool
	}{
		{"a password", on, user("root"), password, true},
		{"a personal access token", on, user("ops"), patAll, true},
		{"a group's admin binding", on, ua, password, true},
		{"a sign-in with a second factor", on, user("root"), mfa, false},
		{"no admin binding", on, user("bob"), password, false},
		{"a deploy token", on, token7, deploy7, false},
		{"the mode off", off, user("root"), password, false},
	} {
		got, err := tc.r.AdminLeftOut(WithCredential(context.Background(), tc.c), tc.p)
		if err != nil || got != tc.want {
			t.Errorf("AdminLeftOut, %s: %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

// TestAdminMFARefusal: a route of the admin role asks a web UI's sign-in without a second factor
// for more (a step-up without max_age: the role needs no fresh sign-in), and refuses every other
// credential of an admin with ErrScope; both name auth.admin.requireMFA, the second the kind of
// the credential.
func TestAdminMFARefusal(t *testing.T) {
	now := time.Now()
	err := AdminMFARefusal(Credential{Kind: CredPAT, SignIn: &SignIn{At: now.Add(-time.Hour), AMR: []string{"pwd"}}}, now)
	var stepUp *StepUpError
	if !errors.As(err, &stepUp) || stepUp.Missing != "a second factor" || stepUp.MaxAge != 0 ||
		!strings.Contains(err.Error(), "(auth.admin.requireMFA): this needs a sign-in with a second factor, and this sign-in's amr (pwd)") {
		t.Errorf("a sign-in with one factor: %#v", err)
	}
	for kind, cred := range map[string]Credential{"a password": password, "a personal access token": patAll, "none": {}} {
		err := AdminMFARefusal(cred, now)
		if !errors.Is(err, ErrScope) || errors.As(err, &stepUp) || !strings.Contains(err.Error(), "auth.admin.requireMFA") ||
			!strings.Contains(err.Error(), "credential is "+kind) {
			t.Errorf("%s: %v", kind, err)
		}
	}
}

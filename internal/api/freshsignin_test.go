package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// TestFreshSignInInPlaceOfPassword: POST /api/v1/users/<name>/tokens and POST
// /api/v1/deploy-tokens take a web UI's fresh sign-in through OIDC where they take a password
// (docs/spec/rbac.md §12, #1323): a person's own token, a service account's and a deploy token
// over a mask by the admin; the token made has createdBy token:<id>. A sign-in that is not fresh
// is asked for more, 401 with max_age=600; any other token is 403. Under the admin mode, the
// admin role needs the sign-in's second factor too (401 without max_age), a person's own token
// does not; without it, the admin's sign-in with one factor is enough.
func TestFreshSignInInPlaceOfPassword(t *testing.T) {
	f := adminModeFixture(t)
	root, alice := auth.Principal{Kind: auth.KindUser, Name: "root"}, auth.Principal{Kind: auth.KindUser, Name: "alice"}
	signed := func(ago time.Duration, amr ...string) auth.Credential {
		return auth.Credential{Kind: auth.CredPAT, TokenID: 8, SignIn: &auth.SignIn{At: testNow.Add(-ago), AMR: amr}}
	}
	fresh, freshMFA, stale := signed(time.Minute, "pwd"), signed(time.Minute, "pwd", "otp"), signed(time.Hour, "pwd", "otp")
	if rr := f.doAs(http.MethodPost, "/api/v1/users", `{"name": "robot", "kind": "service", "owner": "user:root"}`, root, freshMFA); rr.Code != http.StatusCreated {
		t.Fatalf("a service account: %d %s", rr.Code, rr.Body)
	}
	const mask = `{"repoMask": "raw/*", "name": "%s", "scopes": ["read"]}`
	const fresh600 = `DPoP error="insufficient_user_authentication", error_description="a fresh sign-in", max_age=600, algs="ES256"`
	const factor = `DPoP error="insufficient_user_authentication", error_description="a second factor", algs="ES256"`
	for _, tt := range []struct {
		name           string
		p              auth.Principal
		c              auth.Credential
		target, body   string
		status         int
		challenge, msg string
	}{
		{"a person's own, one factor", alice, fresh, "/api/v1/users/alice/tokens", `{"name": "laptop"}`, http.StatusCreated, "", ""},
		{"a person's own, stale", alice, stale, "/api/v1/users/alice/tokens", `{"name": "old"}`, http.StatusUnauthorized, fresh600,
			"a token in place of a password: this needs a sign-in within the last 10 minutes"},
		{"a person's own, with a token", alice, auth.Credential{Kind: auth.CredPAT, TokenID: 9}, "/api/v1/users/alice/tokens",
			`{"name": "x"}`, http.StatusForbidden, "", "fresh sign-in through OIDC, not with a token"},
		{"a service account's, with a second factor", root, freshMFA, "/api/v1/users/robot/tokens", `{"name": "ci", "expiresIn": "30d"}`,
			http.StatusCreated, "", ""},
		{"a service account's, one factor", root, fresh, "/api/v1/users/robot/tokens", `{"name": "ci2"}`, http.StatusUnauthorized,
			factor, "(auth.admin.requireMFA)"},
		{"over a mask, with a second factor", root, freshMFA, "/api/v1/deploy-tokens", fmt.Sprintf(mask, "nodes"), http.StatusCreated, "", ""},
		{"over a mask, stale", root, stale, "/api/v1/deploy-tokens", fmt.Sprintf(mask, "y"), http.StatusUnauthorized, fresh600, "in place of a password"},
		{"over a mask, one factor", root, fresh, "/api/v1/deploy-tokens", fmt.Sprintf(mask, "z"), http.StatusUnauthorized, factor, "(auth.admin.requireMFA)"},
	} {
		rr := f.doAs(http.MethodPost, tt.target, tt.body, tt.p, tt.c)
		challenge := rr.Header().Get("WWW-Authenticate")
		if tt.status == http.StatusCreated {
			wantStatus(t, rr, tt.status)
		} else {
			wantError(t, rr, tt.status, codeOf(tt.status), tt.msg)
		}
		if challenge != tt.challenge {
			t.Errorf("%s: WWW-Authenticate %q, want %q", tt.name, challenge, tt.challenge)
		}
	}
	rows := auditOf(t, f.st, auth.ActionTokenCreate)
	if len(rows) != 3 {
		t.Fatalf("token.create rows: %+v", rows)
	}
	for _, r := range rows {
		in, _ := r.details["signIn"].(map[string]any)
		if r.details["createdBy"] != "token:8" || in["token"] != float64(8) {
			t.Errorf("a token.create row: %v", r.details)
		}
	}
	// Without the admin mode, the admin's fresh sign-in with one factor stands for the password.
	f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log, Auth: auth.NewRBAC(auth.RBACOptions{Index: f.st, Log: f.repos.Log})}))
	wantStatus(t, f.doAs(http.MethodPost, "/api/v1/deploy-tokens", fmt.Sprintf(mask, "plain"), root, fresh), http.StatusCreated)
}

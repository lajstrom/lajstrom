package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// adminModeFixture is a fixture whose handler has the real evaluator under the admin mode
// (auth.admin.requireMFA, docs/spec/rbac.md §12) and answers adminMFA: root has the admin role,
// ops the admin role and maintainer on */*, alice reader on raw/*; the repo raw/open exists.
func adminModeFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, Options{})
	f.addUsers(t, "root", "ops", "alice")
	f.create(t, "raw", "open")
	for _, b := range [][3]string{{"root", auth.RoleAdmin, "*/*"}, {"ops", auth.RoleAdmin, "*/*"}, {"ops", auth.RoleMaintainer, "*/*"},
		{"alice", auth.RoleReader, "raw/*"}} {
		must(t, f.st.CreateBinding(context.Background(), &index.Binding{Principal: "user:" + f.uidOf(t, b[0]), Role: b[1], RepoMask: b[2]}))
	}
	f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log, Info: ServerInfo{AdminMFA: true},
		Auth: auth.NewRBAC(auth.RBACOptions{Index: f.st, Log: f.repos.Log, AdminMFA: true})}))
	return f
}

// TestAdminModeAnswers: under the admin mode, a route of the admin role answers a web UI's
// sign-in through OIDC without a second factor 401 with RFC 9470's challenge and no max_age,
// every other credential of an admin 403 naming auth.admin.requireMFA and its kind, a principal
// without an admin binding the 403 of §3; a sign-in with a second factor is the admin. A repo
// route answers by the bindings that count: an admin's other binding, or the 404 of a repo that
// does not exist.
func TestAdminModeAnswers(t *testing.T) {
	f := adminModeFixture(t)
	root := auth.Principal{Kind: auth.KindUser, Name: "root"}
	ops := auth.Principal{Kind: auth.KindUser, Name: "ops"}
	mfa := auth.Credential{Kind: auth.CredPAT, TokenID: 5, SignIn: &auth.SignIn{At: time.Now().Add(-time.Hour), AMR: []string{"pwd", "otp"}}}
	oneFactor := auth.Credential{Kind: auth.CredPAT, TokenID: 6, SignIn: &auth.SignIn{At: time.Now(), AMR: []string{"pwd"}}}
	password := auth.Credential{Kind: auth.CredPassword}
	pat := auth.Credential{Kind: auth.CredPAT, TokenID: 7}
	for _, tt := range []struct {
		name           string
		p              auth.Principal
		c              auth.Credential
		method, target string
		status         int
		code, msg      string
	}{
		{"a sign-in with a second factor", root, mfa, http.MethodGet, "/api/v1/users", http.StatusOK, "", ""},
		{"a sign-in with one factor", root, oneFactor, http.MethodGet, "/api/v1/users", http.StatusUnauthorized, codeUnauthenticated,
			"(auth.admin.requireMFA): this needs a sign-in with a second factor, and this sign-in's amr (pwd) shows no second factor"},
		{"a password", root, password, http.MethodGet, "/api/v1/bindings", http.StatusForbidden, codeForbidden,
			"(auth.admin.requireMFA): this needs a sign-in with a second factor, and the request's credential is a password"},
		{"a personal access token", ops, pat, http.MethodPost, "/api/v1/repos", http.StatusForbidden, codeForbidden,
			"the request's credential is a personal access token"},
		{"no admin binding", auth.Principal{Kind: auth.KindUser, Name: "alice"}, oneFactor, http.MethodGet, "/api/v1/users",
			http.StatusForbidden, codeForbidden, "manage on the system"},
		{"an admin's other binding", ops, pat, http.MethodPatch, "/api/v1/repos/raw/open", http.StatusOK, "", ""},
		{"no other binding: a repo that does not exist", root, password, http.MethodGet, "/api/v1/repos/raw/open",
			http.StatusNotFound, codeNotFound, "repository raw/open"},
	} {
		rr := f.doAs(tt.method, tt.target, `{"anonymousRead": false}`, tt.p, tt.c)
		challenge := rr.Header().Get("WWW-Authenticate")
		if tt.code == "" {
			wantStatus(t, rr, tt.status)
		} else {
			wantError(t, rr, tt.status, tt.code, tt.msg)
		}
		want := ""
		if tt.status == http.StatusUnauthorized {
			want = `DPoP error="insufficient_user_authentication", error_description="a second factor", algs="ES256"`
		}
		if challenge != want {
			t.Errorf("%s: WWW-Authenticate %q, want %q", tt.name, challenge, want)
		}
	}
}

// TestAdminModeInfo: GET /api/v1/info answers adminMFA under the mode, and the caller's
// adminNeedsMFA when its admin binding is left out for the request's credential: not for the
// admin with a second factor, nor for a principal without an admin binding.
func TestAdminModeInfo(t *testing.T) {
	f := adminModeFixture(t)
	mfa := auth.Credential{Kind: auth.CredPAT, SignIn: &auth.SignIn{AMR: []string{"mfa"}}}
	for _, tt := range []struct {
		name       string
		p          string
		c          auth.Credential
		admin, mfa bool
	}{
		{"a password", "root", auth.Credential{Kind: auth.CredPassword}, false, true},
		{"a sign-in with a second factor", "root", mfa, true, false},
		{"no admin binding", "alice", auth.Credential{Kind: auth.CredPassword}, false, false},
	} {
		var info struct {
			AdminMFA *bool `json:"adminMFA"`
			Caller   struct {
				Admin         bool  `json:"admin"`
				AdminNeedsMFA *bool `json:"adminNeedsMFA"`
			} `json:"caller"`
		}
		rr := f.doAs(http.MethodGet, "/api/v1/info", "", auth.Principal{Kind: auth.KindUser, Name: tt.p}, tt.c)
		decodeJSON(t, rr, http.StatusOK, &info)
		if info.AdminMFA == nil || !*info.AdminMFA || info.Caller.Admin != tt.admin || (info.Caller.AdminNeedsMFA != nil) != tt.mfa {
			raw, _ := json.Marshal(info)
			t.Errorf("%s: %s", tt.name, raw)
		}
	}
	var info map[string]any
	decodeJSON(t, newFixture(t, Options{}).do(http.MethodGet, "/api/v1/info", "", auth.Anonymous), http.StatusOK, &info)
	if _, ok := info["adminMFA"]; ok {
		t.Errorf("adminMFA without the mode: %v", info)
	}
}

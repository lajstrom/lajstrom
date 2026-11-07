package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// fakeAuth allows what grants lists, as "<principal> <action> <format>/<repo>"; a grant on */*
// matches every repo, as the admin binding does. err, when set, is the answer to everything.
type fakeAuth struct {
	grants map[string]bool
	err    error
}

func (f fakeAuth) Allowed(_ context.Context, p auth.Principal, a auth.Action, formatName, repo string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	key := p.String() + " " + string(a) + " "
	return f.grants[key+formatName+"/"+repo] || f.grants[key+"*/*"], nil
}

var (
	admin = auth.Principal{Kind: auth.KindUser, Name: "admin"}
	alice = auth.Principal{Kind: auth.KindUser, Name: "alice"} // reads raw/open
	bob   = auth.Principal{Kind: auth.KindUser, Name: "bob"}   // nothing
)

// grants are the bindings of the authorization tests: admin manages and reads everything,
// alice reads raw/open.
var grants = map[string]bool{
	"user:admin manage */*":    true,
	"user:admin read */*":      true,
	"user:alice read raw/open": true,
}

func TestAuthorization(t *testing.T) {
	tests := []struct {
		name           string
		p              auth.Principal
		method, target string
		body           string
		status         int
		code           string // "" for a success
	}{
		{"admin creates", admin, http.MethodPost, "/api/v1/repos", scratch, http.StatusCreated, ""},
		{"anonymous creates", auth.Anonymous, http.MethodPost, "/api/v1/repos", scratch, http.StatusUnauthorized, codeForbidden},
		{"reader creates", alice, http.MethodPost, "/api/v1/repos", scratch, http.StatusForbidden, codeForbidden},
		{"stranger creates", bob, http.MethodPost, "/api/v1/repos", scratch, http.StatusForbidden, codeForbidden},
		{"anonymous gets", auth.Anonymous, http.MethodGet, "/api/v1/repos/raw/open", "", http.StatusUnauthorized, codeForbidden},
		{"anonymous gets an absent repo", auth.Anonymous, http.MethodGet, "/api/v1/repos/raw/absent", "", http.StatusUnauthorized, codeForbidden},
		{"stranger gets", bob, http.MethodGet, "/api/v1/repos/raw/open", "", http.StatusNotFound, codeNotFound},
		{"reader gets", alice, http.MethodGet, "/api/v1/repos/raw/open", "", http.StatusOK, ""},
		{"reader gets another repo", alice, http.MethodGet, "/api/v1/repos/raw/secret", "", http.StatusNotFound, codeNotFound},
		{"reader patches", alice, http.MethodPatch, "/api/v1/repos/raw/open", `{"anonymousRead":true}`, http.StatusForbidden, codeForbidden},
		{"reader deletes", alice, http.MethodDelete, "/api/v1/repos/raw/open", "", http.StatusForbidden, codeForbidden},
		{"reader reindexes", alice, http.MethodPost, "/api/v1/repos/raw/open/reindex", "", http.StatusForbidden, codeForbidden},
		{"stranger deletes", bob, http.MethodDelete, "/api/v1/repos/raw/open", "", http.StatusNotFound, codeNotFound},
		{"anonymous deletes", auth.Anonymous, http.MethodDelete, "/api/v1/repos/raw/open", "", http.StatusUnauthorized, codeForbidden},
		{"admin patches", admin, http.MethodPatch, "/api/v1/repos/raw/open", `{"anonymousRead":true}`, http.StatusOK, ""},
		{"admin deletes", admin, http.MethodDelete, "/api/v1/repos/raw/secret", "", http.StatusNoContent, ""},
		{"admin reindexes", admin, http.MethodPost, "/api/v1/repos/raw/open/reindex", "", http.StatusNotImplemented, codeUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, Options{Auth: fakeAuth{grants: grants}})
			f.create(t, "raw", "open")
			f.create(t, "raw", "secret")
			rr := f.do(tt.method, tt.target, tt.body, tt.p)
			if tt.code == "" {
				wantStatus(t, rr, tt.status)
			} else {
				wantError(t, rr, tt.status, tt.code)
			}
			wantChallenge(t, rr)
		})
	}
}

// wantChallenge checks that rr has the Basic challenge exactly when it is a 401.
func wantChallenge(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	want := ""
	if rr.Code == http.StatusUnauthorized {
		want = basicChallenge
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != want {
		t.Errorf("status %d with WWW-Authenticate %q, want %q", rr.Code, got, want)
	}
}

func TestListFiltered(t *testing.T) {
	f := newFixture(t, Options{Auth: fakeAuth{grants: grants}})
	f.create(t, "raw", "open")
	f.create(t, "raw", "secret")
	for _, tt := range []struct {
		p    auth.Principal
		want []string
	}{
		{admin, []string{"raw/open", "raw/secret"}},
		{alice, []string{"raw/open"}},
		{bob, []string{}},
		{auth.Anonymous, []string{}},
	} {
		var got RepoList
		decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos", "", tt.p), http.StatusOK, &got)
		if ids := repoIDs(got.Repos); strings.Join(ids, " ") != strings.Join(tt.want, " ") {
			t.Errorf("%s lists %v, want %v", tt.p, ids, tt.want)
		}
	}
}

// An Authorizer that cannot decide is a 500, never a denial.
func TestAuthorizerFailure(t *testing.T) {
	f := newFixture(t, Options{Auth: fakeAuth{err: errors.New("bindings: database is gone")}})
	f.create(t, "raw", "open")
	for _, target := range []string{"/api/v1/repos", "/api/v1/repos/raw/open"} {
		wantError(t, f.do(http.MethodGet, target, "", alice), http.StatusInternalServerError, codeInternal, "Internal Server Error")
	}
}

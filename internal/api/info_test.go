package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/server"
	"github.com/lajstrom/lajstrom/internal/version"
)

// namedPlugin is a format plugin that has a name and an artifact type, and nothing else.
type namedPlugin struct {
	format.Plugin
	name string
}

func (p namedPlugin) Name() string         { return p.name }
func (p namedPlugin) ArtifactType() string { return "application/vnd.lajstrom." + p.name + ".v1" }

// testInfo is the ServerInfo of the info tests.
var testInfo = ServerInfo{ExternalURL: "https://art.example.com", AnonymousRead: true,
	Formats: FormatsOf([]format.Plugin{namedPlugin{name: "raw"}, namedPlugin{name: "apt"}})}

// TestFormatsOf checks that the formats are the plugins' names and artifact types, by name.
func TestFormatsOf(t *testing.T) {
	want := []FormatInfo{{"apt", "application/vnd.lajstrom.apt.v1"}, {"raw", "application/vnd.lajstrom.raw.v1"}}
	if !slices.Equal(testInfo.Formats, want) {
		t.Errorf("FormatsOf = %+v, want %+v", testInfo.Formats, want)
	}
	if got := FormatsOf(nil); got == nil || len(got) != 0 {
		t.Errorf("FormatsOf(nil) = %#v, want an empty list", got)
	}
}

// TestInfo checks GET /api/v1/info: the build, the facts of Options.Info and the auth modes to
// every principal, the anonymous one too, and the caller with admin when its credential reaches
// the admin role, which a personal access token with scopes and a deploy token do not.
func TestInfo(t *testing.T) {
	password := auth.Credential{Kind: auth.CredPassword}
	scoped := auth.Credential{Kind: auth.CredPAT, TokenID: 1, UserID: 1, Scopes: []string{"read", "write"}}
	deploy := auth.Credential{Kind: auth.CredDeploy, TokenID: 42, RepoID: 1, Repo: "raw/open", Scopes: []string{"read"}}
	tests := []struct {
		name string
		p    auth.Principal
		c    auth.Credential
		want Caller // Principal and UID filled in for a user
	}{
		{"anonymous", auth.Anonymous, auth.Credential{}, Caller{Whoami: Whoami{Principal: "anonymous", Kind: "anonymous", Name: "anonymous"}}},
		{"a user", alice, password, Caller{Whoami: Whoami{Kind: "user", Name: "alice"}}},
		{"a user without any grant", bob, password, Caller{Whoami: Whoami{Kind: "user", Name: "bob"}}},
		{"the admin", admin, password, Caller{Whoami: Whoami{Kind: "user", Name: "admin"}, Admin: true}},
		{"the admin's scoped token", admin, scoped, Caller{Whoami: Whoami{Kind: "user", Name: "admin"}}},
		{"a deploy token", auth.Principal{Kind: auth.KindToken, Name: "42"}, deploy,
			Caller{Whoami: Whoami{Principal: "token:42", Kind: "token", Name: "42"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, Options{Auth: fakeAuth{grants: grants}, Info: testInfo})
			f.addUsers(t, "admin", "alice", "bob")
			if tt.p.Kind == auth.KindUser {
				tt.want.UID = f.uidOf(t, tt.p.Name)
				tt.want.Principal = "user:" + tt.want.UID
			}
			var got Info
			decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/info", "", tt.p, tt.c), http.StatusOK, &got)
			b := version.Get()
			if got.Version != b.Version || got.Commit != b.Commit || got.Date != b.Date {
				t.Errorf("build = %q %q %q, want %+v", got.Version, got.Commit, got.Date, b)
			}
			if got.ExternalURL != testInfo.ExternalURL || got.AnonymousRead != testInfo.AnonymousRead ||
				!slices.Equal(got.Formats, testInfo.Formats) {
				t.Errorf("info = %+v, want the facts of %+v", got, testInfo)
			}
			if !slices.Equal(got.AuthModes, []string{"password", "token"}) {
				t.Errorf("authModes = %q, want password and token", got.AuthModes)
			}
			if got.Caller != tt.want {
				t.Errorf("caller = %+v, want %+v", got.Caller, tt.want)
			}
		})
	}
}

// TestInfoZero checks the answer of a handler without Options.Info: lists, never null.
func TestInfoZero(t *testing.T) {
	f := newFixture(t, Options{})
	rr := f.do(http.MethodGet, "/api/v1/info", "", auth.Anonymous)
	wantStatus(t, rr, http.StatusOK)
	for _, want := range []string{`"externalURL":""`, `"anonymousRead":false`, `"formats":[]`, `"admin":false`} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("body %s does not contain %s", rr.Body, want)
		}
	}
}

// TestInfoThroughServer runs info behind the server's middleware: without credentials it is 200
// with the anonymous caller, with a password the user, and with a wrong password 401. A
// preflight from another origin gets no CORS header (docs/spec/http-routing.md §4, same origin
// only): OPTIONS is 404, as is every method a route does not have.
func TestInfoThroughServer(t *testing.T) {
	f := newFixture(t, Options{Info: testInfo})
	users := &auth.Users{Index: f.st}
	if _, err := users.Create(context.Background(), "alice", "alice-password", false, auth.Origin{Source: SourceCLI}); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Options{Listen: "127.0.0.1:0", API: f.h, Log: f.repos.Log,
		Authenticator: auth.NewAuthenticator(auth.Options{Index: f.st})})
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, target, user, password string, header http.Header) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		if user != "" {
			r.SetBasicAuth(user, password)
		}
		for k, v := range header {
			r.Header[k] = v
		}
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, r)
		return rr
	}

	var got Info
	decodeJSON(t, send(http.MethodGet, "/api/v1/info", "", "", nil), http.StatusOK, &got)
	if got.Caller.Principal != "anonymous" || got.Caller.Admin || got.ExternalURL != testInfo.ExternalURL {
		t.Errorf("anonymous info = %+v", got)
	}
	decodeJSON(t, send(http.MethodGet, "/api/v1/info", "alice", "alice-password", nil), http.StatusOK, &got)
	if got.Caller.Kind != "user" || got.Caller.Name != "alice" || got.Caller.Principal != "user:"+got.Caller.UID {
		t.Errorf("alice's info = %+v", got)
	}
	wantError(t, send(http.MethodGet, "/api/v1/info", "alice", "wrong-password", nil),
		http.StatusUnauthorized, codeUnauthenticated, "credentials are needed")

	preflight := http.Header{"Origin": {"https://evil.test"}, "Access-Control-Request-Method": {"GET"},
		"Access-Control-Request-Headers": {"authorization"}, "Sec-Fetch-Site": {"cross-site"}}
	for _, target := range []string{"/api/v1/info", "/api/v1/repos"} {
		rr := send(http.MethodOptions, target, "", "", preflight)
		wantError(t, rr, http.StatusNotFound, codeNotFound, "OPTIONS "+target)
		for k := range rr.Header() {
			if strings.HasPrefix(k, "Access-Control-") {
				t.Errorf("OPTIONS %s from another origin: %s: %q, want no CORS header", target, k, rr.Header().Get(k))
			}
		}
	}
}

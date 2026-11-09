package token

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// TestParseScopes is the table of the scope parameters: actions, *, unknown actions, unknown
// types, repeated and merged scopes, several scopes in one parameter, bad names, and the
// catalog scope.
func TestParseScopes(t *testing.T) {
	long := "raw/assets/" + strings.Repeat("a", 250)
	tests := []struct {
		name   string
		params []string
		want   string // fmt of the result
	}{
		{"none", nil, "[]"},
		{"pull,push", []string{"repository:raw/assets/hello:pull,push"}, "[{repository raw/assets/hello [pull push]}]"},
		{"star", []string{"repository:raw/assets/hello:*"}, "[{repository raw/assets/hello [pull push delete]}]"},
		{"order of the actions", []string{"repository:raw/assets/hello:delete,pull"}, "[{repository raw/assets/hello [pull delete]}]"},
		{"unknown actions ignored", []string{"repository:raw/assets/hello:pull,admin,PUSH,"}, "[{repository raw/assets/hello [pull]}]"},
		{"no known action", []string{"repository:raw/assets/hello:admin"}, "[]"},
		{"no actions", []string{"repository:raw/assets/hello:"}, "[]"},
		{"repeated and merged", []string{"repository:raw/assets/hello:pull", "repository:raw/assets/hello:push,pull"},
			"[{repository raw/assets/hello [pull push]}]"},
		{"sorted by name", []string{"repository:raw/assets/zeta:pull", "repository:pypi/internal/alpha:push"},
			"[{repository pypi/internal/alpha [push]} {repository raw/assets/zeta [pull]}]"},
		{"several in one parameter", []string{"repository:raw/assets/a:pull repository:raw/assets/b:push"},
			"[{repository raw/assets/a [pull]} {repository raw/assets/b [push]}]"},
		{"catalog kept", []string{"registry:catalog:*", "repository:raw/assets/a:pull"},
			"[{registry catalog [*]} {repository raw/assets/a [pull]}]"},
		{"another registry scope dropped", []string{"registry:catalog:pull", "registry:other:*"}, "[]"},
		{"another type dropped", []string{"repository(plugin):raw/assets/a:pull", "unknown:raw/assets/a:pull"}, "[]"},
		{"a name of two components", []string{"repository:raw/assets:pull"}, "[]"},
		{"a name with upper case", []string{"repository:Raw/assets/a:pull"}, "[]"},
		{"a name with a host", []string{"repository:localhost:8080/raw/assets/a:pull"}, "[]"},
		{"a name too long", []string{"repository:" + long + ":pull"}, "[]"},
		{"no colon", []string{"repository"}, "[]"},
		{"one colon", []string{"repository:raw/assets/a"}, "[]"},
	}
	for _, tt := range tests {
		got := ParseScopes(tt.params)
		if s := fmt.Sprint(got); s != tt.want {
			t.Errorf("%s: ParseScopes(%q) = %s, want %s", tt.name, tt.params, s, tt.want)
		}
	}
}

// grants allows each user the actions listed for it on each repo (<format>/<name>).
type grants map[string]map[string][]auth.Action

func (g grants) Allowed(_ context.Context, p auth.Principal, a auth.Action, f, r string) (bool, error) {
	return slices.Contains(g[p.String()][f+"/"+r], a), nil
}

// failing is an Authorizer that cannot decide.
type failing struct{}

var errAuthz = errors.New("bindings unreachable")

func (failing) Allowed(context.Context, auth.Principal, auth.Action, string, string) (bool, error) {
	return false, errAuthz
}

// TestReduce checks the reduction of the scopes to what the Authorizer allows: bob, a reader of
// raw/assets, gets pull only on a pull,push scope (O-12 at the unit level), the scope of a repo
// he may not read with no action, and the catalog; alice, a writer, gets pull and push; without
// an Authorizer every action is granted, the anonymous principal's included; an Authorizer
// that fails is an error.
func TestReduce(t *testing.T) {
	z := grants{
		"user:bob":   {"raw/assets": {auth.ActionRead}},
		"user:alice": {"raw/assets": {auth.ActionRead, auth.ActionWrite}},
	}
	scopes := ParseScopes([]string{"repository:raw/assets/hello:pull,push", "repository:pypi/internal/x:pull", "registry:catalog:*"})
	for _, tt := range []struct {
		name  string
		authz auth.Authorizer
		p     auth.Principal
		want  string
	}{
		{"bob, a reader", z, auth.Principal{Kind: auth.KindUser, Name: "bob"}, "[{registry catalog [*]} {repository pypi/internal/x []} {repository raw/assets/hello [pull]}]"},
		{"alice, a writer", z, alice, "[{registry catalog [*]} {repository pypi/internal/x []} {repository raw/assets/hello [pull push]}]"},
		{"anonymous", z, auth.Anonymous, "[{registry catalog [*]} {repository pypi/internal/x []} {repository raw/assets/hello []}]"},
		{"no Authorizer", nil, auth.Anonymous, "[{registry catalog [*]} {repository pypi/internal/x [pull]} {repository raw/assets/hello [pull push]}]"},
	} {
		got, err := Reduce(t.Context(), tt.authz, tt.p, scopes)
		must(t, err)
		if fmt.Sprint(got) != tt.want {
			t.Errorf("%s: %v, want %s", tt.name, got, tt.want)
		}
	}
	if fmt.Sprint(scopes) != "[{registry catalog [*]} {repository pypi/internal/x [pull]} {repository raw/assets/hello [pull push]}]" {
		t.Errorf("Reduce changed its argument: %v", scopes)
	}
	z["user:carol"] = map[string][]auth.Action{"raw/assets": {auth.ActionDelete}}
	got, err := Reduce(t.Context(), z, auth.Principal{Kind: auth.KindUser, Name: "carol"}, ParseScopes([]string{"repository:raw/assets/a:*"}))
	if err != nil || fmt.Sprint(got) != "[{repository raw/assets/a [delete]}]" {
		t.Errorf("delete only: %v, %v", got, err)
	}
	if _, err := Reduce(t.Context(), failing{}, alice, scopes); !errors.Is(err, errAuthz) {
		t.Errorf("an Authorizer that fails: %v", err)
	}
}

// TestReduceCredential checks that the credential of the request narrows the token before the
// Authorizer, with or without one: a personal access token with read gets pull only (S-03 on
// /v2), with write push only, without scopes everything its user gets; a deploy token gets the
// actions of its scopes on its own repo, nothing elsewhere and never delete (S-04, X-14); a
// password changes nothing.
func TestReduceCredential(t *testing.T) {
	alice := auth.Principal{Kind: auth.KindUser, Name: "alice"}
	deploy := auth.Principal{Kind: auth.KindToken, Name: "20"}
	z := grants{
		"user:alice": {"raw/assets": {auth.ActionRead, auth.ActionWrite, auth.ActionDelete}},
		"token:20":   {"raw/assets": {auth.ActionRead, auth.ActionWrite}},
	}
	scopes := ParseScopes([]string{"repository:raw/assets/hello:*", "repository:pypi/internal/x:pull,push", "registry:catalog:*"})
	pat := func(sc ...string) auth.Credential {
		return auth.Credential{Kind: auth.CredPAT, TokenID: 1, UserID: 1, Scopes: sc}
	}
	dep := func(sc ...string) auth.Credential {
		return auth.Credential{Kind: auth.CredDeploy, TokenID: 20, RepoID: 10, Repo: "raw/assets", Scopes: sc}
	}
	for _, tt := range []struct {
		name  string
		authz auth.Authorizer
		p     auth.Principal
		cred  auth.Credential
		want  string
	}{
		{"password", nil, alice, auth.Credential{Kind: auth.CredPassword},
			"[{registry catalog [*]} {repository pypi/internal/x [pull push]} {repository raw/assets/hello [pull push delete]}]"},
		{"pat without scopes", z, alice, pat(), "[{registry catalog [*]} {repository pypi/internal/x []} {repository raw/assets/hello [pull push delete]}]"},
		{"pat read", nil, alice, pat("read"), "[{registry catalog [*]} {repository pypi/internal/x [pull]} {repository raw/assets/hello [pull]}]"},
		{"pat read, with an Authorizer", z, alice, pat("read"), "[{registry catalog [*]} {repository pypi/internal/x []} {repository raw/assets/hello [pull]}]"},
		{"pat write", nil, alice, pat("write"), "[{registry catalog [*]} {repository pypi/internal/x [push]} {repository raw/assets/hello [push]}]"},
		{"deploy read write", nil, deploy, dep("read", "write"), "[{registry catalog [*]} {repository pypi/internal/x []} {repository raw/assets/hello [pull push]}]"},
		{"deploy read write, with an Authorizer", z, deploy, dep("read", "write"), "[{registry catalog [*]} {repository pypi/internal/x []} {repository raw/assets/hello [pull push]}]"},
		{"deploy read", nil, deploy, dep("read"), "[{registry catalog [*]} {repository pypi/internal/x []} {repository raw/assets/hello [pull]}]"},
	} {
		got, err := Reduce(auth.WithCredential(t.Context(), tt.cred), tt.authz, tt.p, scopes)
		must(t, err)
		if fmt.Sprint(got) != tt.want {
			t.Errorf("%s: %v, want %s", tt.name, got, tt.want)
		}
	}
}

func TestScopeAction(t *testing.T) {
	for a, want := range map[auth.Action]string{auth.ActionRead: "pull", auth.ActionWrite: "push", auth.ActionDelete: "delete", auth.ActionManage: ""} {
		if got := ScopeAction(a); got != want {
			t.Errorf("ScopeAction(%s) = %q, want %q", a, got, want)
		}
		if want != "" && authAction(want) != a {
			t.Errorf("authAction(%s) = %s, want %s", want, authAction(want), a)
		}
	}
}

// TestRealm checks the realm and the service: from the external URL with its port and without
// its path, else from the request's Host.
func TestRealm(t *testing.T) {
	r := httptest.NewRequest("GET", "http://localhost:8080/v2/", nil)
	for _, tt := range []struct{ externalURL, realm, service string }{
		{"https://art.test", "https://art.test/v2/token", "art.test"},
		{"https://art.test:8443/", "https://art.test:8443/v2/token", "art.test:8443"},
		{"http://localhost:18080", "http://localhost:18080/v2/token", "localhost:18080"},
		{"", "http://localhost:8080/v2/token", "localhost:8080"},
		{"::not a url", "http://localhost:8080/v2/token", "localhost:8080"},
	} {
		realm, svc := Realm(tt.externalURL, r)
		if realm != tt.realm || svc != tt.service {
			t.Errorf("Realm(%q) = %q, %q; want %q, %q", tt.externalURL, realm, svc, tt.realm, tt.service)
		}
	}
}

func TestChallenge(t *testing.T) {
	for _, tt := range []struct {
		scope        string
		insufficient bool
		want         string
	}{
		{"", false, `Bearer realm="https://art.test/v2/token",service="art.test"`},
		{"repository:raw/assets/hello:pull", false, `Bearer realm="https://art.test/v2/token",service="art.test",scope="repository:raw/assets/hello:pull"`},
		{"repository:a/b/c:pull,push repository:d/e/f:pull", true,
			`Bearer realm="https://art.test/v2/token",service="art.test",scope="repository:a/b/c:pull,push repository:d/e/f:pull",error="insufficient_scope"`},
	} {
		if got := Challenge("https://art.test/v2/token", "art.test", tt.scope, tt.insufficient); got != tt.want {
			t.Errorf("Challenge(%q, %t) = %s, want %s", tt.scope, tt.insufficient, got, tt.want)
		}
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"raw/assets/hello": true, "raw/assets/docs/hello.txt": true, "oci/images/a-b__c": true,
		"raw/assets": false, "raw/assets/": false, "Raw/assets/x": false, "raw//assets/x": false, "": false,
		"raw/assets/" + strings.Repeat("x", 245): false,
	} {
		if ValidName(name) != want {
			t.Errorf("ValidName(%q) = %t", name, !want)
		}
	}
}

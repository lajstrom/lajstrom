package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// allTokensFixture is an RBAC fixture with root the admin, erin a maintainer on */* and alice a
// reader of raw/*, the repos raw/scratch and raw/other, and tokens of every kind: alice's
// personal access tokens (one expired), raw/scratch's deploy token, a deploy token over oci/*,
// erin's revoked one, and those of a deleted user, gone.
func allTokensFixture(t *testing.T) *fixture {
	t.Helper()
	f := rbacFixture(t, [3]string{"root", auth.RoleAdmin, "*/*"}, [3]string{"erin", auth.RoleMaintainer, "*/*"},
		[3]string{"alice", auth.RoleReader, "raw/*"})
	f.addUsers(t, "gone")
	scratch, other := f.create(t, "raw", "scratch"), f.create(t, "raw", "other")
	ctx := context.Background()
	id := func(name string) *int64 {
		u, err := f.st.GetUser(ctx, name)
		must(t, err)
		return &u.ID
	}
	now := testNow // the clock of the fixture's services
	for _, tok := range []index.Token{
		{Kind: auth.TokenPAT, UserID: id("alice"), Name: "laptop", ExpiresAt: new(now.Add(time.Hour))},
		{Kind: auth.TokenDeploy, RepoID: &scratch.ID, Name: "ci", Scopes: []string{"write"}},
		{Kind: auth.TokenPAT, UserID: id("alice"), Name: "old", ExpiresAt: new(now.Add(-time.Hour))},
		{Kind: auth.TokenDeploy, RepoMask: "oci/*", Name: "nodes", Scopes: []string{"read"}},
		{Kind: auth.TokenPAT, UserID: id("erin"), Name: "revoked", ExpiresAt: new(now.Add(time.Hour)), RevokedAt: new(now)},
		{Kind: auth.TokenDeploy, RepoID: &other.ID, Name: "mirror", Scopes: []string{"read"}},
		{Kind: auth.TokenPAT, UserID: id("gone"), Name: "left", ExpiresAt: new(now.Add(time.Hour))},
	} {
		tok.Hash = "abababab" + fmt.Sprintf("%056x", tok.Name)
		must(t, f.st.CreateToken(ctx, &tok))
	}
	users := &auth.Users{Index: f.st, Log: f.repos.Log}
	must(t, users.Delete(ctx, "gone", auth.Origin{}))
	return f
}

var rootPassword = auth.Credential{Kind: auth.CredPassword}

// listed returns the names of the tokens GET /api/v1/tokens?<query> answers, its owner after
// each: "name:owner".
func listed(t *testing.T, f *fixture, query string) (string, TokenPage) {
	t.Helper()
	var page TokenPage
	decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/tokens?"+query, "", auth.Principal{Kind: auth.KindUser, Name: "root"}, rootPassword),
		http.StatusOK, &page)
	var out []string
	for _, tok := range page.Tokens {
		out = append(out, tok.Name+":"+tok.User+tok.Repo+tok.RepoMask)
	}
	return strings.Join(out, " "), page
}

// TestAllTokens: GET /api/v1/tokens lists every token by id, with its owner, a deleted user's
// under the name it had; each filter narrows, they add up, and a user or repo that does not
// exist has none; never a secret, and of the hash only its prefix.
func TestAllTokens(t *testing.T) {
	f := allTokensFixture(t)
	uid := f.uidOf(t, "alice")
	for _, tt := range []struct{ query, want string }{
		{"", "laptop:alice ci:raw/scratch old:alice nodes:oci/* revoked:erin mirror:raw/other left:gone"},
		{"kind=pat", "laptop:alice old:alice revoked:erin left:gone"},
		{"kind=deploy", "ci:raw/scratch nodes:oci/* mirror:raw/other"},
		{"user=alice", "laptop:alice old:alice"},
		{"user=" + uid, "laptop:alice old:alice"},
		{"user=alice&live=true", "laptop:alice"},
		{"user=gone", ""},
		{"user=nobody", ""},
		{"repo=raw/scratch", "ci:raw/scratch"},
		{"repo=raw/missing", ""},
		{"repoMask=oci/*", "nodes:oci/*"},
		{"repoMask=*/*", ""},
		{"live=true", "laptop:alice ci:raw/scratch nodes:oci/* mirror:raw/other"},
		{"live=false&kind=deploy&repo=raw/other", "mirror:raw/other"},
		{"kind=deploy&user=alice", ""},
	} {
		t.Run(tt.query, func(t *testing.T) {
			if got, page := listed(t, f, tt.query); got != tt.want || page.Next != "" {
				t.Errorf("tokens = %q, next %q; want %q and no next", got, page.Next, tt.want)
			}
		})
	}
	body := f.doAs(http.MethodGet, "/api/v1/tokens", "", auth.Principal{Kind: auth.KindUser, Name: "root"}, rootPassword).Body.String()
	if strings.Contains(body, fmt.Sprintf("%056x", "laptop")) || !strings.Contains(body, `"hashPrefix":"abababab"`) {
		t.Errorf("the answer has more of a hash than its prefix, or not the prefix: %s", body)
	}
}

// TestAllTokensPages: the pages of GET /api/v1/tokens follow next, each of at most limit
// tokens, the last without next, and hold every token once; a cursor that is not a next is 400.
func TestAllTokensPages(t *testing.T) {
	f := allTokensFixture(t)
	var got []string
	for query, n := "limit=3", 0; ; n++ {
		names, page := listed(t, f, query)
		if len(page.Tokens) > 3 {
			t.Fatalf("a page of %d", len(page.Tokens))
		}
		got = append(got, names)
		if page.Next == "" || n > 5 {
			break
		}
		query = "limit=3&cursor=" + url.QueryEscape(page.Next)
	}
	want := "laptop:alice ci:raw/scratch old:alice|nodes:oci/* revoked:erin mirror:raw/other|left:gone"
	if strings.Join(got, "|") != want {
		t.Errorf("pages %q, want %q", strings.Join(got, "|"), want)
	}
	if names, page := listed(t, f, "kind=pat&limit=2&cursor="+url.QueryEscape(cursor("2"))); names != "old:alice revoked:erin" || page.Next == "" {
		t.Errorf("the pats after id 2 = %q, next %q", names, page.Next)
	}
}

// TestAllTokensInvalid: a kind that is neither, a live that is no boolean, a repo or a mask
// that breaks its grammar, a limit out of bounds and a cursor that is no next are 400 naming
// the parameter.
func TestAllTokensInvalid(t *testing.T) {
	f := allTokensFixture(t)
	root := auth.Principal{Kind: auth.KindUser, Name: "root"}
	for query, field := range map[string]string{
		"kind=service": "kind:", "live=yes": "live:", "repo=scratch": "repo:", "repoMask=oci": "repoMask:",
		"repoMask=oci/Bad": "repoMask:", "limit=0": "limit:", "cursor=" + cursor("x"): "cursor:", "cursor=%25": "cursor:",
	} {
		wantError(t, f.doAs(http.MethodGet, "/api/v1/tokens?"+query, "", root, rootPassword), http.StatusBadRequest, codeInvalid, field)
	}
}

// TestAllTokensAuthorization: every token is the admin's alone, with any credential that
// reaches the role, a personal access token without scopes too; a maintainer on */*, a reader,
// a token with scopes and a deploy token are 403, anonymous 401, whatever the query.
func TestAllTokensAuthorization(t *testing.T) {
	f := allTokensFixture(t)
	user := func(name string) auth.Principal { return auth.Principal{Kind: auth.KindUser, Name: name} }
	for _, tt := range []struct {
		name   string
		p      auth.Principal
		c      auth.Credential
		query  string
		status int
	}{
		{"admin", user("root"), rootPassword, "", http.StatusOK},
		{"admin's personal access token", user("root"), auth.Credential{Kind: auth.CredPAT, TokenID: 90}, "", http.StatusOK},
		{"admin's token with scopes", user("root"), auth.Credential{Kind: auth.CredPAT, TokenID: 90, Scopes: []string{"read"}}, "", http.StatusForbidden},
		{"maintainer on */*", user("erin"), rootPassword, "", http.StatusForbidden},
		{"maintainer on */*, a query that does not parse", user("erin"), rootPassword, "kind=x", http.StatusForbidden},
		{"reader, its own tokens", user("alice"), rootPassword, "user=alice", http.StatusForbidden},
		{"a deploy token over */*", auth.Principal{Kind: auth.KindToken, Name: "91"},
			auth.Credential{Kind: auth.CredDeploy, TokenID: 91, Mask: "*/*", Scopes: []string{"read", "write"}}, "", http.StatusForbidden},
		{"anonymous", auth.Anonymous, auth.Credential{}, "repo=raw/scratch", http.StatusUnauthorized},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rr := f.doAs(http.MethodGet, "/api/v1/tokens?"+tt.query, "", tt.p, tt.c)
			wantStatus(t, rr, tt.status)
			wantChallenge(t, rr)
		})
	}
}

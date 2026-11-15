package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// sessionsFixture is an RBAC fixture with root the admin, erin a maintainer of maven/*, wanda a
// writer of maven/releases and alice a reader of maven/*, the repo maven/releases with sessions
// in every state and a blob upload of /v2, and raw/files, which opens none.
func sessionsFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := rbacFixture(t, [3]string{"root", auth.RoleAdmin, "*/*"}, [3]string{"erin", auth.RoleMaintainer, "maven/*"},
		[3]string{"wanda", auth.RoleWriter, "maven/releases"}, [3]string{"alice", auth.RoleReader, "maven/*"})
	f.addUsers(t, "bob")
	ctx := context.Background()
	releases := index.Repo{Format: "maven", Name: "releases", Registry: "local", Prefix: "maven/releases", Kind: index.KindHosted}
	must(t, f.st.CreateRepo(ctx, &releases))
	f.create(t, "raw", "files")
	wanda := "user:" + f.uidOf(t, "wanda")
	for _, s := range []index.Session{
		{Key: "com.example:lib-core:1.4.0", Principal: wanda, State: "open", ExpiresAt: testNow.Add(30 * time.Minute),
			Files: json.RawMessage(`{"name": "com.example:lib-core", "version": "1.4.0", "files": [
				{"name": "com/example/lib-core/1.4.0/lib-core-1.4.0.pom", "digest": "sha256:aa", "size": 1711, "checksums": {"sha1": "x"}},
				{"name": "com/example/lib-core/1.4.0/lib-core-1.4.0.jar", "digest": "sha256:bb", "size": 48213, "checksums": {}}],
				"refused": {"com/example/lib-core/1.4.0/lib-core-1.4.0.jar": "its .sha1 is not the file's"}}`)},
		{Key: "com.example:lib-core:1.3.0", Principal: wanda, State: "committed", ExpiresAt: testNow.Add(30 * time.Minute)},
		{Key: "blob-upload:5d1e", Principal: wanda, State: "open", ExpiresAt: testNow.Add(time.Hour)},
		{Key: "com.example:lib-core:1.2.0", Principal: wanda, State: "open", ExpiresAt: testNow.Add(-time.Minute)},
		{Key: "com.example:app:2.0", Principal: "token:8", State: "open", ExpiresAt: testNow.Add(10 * time.Minute),
			Files: json.RawMessage(`{"name": "com.example:app", "version": "2.0", "files": [], "committing": "2026-01-02T11:59:00Z"}`)},
	} {
		s.RepoID = releases.ID
		must(t, f.st.UpsertSession(ctx, &s))
	}
	return f, wanda
}

// TestSessions: GET …/sessions answers the repo's open publication sessions whose time is not
// up, by id, with their files, refused files, committing mark and a user principal's name; not
// a session that committed or expired, nor a blob upload; a repo that opens none has none; the
// pages follow next.
func TestSessions(t *testing.T) {
	f, wanda := sessionsFixture(t)
	root := auth.Principal{Kind: auth.KindUser, Name: "root"}
	var list SessionList
	decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/repos/maven/releases/sessions", "", root, rootPassword), http.StatusOK, &list)
	if len(list.Sessions) != 2 || list.Next != "" {
		t.Fatalf("sessions %+v, next %q; want 2 and no next", list.Sessions, list.Next)
	}
	got := list.Sessions[0]
	got.CreatedAt, got.UpdatedAt = time.Time{}, time.Time{}
	wantJSON(t, "the deploy of wanda", got, `{"id": 1, "key": "com.example:lib-core:1.4.0", "principal": "`+wanda+`",
		"principalName": "wanda", "files": [
			{"name": "com/example/lib-core/1.4.0/lib-core-1.4.0.pom", "digest": "sha256:aa", "size": 1711},
			{"name": "com/example/lib-core/1.4.0/lib-core-1.4.0.jar", "digest": "sha256:bb", "size": 48213}],
		"refused": {"com/example/lib-core/1.4.0/lib-core-1.4.0.jar": "its .sha1 is not the file's"},
		"committing": false, "createdAt": "0001-01-01T00:00:00Z", "updatedAt": "0001-01-01T00:00:00Z",
		"expiresAt": "2026-01-02T12:30:00Z"}`)
	if s := list.Sessions[1]; s.Key != "com.example:app:2.0" || s.Principal != "token:8" || s.PrincipalName != "" ||
		!s.Committing || len(s.Files) != 0 || s.Refused != nil {
		t.Errorf("the deploy of token 8 = %+v", s)
	}

	var first, second SessionList
	decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/repos/maven/releases/sessions?limit=1", "", root, rootPassword), http.StatusOK, &first)
	decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/repos/maven/releases/sessions?limit=1&cursor="+url.QueryEscape(first.Next), "",
		root, rootPassword), http.StatusOK, &second)
	if len(first.Sessions) != 1 || first.Next == "" || len(second.Sessions) != 1 || second.Next != "" ||
		second.Sessions[0].ID != list.Sessions[1].ID {
		t.Errorf("pages of 1: %+v, then %+v", first, second)
	}
	var none SessionList
	decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/repos/raw/files/sessions", "", root, rootPassword), http.StatusOK, &none)
	if len(none.Sessions) != 0 {
		t.Errorf("raw/files has sessions: %+v", none)
	}
	wantError(t, f.doAs(http.MethodGet, "/api/v1/repos/maven/releases/sessions?cursor="+cursor("x"), "", root, rootPassword),
		http.StatusBadRequest, codeInvalid, "cursor:")
}

// TestSessionsAuthorization: the open sessions need manage on the repo, decided before the repo
// is looked up (docs/spec/rbac.md §3): a writer and a reader get 403, a user without access and
// a repo that does not exist 404, anonymous 401; a token with scopes never manages.
func TestSessionsAuthorization(t *testing.T) {
	f, _ := sessionsFixture(t)
	user := func(name string) auth.Principal { return auth.Principal{Kind: auth.KindUser, Name: name} }
	for _, tt := range []struct {
		name   string
		p      auth.Principal
		c      auth.Credential
		target string
		status int
	}{
		{"admin", user("root"), rootPassword, "maven/releases", http.StatusOK},
		{"maintainer", user("erin"), rootPassword, "maven/releases", http.StatusOK},
		{"maintainer, a repo that does not exist", user("erin"), rootPassword, "maven/missing", http.StatusNotFound},
		{"maintainer's token with scopes", user("erin"), auth.Credential{Kind: auth.CredPAT, TokenID: 5, Scopes: []string{"read", "write"}},
			"maven/releases", http.StatusForbidden},
		{"writer, its own deploy", user("wanda"), rootPassword, "maven/releases", http.StatusForbidden},
		{"reader", user("alice"), rootPassword, "maven/releases", http.StatusForbidden},
		{"no access", user("bob"), rootPassword, "maven/releases", http.StatusNotFound},
		{"no access, a repo that does not exist", user("bob"), rootPassword, "maven/missing", http.StatusNotFound},
		{"the repo's deploy token", auth.Principal{Kind: auth.KindToken, Name: "8"},
			auth.Credential{Kind: auth.CredDeploy, TokenID: 8, Repo: "maven/releases", Scopes: []string{"read", "write"}},
			"maven/releases", http.StatusForbidden},
		{"anonymous", auth.Anonymous, auth.Credential{}, "maven/releases", http.StatusUnauthorized},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rr := f.doAs(http.MethodGet, "/api/v1/repos/"+tt.target+"/sessions", "", tt.p, tt.c)
			wantStatus(t, rr, tt.status)
			wantChallenge(t, rr)
		})
	}
}

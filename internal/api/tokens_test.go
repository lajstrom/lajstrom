package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// created is the answer of a token creation.
type created struct {
	auth.Token
	Secret string `json:"secret"`
}

// redacted returns c with its secret replaced by its prefix and length, for a comparison that
// prints no secret.
func redacted(c created) created {
	c.Secret = fmt.Sprintf("%.8s and %d characters", c.Secret, len(c.Secret)-min(8, len(c.Secret)))
	return c
}

// addUsers creates the users of names, without passwords, in the fixture's index.
func (f *fixture) addUsers(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := f.st.CreateUser(context.Background(), &index.User{Name: name, Kind: auth.KindLocal}); err != nil {
			t.Fatal(err)
		}
	}
}

// asStored returns the principal the authenticator makes for p: user:<uid> of the user of that
// name in the index, when the tests name one that exists, and records the uid's name for
// fakeAuth; p otherwise.
func (f *fixture) asStored(p auth.Principal) auth.Principal {
	if p.Kind != auth.KindUser || index.IsUID(p.Name) {
		return p
	}
	u, err := f.st.GetUser(context.Background(), p.Name)
	if err != nil {
		return p
	}
	testNames.Store(u.UID, u.Name)
	return auth.UserPrincipal(u)
}

// testNames maps the uids asStored made to the users' names, for fakeAuth, whose grants name
// users.
var testNames sync.Map

// uidOf returns the uid of the user called name.
func (f *fixture) uidOf(t *testing.T, name string) string {
	t.Helper()
	u, err := f.st.GetUser(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u.UID
}

// doAs is do with the credential c in the request's context too.
func (f *fixture) doAs(method, target, body string, p auth.Principal, c auth.Credential) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r = r.WithContext(auth.WithCredential(auth.WithPrincipal(r.Context(), f.asStored(p)), c))
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, r)
	return rr
}

// postToken creates a token through the API as admin, checks the 201 and its Location (the
// collection and the id), and returns the answer.
func (f *fixture) postToken(t *testing.T, collection, body string) created {
	t.Helper()
	rr := f.do(http.MethodPost, collection, body, admin)
	var c created
	decodeJSON(t, rr, http.StatusCreated, &c)
	if loc, want := rr.Header().Get("Location"), fmt.Sprintf("%s/%d", collection, c.ID); loc != want {
		t.Errorf("Location = %q, want %q", loc, want)
	}
	return c
}

// wantListed checks that GET collection answers exactly want, by its JSON, and no secret.
func (f *fixture) wantListed(t *testing.T, collection string, want auth.Token, secrets ...string) {
	t.Helper()
	rr := f.do(http.MethodGet, collection, "", admin)
	var list TokenList
	decodeJSON(t, rr, http.StatusOK, &list)
	b, _ := json.Marshal(want)
	wantJSON(t, "GET "+collection, list, `{"tokens": [`+string(b)+`]}`)
	for _, s := range secrets {
		if strings.Contains(rr.Body.String(), s) {
			t.Errorf("GET %s answers a secret", collection)
		}
	}
}

// TestTokensAPI runs the life of the tokens of a user and of a repo through every endpoint:
// create (201, Location, the resource and its secret), list (the hash prefix, never the
// secret), revoke (204, again 204); the secret is in the creation answer and nowhere else,
// not in the log either.
func TestTokensAPI(t *testing.T) {
	f := newFixture(t, Options{})
	f.addUsers(t, "alice")
	f.create(t, "raw", "scratch")

	pat := f.postToken(t, "/api/v1/users/alice/tokens", `{"name": "laptop", "scopes": ["read"], "expiresIn": "30d"}`)
	row, err := f.st.GetTokenByHash(context.Background(), auth.HashSecret(pat.Secret))
	if err != nil {
		t.Fatalf("no token with the hash of the secret: %v", err)
	}
	wantJSON(t, "the created personal access token", redacted(pat), fmt.Sprintf(`{"id": %d, "kind": "pat", "user": "alice", "name": "laptop",
		"scopes": ["read"], "expiresAt": "2026-02-01T12:00:00Z", "lastUsedAt": null, "createdAt": "2026-01-02T12:00:00Z",
		"revokedAt": null, "hashPrefix": %q, "secret": "ljs_pat_ and 43 characters"}`, row.ID, row.Hash[:8]))
	dep := f.postToken(t, "/api/v1/repos/raw/scratch/deploy-tokens", `{"name": "ci", "scopes": ["write"]}`)
	wantJSON(t, "the created deploy token", redacted(dep), fmt.Sprintf(`{"id": %d, "kind": "deploy", "repo": "raw/scratch", "name": "ci",
		"scopes": ["write"], "expiresAt": null, "lastUsedAt": null, "createdAt": "2026-01-02T12:00:00Z",
		"revokedAt": null, "hashPrefix": %q, "secret": "ljs_dep_ and 43 characters"}`, dep.ID, auth.HashSecret(dep.Secret)[:8]))
	if !strings.HasPrefix(pat.Secret, auth.PATPrefix) || !strings.HasPrefix(dep.Secret, auth.DeployPrefix) {
		t.Errorf("secrets %.8s… and %.8s…, want the prefixes of their kinds", pat.Secret, dep.Secret)
	}
	f.wantListed(t, "/api/v1/users/alice/tokens", pat.Token, pat.Secret, dep.Secret)
	f.wantListed(t, "/api/v1/repos/raw/scratch/deploy-tokens", dep.Token, pat.Secret, dep.Secret)

	for range 2 {
		wantStatus(t, f.do(http.MethodDelete, fmt.Sprintf("/api/v1/users/alice/tokens/%d", pat.ID), "", admin), http.StatusNoContent)
		wantStatus(t, f.do(http.MethodDelete, fmt.Sprintf("/api/v1/repos/raw/scratch/deploy-tokens/%d", dep.ID), "", admin), http.StatusNoContent)
	}
	revoked := pat.Token
	revoked.RevokedAt = &testNow
	f.wantListed(t, "/api/v1/users/alice/tokens", revoked, pat.Secret)
	for _, secret := range []string{pat.Secret, dep.Secret, row.Hash} {
		if strings.Contains(f.logs.String(), secret) {
			t.Errorf("the log holds a secret or a hash")
		}
	}
}

// TestTokensAPIAudit checks the audit rows of the API: one token.create and one token.revoke
// per token, by the principal, from the client address, with the request's source.
func TestTokensAPIAudit(t *testing.T) {
	f := newFixture(t, Options{})
	f.addUsers(t, "alice")
	pat := f.postToken(t, "/api/v1/users/alice/tokens", `{"name": "laptop"}`)
	wantStatus(t, f.do(http.MethodDelete, fmt.Sprintf("/api/v1/users/alice/tokens/%d", pat.ID), "", admin), http.StatusNoContent)
	wantStatus(t, f.do(http.MethodDelete, fmt.Sprintf("/api/v1/users/alice/tokens/%d", pat.ID), "", admin), http.StatusNoContent)
	ev := oneAudit(t, f.st, auth.ActionTokenCreate)
	if ev.Principal != "user:admin" || ev.Subject != "user:"+f.uidOf(t, "alice") || ev.IP != "192.0.2.1" || ev.details["source"] != SourceAPI ||
		ev.details["name"] != "laptop" || ev.details["kind"] != "pat" {
		t.Errorf("token.create = %+v %v", ev.AuditEvent, ev.details)
	}
	ev = oneAudit(t, f.st, auth.ActionTokenRevoke)
	if ev.Principal != "user:admin" || ev.Subject != "user:"+f.uidOf(t, "alice") || ev.details["id"] != float64(pat.ID) {
		t.Errorf("token.revoke = %+v %v", ev.AuditEvent, ev.details)
	}
	evs, _ := f.st.ListAudit(context.Background(), index.AuditFilter{})
	raw, _ := json.Marshal(evs)
	if bytes.Contains(raw, []byte(pat.Secret)) || bytes.Contains(raw, []byte(auth.HashSecret(pat.Secret))) {
		t.Error("the audit log holds the secret or its hash")
	}
}

// TestTokensAPIRefused is the table of the refusals: 400 for a field that breaks its rule
// (S-06: an expiry over a year; X-14: delete in the scopes of a deploy token), 409 for a name
// taken, 404 for an unknown user, repo or token, and for the token of another user or repo.
func TestTokensAPIRefused(t *testing.T) {
	f := newFixture(t, Options{})
	f.addUsers(t, "alice", "bob")
	f.create(t, "raw", "scratch")
	f.create(t, "raw", "other")
	alicePAT := f.postToken(t, "/api/v1/users/alice/tokens", `{"name": "laptop"}`)
	bobPAT := f.postToken(t, "/api/v1/users/bob/tokens", `{"name": "laptop"}`)
	otherDep := f.postToken(t, "/api/v1/repos/raw/other/deploy-tokens", `{"name": "ci", "scopes": ["read"]}`)
	year := time.Now().UTC().Add(400 * 24 * time.Hour).Format(time.RFC3339)
	tests := []struct {
		name, method, target, body string
		status                     int
		code                       string
		msg                        string
	}{
		{"expiry over a year", "POST", "/api/v1/users/alice/tokens", `{"name": "x", "expiresAt": "` + year + `"}`, 400, codeInvalid,
			"expiresAt: a personal access token expires at most 365 days ahead"},
		{"lifetime over a year", "POST", "/api/v1/users/alice/tokens", `{"name": "x", "expiresIn": "400d"}`, 400, codeInvalid,
			"expiresIn: a personal access token expires at most 365 days ahead"},
		{"both expiries", "POST", "/api/v1/users/alice/tokens", `{"name": "x", "expiresAt": "2026-12-01T00:00:00Z", "expiresIn": "1d"}`, 400, codeInvalid,
			"expiresAt and expiresIn: give one of them, not both"},
		{"expiry not RFC 3339", "POST", "/api/v1/users/alice/tokens", `{"name": "x", "expiresAt": "tomorrow"}`, 400, codeInvalid,
			`expiresAt: "tomorrow" is not an RFC 3339 time`},
		{"lifetime not a duration", "POST", "/api/v1/users/alice/tokens", `{"name": "x", "expiresIn": "a while"}`, 400, codeInvalid,
			`expiresIn: "a while" is not a positive number of days`},
		{"expiry in the past", "POST", "/api/v1/users/alice/tokens", `{"name": "x", "expiresAt": "2020-01-01T00:00:00Z"}`, 400, codeInvalid,
			"expiresAt: must be in the future"},
		{"empty name", "POST", "/api/v1/users/alice/tokens", `{"name": ""}`, 400, codeInvalid, "name: must not be empty"},
		{"no name", "POST", "/api/v1/repos/raw/scratch/deploy-tokens", `{"scopes": ["read"]}`, 400, codeInvalid, "name: must not be empty"},
		{"pat scope", "POST", "/api/v1/users/alice/tokens", `{"name": "x", "scopes": ["admin"]}`, 400, codeInvalid, `scopes: "admin" is not a scope`},
		{"deploy delete", "POST", "/api/v1/repos/raw/scratch/deploy-tokens", `{"name": "x", "scopes": ["read", "delete"]}`, 400, codeInvalid,
			`scopes: "delete" is not a scope of a deploy token: it gets read, write or both, never delete or manage`},
		{"deploy manage", "POST", "/api/v1/repos/raw/scratch/deploy-tokens", `{"name": "x", "scopes": ["manage"]}`, 400, codeInvalid,
			`scopes: "manage" is not a scope of a deploy token`},
		{"deploy without scopes", "POST", "/api/v1/repos/raw/scratch/deploy-tokens", `{"name": "x"}`, 400, codeInvalid,
			"scopes: a deploy token needs read, write or both"},
		{"unknown field", "POST", "/api/v1/users/alice/tokens", `{"name": "x", "secret": "mine"}`, 400, codeInvalid, `unknown field "secret"`},
		{"name taken", "POST", "/api/v1/users/alice/tokens", `{"name": "laptop"}`, 409, codeConflict, `name: "laptop" is taken by a live token of user alice`},
		{"deploy name taken", "POST", "/api/v1/repos/raw/other/deploy-tokens", `{"name": "ci", "scopes": ["write"]}`, 409, codeConflict,
			`name: "ci" is taken by a live token of repo raw/other`},
		{"unknown user", "POST", "/api/v1/users/dave/tokens", `{"name": "x"}`, 404, codeNotFound, "user dave does not exist"},
		{"list of an unknown user", "GET", "/api/v1/users/dave/tokens", "", 404, codeNotFound, "user dave does not exist"},
		{"unknown repo", "POST", "/api/v1/repos/raw/absent/deploy-tokens", `{"name": "x", "scopes": ["read"]}`, 404, codeNotFound, "repo raw/absent does not exist"},
		{"list of an unknown repo", "GET", "/api/v1/repos/raw/absent/deploy-tokens", "", 404, codeNotFound, "repo raw/absent does not exist"},
		{"token of another user", "DELETE", fmt.Sprintf("/api/v1/users/alice/tokens/%d", bobPAT.ID), "", 404, codeNotFound,
			fmt.Sprintf("token %d of user alice does not exist", bobPAT.ID)},
		{"deploy token as a pat", "DELETE", fmt.Sprintf("/api/v1/users/alice/tokens/%d", otherDep.ID), "", 404, codeNotFound, "of user alice does not exist"},
		{"token of another repo", "DELETE", fmt.Sprintf("/api/v1/repos/raw/scratch/deploy-tokens/%d", otherDep.ID), "", 404, codeNotFound,
			fmt.Sprintf("token %d of repo raw/scratch does not exist", otherDep.ID)},
		{"pat as a deploy token", "DELETE", fmt.Sprintf("/api/v1/repos/raw/other/deploy-tokens/%d", alicePAT.ID), "", 404, codeNotFound, "of repo raw/other does not exist"},
		{"unknown id", "DELETE", "/api/v1/users/alice/tokens/999", "", 404, codeNotFound, "token 999 of user alice does not exist"},
		{"id not a number", "DELETE", "/api/v1/users/alice/tokens/latest", "", 404, codeNotFound, "token latest of user alice does not exist"},
		{"id zero", "DELETE", "/api/v1/repos/raw/other/deploy-tokens/0", "", 404, codeNotFound, "token 0 of repo raw/other does not exist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantError(t, f.do(tt.method, tt.target, tt.body, admin), tt.status, tt.code, tt.msg)
		})
	}
	var list TokenList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/users/bob/tokens", "", admin), http.StatusOK, &list)
	if len(list.Tokens) != 1 || list.Tokens[0].RevokedAt != nil {
		t.Errorf("bob's token after alice's refusals: %+v, want it live", list)
	}
}

// TestTokensAPIAuthorization checks who may manage tokens: a user its own, the admin anyone's,
// manage on the repo for its deploy tokens; a deploy token and a personal access token with
// scopes are 403 without any Authorizer, as is a deploy token changing its own repo (X-14),
// and a deploy token on another repo is 404 (S-04).
func TestTokensAPIAuthorization(t *testing.T) {
	deployCred := func(repo string, id int64) auth.Credential {
		return auth.Credential{Kind: auth.CredDeploy, TokenID: 20, RepoID: id, Repo: repo, Scopes: []string{"read", "write"}}
	}
	scoped := auth.Credential{Kind: auth.CredPAT, TokenID: 1, UserID: 2, Scopes: []string{"read", "write"}}
	full := auth.Credential{Kind: auth.CredPAT, TokenID: 1, UserID: 2}
	deploy := auth.Principal{Kind: auth.KindToken, Name: "20"}
	carol := auth.Principal{Kind: auth.KindUser, Name: "carol"}
	g := map[string]bool{"user:admin manage */*": true, "user:admin read */*": true, "user:carol manage raw/scratch": true,
		"user:carol read raw/scratch": true, "user:alice read raw/open": true}
	tests := []struct {
		name           string
		authz          bool // the fake Authorizer of g, else none
		p              auth.Principal
		c              auth.Credential
		method, target string
		body           string
		status         int
	}{
		{"self", true, alice, auth.Credential{Kind: auth.CredPassword}, "POST", "/api/v1/users/alice/tokens", `{"name": "x"}`, 201},
		{"self lists", true, alice, full, "GET", "/api/v1/users/alice/tokens", "", 200},
		{"another user", true, alice, auth.Credential{Kind: auth.CredPassword}, "GET", "/api/v1/users/bob/tokens", "", 403},
		{"anonymous", true, auth.Anonymous, auth.Credential{}, "GET", "/api/v1/users/alice/tokens", "", 401},
		{"admin", true, admin, auth.Credential{Kind: auth.CredPassword}, "POST", "/api/v1/users/alice/tokens", `{"name": "y"}`, 201},
		{"admin of an unknown user", true, admin, auth.Credential{}, "GET", "/api/v1/users/dave/tokens", "", 404},
		{"stranger of an unknown user", true, bob, auth.Credential{}, "GET", "/api/v1/users/dave/tokens", "", 403},
		{"maintainer of the repo", true, carol, auth.Credential{Kind: auth.CredPassword}, "POST", "/api/v1/repos/raw/scratch/deploy-tokens", `{"name": "x", "scopes": ["read"]}`, 201},
		{"maintainer of another repo", true, carol, auth.Credential{Kind: auth.CredPassword}, "GET", "/api/v1/repos/raw/other/deploy-tokens", "", 404},
		{"reader of the repo", true, alice, auth.Credential{Kind: auth.CredPassword}, "GET", "/api/v1/repos/raw/open/deploy-tokens", "", 403},
		{"self with a scoped pat", false, alice, scoped, "POST", "/api/v1/users/alice/tokens", `{"name": "z"}`, 403},
		{"self with a scoped pat lists", false, alice, scoped, "GET", "/api/v1/users/alice/tokens", "", 403},
		{"scoped pat on deploy tokens", false, alice, scoped, "GET", "/api/v1/repos/raw/scratch/deploy-tokens", "", 403},
		{"deploy token on user tokens", false, deploy, deployCred("raw/scratch", 0), "GET", "/api/v1/users/alice/tokens", "", 403},
		{"deploy token on its repo's tokens", false, deploy, deployCred("raw/scratch", 0), "POST", "/api/v1/repos/raw/scratch/deploy-tokens", `{"name": "x", "scopes": ["read"]}`, 403},
		{"deploy token on another repo's tokens", false, deploy, deployCred("raw/scratch", 0), "GET", "/api/v1/repos/raw/other/deploy-tokens", "", 404},
		{"deploy token patches its repo", false, deploy, deployCred("raw/scratch", 0), "PATCH", "/api/v1/repos/raw/scratch", `{"anonymousRead": true}`, 403},
		{"deploy token deletes its repo", false, deploy, deployCred("raw/scratch", 0), "DELETE", "/api/v1/repos/raw/scratch", "", 403},
		{"deploy token creates a repo", false, deploy, deployCred("raw/scratch", 0), "POST", "/api/v1/repos", scratch, 403},
		{"deploy token reads its repo", false, deploy, deployCred("raw/scratch", 0), "GET", "/api/v1/repos/raw/scratch", "", 200},
		{"deploy token reads another repo", false, deploy, deployCred("raw/scratch", 0), "GET", "/api/v1/repos/raw/other", "", 404},
		{"scoped pat patches a repo", false, alice, scoped, "PATCH", "/api/v1/repos/raw/scratch", `{"anonymousRead": true}`, 403},
		{"another user's tokens without an Authorizer", false, bob, auth.Credential{Kind: auth.CredPassword}, "GET", "/api/v1/users/alice/tokens", "", 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := Options{}
			if tt.authz {
				o.Auth = fakeAuth{grants: g}
			}
			f := newFixture(t, o)
			f.addUsers(t, "alice", "bob")
			for _, name := range []string{"scratch", "other", "open"} {
				f.create(t, "raw", name)
			}
			rr := f.doAs(tt.method, tt.target, tt.body, tt.p, tt.c)
			wantStatus(t, rr, tt.status)
			wantChallenge(t, rr)
		})
	}
	f := newFixture(t, Options{})
	f.create(t, "raw", "scratch")
	var list RepoList
	decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/repos", "", deploy, deployCred("raw/scratch", 0)), http.StatusOK, &list)
	f.create(t, "raw", "other")
	decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/repos", "", deploy, deployCred("raw/scratch", 0)), http.StatusOK, &list)
	if ids := repoIDs(list.Repos); strings.Join(ids, " ") != "raw/scratch" {
		t.Errorf("a deploy token lists %v, want its repo only", ids)
	}
}

// TestTokensNeverLogged runs the authenticator, the middleware and the API with every logger
// writing JSON at debug into one buffer: a token is created, used in Basic credentials and as
// a bearer, refused once revoked, and a deploy token is used and refused as a bearer; the log
// then holds neither secret nor its hash, and the access log names the principals,
// user:<owner> for the personal access token and token:<id> for the deploy token.
//
// Two more tokens have secrets planted by the test, whose rows it writes itself: the report of
// #44 greps the whole output of go test -v ./... for them, and this test writes its log there
// too, at debug.
func TestTokensNeverLogged(t *testing.T) {
	f := newFixture(t, Options{})
	f.addUsers(t, "alice")
	f.create(t, "raw", "scratch")
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(io.MultiWriter(&logs, t.Output()), &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.repos.Log, f.repos.Now = log, nil // the authenticator's clock is the real one
	api := New(Options{Repos: f.repos, Log: log})
	srv, err := server.New(server.Options{Listen: "127.0.0.1:0", API: api, Log: log,
		Authenticator: auth.NewAuthenticator(auth.Options{Index: f.st, Log: log})})
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, target, body, authorization string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		if authorization != "" {
			r.Header.Set("Authorization", authorization)
		}
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, r)
		return rr
	}
	var pat, dep created
	decodeJSON(t, send("POST", "/api/v1/users/alice/tokens", `{"name": "laptop"}`, ""), http.StatusCreated, &pat)
	decodeJSON(t, send("POST", "/api/v1/repos/raw/scratch/deploy-tokens", `{"name": "ci", "scopes": ["read"]}`, ""), http.StatusCreated, &dep)
	basicAuth := func(user, secret string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.SetBasicAuth(user, secret)
		return r.Header.Get("Authorization")
	}

	var who Whoami
	decodeJSON(t, send("GET", "/api/v1/whoami", "", "Bearer "+pat.Secret), http.StatusOK, &who)
	alice := "user:" + f.uidOf(t, "alice")
	if who.Principal != alice || who.Name != "alice" {
		t.Errorf("whoami with the bearer = %+v", who)
	}
	decodeJSON(t, send("GET", "/api/v1/whoami", "", basicAuth("alice", pat.Secret)), http.StatusOK, &who)
	if who.Principal != alice {
		t.Errorf("whoami with Basic = %+v", who)
	}
	decodeJSON(t, send("GET", "/api/v1/whoami", "", basicAuth("deploy", dep.Secret)), http.StatusOK, &who)
	if who.Principal != fmt.Sprintf("token:%d", dep.ID) {
		t.Errorf("whoami of the deploy token = %+v", who)
	}
	wantStatus(t, send("GET", "/api/v1/whoami", "", "Bearer "+dep.Secret), http.StatusUnauthorized)
	wantStatus(t, send("DELETE", fmt.Sprintf("/api/v1/users/alice/tokens/%d", pat.ID), "", "Bearer "+pat.Secret), http.StatusNoContent)
	wantStatus(t, send("GET", "/api/v1/whoami", "", "Bearer "+pat.Secret), http.StatusUnauthorized) // S-05: at the next request
	wantStatus(t, send("GET", "/api/v1/whoami", "", basicAuth("alice", pat.Secret)), http.StatusUnauthorized)

	plantedID, plantedPAT, plantedDeploy := plant(t, f), plantedPATSecret, plantedDeploySecret
	for _, tt := range []struct {
		authorization string
		status        int
	}{
		{"Bearer " + plantedPAT, http.StatusOK},
		{basicAuth("alice", plantedPAT), http.StatusOK},
		{basicAuth("bob", plantedPAT), http.StatusUnauthorized},
		{basicAuth("deploy", plantedDeploy), http.StatusOK},
		{"Bearer " + plantedDeploy, http.StatusUnauthorized},
	} {
		wantStatus(t, send("GET", "/api/v1/whoami", "", tt.authorization), tt.status)
	}
	wantStatus(t, send("DELETE", fmt.Sprintf("/api/v1/users/alice/tokens/%d", plantedID), "", ""), http.StatusNoContent)
	wantStatus(t, send("GET", "/api/v1/whoami", "", "Bearer "+plantedPAT), http.StatusUnauthorized)

	text := logs.String()
	for _, s := range []string{pat.Secret, dep.Secret, plantedPAT, plantedDeploy} {
		if strings.Contains(text, s) || strings.Contains(text, auth.HashSecret(s)) {
			t.Fatalf("the log holds the secret %s… or its hash", s[:len(auth.PATPrefix)])
		}
	}
	for _, want := range []string{
		`"path":"/api/v1/whoami","status":200`, `"principal":"` + alice + `"`, fmt.Sprintf(`"principal":"token:%d"`, dep.ID),
		fmt.Sprintf(`"msg":"authentication refused","token":%d,"reason":"revoked"`, pat.ID),
		`"reason":"a deploy token as a bearer: it goes in Basic credentials, deploy:<token>"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the log does not have %s:\n%s", want, text)
		}
	}
}

// Secrets planted by TestTokensNeverLogged: of the token shape, and found nowhere in the output
// of go test -v ./... (the report of #44 greps it).
const (
	plantedPATSecret    = auth.PATPrefix + "PLANTED0secret0for0the0log0grep0of0issue044"
	plantedDeploySecret = auth.DeployPrefix + "PLANTED0deploy0for0the0log0grep0of0issue044"
)

// plant writes a personal access token of alice and a deploy token of raw/scratch with the
// planted secrets, and returns the id of the first.
func plant(t *testing.T, f *fixture) int64 {
	t.Helper()
	ctx := context.Background()
	alice, err := f.st.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := f.st.GetRepo(ctx, "raw", "scratch")
	if err != nil {
		t.Fatal(err)
	}
	hour := time.Now().Add(time.Hour)
	pat := index.Token{Kind: auth.TokenPAT, UserID: &alice.ID, Name: "planted", Hash: auth.HashSecret(plantedPATSecret), ExpiresAt: &hour}
	dep := index.Token{Kind: auth.TokenDeploy, RepoID: &repo.ID, Name: "planted", Hash: auth.HashSecret(plantedDeploySecret), Scopes: []string{"read"}}
	for _, row := range []*index.Token{&pat, &dep} {
		if err := f.st.CreateToken(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	return pat.ID
}

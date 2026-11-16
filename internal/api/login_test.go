package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// loginFixture returns a fixture whose sign-in checks passwords with an authenticator over its
// index, and the users alice (password alicePassword) and oidc-bob (no password).
func loginFixture(t *testing.T, o Options) *fixture {
	t.Helper()
	f := newFixture(t, Options{})
	hash, err := auth.HashPassword(loginPassword)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []index.User{{Name: "alice", Kind: auth.KindLocal, PasswordHash: hash}, {Name: "oidc-bob", Kind: auth.KindOIDC}} {
		if err := f.st.CreateUser(context.Background(), &u); err != nil {
			t.Fatal(err)
		}
	}
	if o.Passwords == nil {
		o.Passwords = auth.NewAuthenticator(auth.Options{Index: f.st})
	}
	o.Repos, o.Log = f.repos, f.repos.Log
	f.h = recordStatuses(New(o))
	return f
}

// loginPassword is alice's password in loginFixture.
const loginPassword = "correct horse battery staple"

// TestLogin checks POST /api/v1/login with a password (docs/spec/rbac.md §10): 201 with a
// personal access token of the user without scopes, named login <time> <4 hex>, that expires
// after the login lifetime, with its Location and a token.create row by the user, createdBy
// password; the token authenticates as the user. The request's own credential does not count.
func TestLogin(t *testing.T) {
	f := loginFixture(t, Options{LoginLifetime: 3 * time.Hour})
	rr := f.doAs(http.MethodPost, "/api/v1/login", fmt.Sprintf(`{"username": "alice", "password": %q}`, loginPassword),
		auth.Principal{Kind: auth.KindToken, Name: "9"}, auth.Credential{Kind: auth.CredDeploy, TokenID: 9})
	var c created
	decodeJSON(t, rr, http.StatusCreated, &c)
	row, err := f.st.GetTokenByHash(context.Background(), auth.HashSecret(c.Secret))
	if err != nil {
		t.Fatalf("no token with the hash of the secret: %v", err)
	}
	if !strings.HasPrefix(c.Name, "login 2026-01-02T12:00:00Z ") || len(c.Name) != len("login 2026-01-02T12:00:00Z 0a1b") {
		t.Errorf("name = %q, want login <time> <4 hex>", c.Name)
	}
	c.Name = "login"
	wantJSON(t, "the token of the sign-in", redacted(c), fmt.Sprintf(`{"id": %d, "kind": "pat", "user": "alice", "name": "login",
		"scopes": [], "expiresAt": "2026-01-02T15:00:00Z", "lastUsedAt": null, "createdAt": "2026-01-02T12:00:00Z",
		"revokedAt": null, "hashPrefix": %q, "secret": "ljs_pat_ and 43 characters"}`, row.ID, row.Hash[:8]))
	if loc := rr.Header().Get("Location"); loc != fmt.Sprintf("/api/v1/users/alice/tokens/%d", c.ID) {
		t.Errorf("Location = %q", loc)
	}
	if cookies := rr.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("cookies %v, want none", cookies)
	}
	ev := oneAudit(t, f.st, auth.ActionTokenCreate)
	if ev.Principal != "user:"+f.uidOf(t, "alice") || ev.details["createdBy"] != "password" || ev.details["source"] != SourceAPI {
		t.Errorf("token.create = %+v %v, want by alice, createdBy password, source api", ev.AuditEvent, ev.details)
	}
	p, cred, err := auth.NewAuthenticator(auth.Options{Index: f.st, Now: func() time.Time { return testNow }}).
		AuthenticateBearer(context.Background(), c.Secret)
	if err != nil || p.Name != f.uidOf(t, "alice") || cred.Kind != auth.CredPAT {
		t.Errorf("the token authenticates as %v %v, %v; want alice's personal access token", p, cred, err)
	}
	if strings.Contains(f.logs.String(), c.Secret) || strings.Contains(f.logs.String(), loginPassword) {
		t.Error("the log holds the secret or the password")
	}
}

// TestLoginDefaultLifetime checks that a sign-in's token expires after 12 hours by default.
func TestLoginDefaultLifetime(t *testing.T) {
	f := loginFixture(t, Options{})
	var c created
	decodeJSON(t, f.do(http.MethodPost, "/api/v1/login", fmt.Sprintf(`{"username": "alice", "password": %q}`, loginPassword), auth.Principal{}),
		http.StatusCreated, &c)
	if want := testNow.Add(12 * time.Hour); c.ExpiresAt == nil || !c.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt = %v, want %v", c.ExpiresAt, want)
	}
}

// TestLoginRefused checks the refusals of POST /api/v1/login: 401 whatever the reason, 400 for
// a body that is not a name and a password, and no token made.
func TestLoginRefused(t *testing.T) {
	f := loginFixture(t, Options{})
	for _, tc := range []struct {
		body   string
		status int
		code   string
		msg    string
	}{
		{`{"username": "alice", "password": "wrong password"}`, http.StatusUnauthorized, codeUnauthenticated, "the user name or the password is wrong"},
		{`{"username": "nobody", "password": "a password"}`, http.StatusUnauthorized, codeUnauthenticated, "the user name or the password is wrong"},
		{`{"username": "oidc-bob", "password": "a password"}`, http.StatusUnauthorized, codeUnauthenticated, "the user name or the password is wrong"},
		{`{"username": "Alice", "password": "a password"}`, http.StatusUnauthorized, codeUnauthenticated, "the user name or the password is wrong"},
		{`{"username": "alice", "password": "` + auth.PATPrefix + `0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"}`, http.StatusUnauthorized,
			codeUnauthenticated, "the user name or the password is wrong"},
		{`{"username": "alice"}`, http.StatusBadRequest, codeInvalid, `must be {"username", "password"}`},
		{`{}`, http.StatusBadRequest, codeInvalid, `must be {"username", "password"}`},
		{`{"username": "alice", "password": "x", "expiresIn": "365d"}`, http.StatusBadRequest, codeInvalid, "expiresIn"},
		{`[]`, http.StatusBadRequest, codeInvalid, "body"},
	} {
		wantError(t, f.do(http.MethodPost, "/api/v1/login", tc.body, auth.Principal{}), tc.status, tc.code, tc.msg)
	}
	if evs := auditOf(t, f.st, auth.ActionTokenCreate); len(evs) != 0 {
		t.Errorf("%d token.create rows, want none", len(evs))
	}
}

// throttledPasswords refuses every password as a throttled account.
type throttledPasswords struct{}

func (throttledPasswords) AuthenticatePassword(context.Context, string, string) (auth.Principal, auth.Credential, error) {
	return auth.Principal{}, auth.Credential{}, &auth.ThrottledError{Throttle: "account", Account: "user:x", RetryAfter: 1500 * time.Millisecond}
}

// TestLoginThrottled checks that a throttled account or address is 429 with Retry-After in whole
// seconds, rounded up, and that a server without passwords answers 501.
func TestLoginThrottled(t *testing.T) {
	f := loginFixture(t, Options{Passwords: throttledPasswords{}})
	rr := f.do(http.MethodPost, "/api/v1/login", `{"username": "alice", "password": "x"}`, auth.Principal{})
	wantError(t, rr, http.StatusTooManyRequests, codeUnavailable, "too many failed logins")
	if got := rr.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2", got)
	}
	f = newFixture(t, Options{})
	wantError(t, f.do(http.MethodPost, "/api/v1/login", `{"username": "alice", "password": "x"}`, auth.Principal{}),
		http.StatusNotImplemented, codeUnavailable, "checks no passwords")
}

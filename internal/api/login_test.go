package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/dpop"
	"github.com/lajstrom/lajstrom/internal/auth/dpop/dpoptest"
	"github.com/lajstrom/lajstrom/internal/index"
)

// loginURL is server.externalURL in the sign-in tests: the scheme and host of a proof's htu.
const loginURL = "https://lajstrom.test"

// loginFixture returns a fixture whose sign-in checks passwords with an authenticator over its
// index and DPoP proofs with a verifier over it, at testNow, and the users alice (password
// alicePassword) and oidc-bob (no password).
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
	if o.DPoP == nil {
		o.DPoP = loginVerifier(f.st)
	}
	o.Repos, o.Log = f.repos, f.repos.Log
	f.h = recordStatuses(New(o))
	return f
}

// loginVerifier is the DPoP verifier of the sign-in tests over st.
func loginVerifier(st index.Store) *dpop.Verifier {
	return &dpop.Verifier{ExternalURL: loginURL, Replays: st, Now: func() time.Time { return testNow }}
}

// loginPassword is alice's password in loginFixture.
const loginPassword = "correct horse battery staple"

// loginProof returns a proof of key for POST /api/v1/login at testNow.
func loginProof(t *testing.T, key *dpoptest.Key) string {
	t.Helper()
	return key.Proof(t, dpoptest.Claims{HTM: http.MethodPost, HTU: loginURL + "/api/v1/login", IAT: testNow})
}

// signIn posts body to POST /api/v1/login with proof as its DPoP header, unless it is "", as p
// with c.
func (f *fixture) signIn(body, proof string, p auth.Principal, c auth.Credential) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(body))
	if proof != "" {
		r.Header.Set("DPoP", proof)
	}
	if p.Kind != "" {
		r = r.WithContext(auth.WithCredential(auth.WithPrincipal(r.Context(), f.asStored(p)), c))
	}
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, r)
	return rr
}

// signedIn is the answer of a sign-in.
type signedIn struct {
	created
	TokenType string `json:"tokenType"`
}

// TestLogin checks POST /api/v1/login with a password (docs/spec/rbac.md §10): 201 with a
// personal access token of the user without scopes, bound to the key of the request's DPoP
// proof, named login <time> <4 hex>, that expires after the login lifetime, with tokenType DPoP,
// its Location and a token.create row by the user, createdBy password and dpopJkt; the token
// authenticates as the user with a proof of the key, and not as a bearer. The request's own
// credential does not count.
func TestLogin(t *testing.T) {
	f := loginFixture(t, Options{LoginLifetime: 3 * time.Hour})
	key := dpoptest.NewKey(t)
	rr := f.signIn(fmt.Sprintf(`{"username": "alice", "password": %q}`, loginPassword), loginProof(t, key),
		auth.Principal{Kind: auth.KindToken, Name: "9"}, auth.Credential{Kind: auth.CredDeploy, TokenID: 9})
	var c signedIn
	decodeJSON(t, rr, http.StatusCreated, &c)
	row, err := f.st.GetTokenByHash(context.Background(), auth.HashSecret(c.Secret))
	if err != nil {
		t.Fatalf("no token with the hash of the secret: %v", err)
	}
	if !strings.HasPrefix(c.Name, "login 2026-01-02T12:00:00Z ") || len(c.Name) != len("login 2026-01-02T12:00:00Z 0a1b") {
		t.Errorf("name = %q, want login <time> <4 hex>", c.Name)
	}
	if c.TokenType != "DPoP" || row.DPoPJKT != key.JKT {
		t.Errorf("tokenType %q, the row bound to %q; want DPoP and %s", c.TokenType, row.DPoPJKT, key.JKT)
	}
	if ev := oneAudit(t, f.st, auth.ActionTokenCreate); ev.details["source"] != SourceAPI {
		t.Errorf("token.create %v, want source api", ev.details)
	}
	c.Name = "login"
	wantJSON(t, "the token of the sign-in", redacted(c.created), fmt.Sprintf(`{"id": %d, "kind": "pat", "user": "alice", "name": "login",
		"scopes": [], "expiresAt": "2026-01-02T15:00:00Z", "lastUsedAt": null, "createdAt": "2026-01-02T12:00:00Z",
		"revokedAt": null, "hashPrefix": %q, "dpopJkt": %q, "secret": "ljs_pat_ and 43 characters"}`, row.ID, row.Hash[:8], key.JKT))
	if loc := rr.Header().Get("Location"); loc != fmt.Sprintf("/api/v1/users/alice/tokens/%d", c.ID) {
		t.Errorf("Location = %q", loc)
	}
	if cookies := rr.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("cookies %v, want none", cookies)
	}
	wantBoundLogin(t, f, c, key, "password")
}

// wantBoundLogin checks the token c of a sign-in of alice with key: a token.create row by
// alice, createdBy by, source api, dpopJkt; the token authenticates as alice with the key, and
// not as a bearer; the log holds neither the secret nor alice's password.
func wantBoundLogin(t *testing.T, f *fixture, c signedIn, key *dpoptest.Key, by string) {
	t.Helper()
	ev := oneAudit(t, f.st, auth.ActionTokenCreate)
	if ev.Principal != "user:"+f.uidOf(t, "alice") || ev.details["createdBy"] != by || ev.details["dpopJkt"] != key.JKT {
		t.Errorf("token.create = %+v %v, want by alice, createdBy %s, dpopJkt", ev.AuditEvent, ev.details, by)
	}
	authn := auth.NewAuthenticator(auth.Options{Index: f.st, Now: func() time.Time { return testNow }})
	p, cred, err := authn.AuthenticateDPoP(context.Background(), c.Secret, key.JKT)
	if err != nil || p.Name != f.uidOf(t, "alice") || cred.Kind != auth.CredPAT {
		t.Errorf("the token authenticates with its key as %v %v, %v; want alice's personal access token", p, cred, err)
	}
	if _, _, err := authn.AuthenticateBearer(context.Background(), c.Secret); err == nil {
		t.Error("the token authenticates as a bearer")
	}
	if strings.Contains(f.logs.String(), c.Secret) || strings.Contains(f.logs.String(), loginPassword) {
		t.Error("the log holds the secret or the password")
	}
}

// TestLoginDefaultLifetime checks that a sign-in's token expires after 12 hours by default.
func TestLoginDefaultLifetime(t *testing.T) {
	f := loginFixture(t, Options{})
	var c signedIn
	decodeJSON(t, f.signIn(fmt.Sprintf(`{"username": "alice", "password": %q}`, loginPassword), loginProof(t, dpoptest.NewKey(t)),
		auth.Principal{}, auth.Credential{}), http.StatusCreated, &c)
	if want := testNow.Add(12 * time.Hour); c.ExpiresAt == nil || !c.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt = %v, want %v", c.ExpiresAt, want)
	}
}

// TestLoginRefused checks the refusals of POST /api/v1/login: 401 whatever the reason, 400 for
// a body that is not a name and a password, and no token made.
func TestLoginRefused(t *testing.T) {
	f := loginFixture(t, Options{})
	key := dpoptest.NewKey(t)
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
		wantError(t, f.signIn(tc.body, loginProof(t, key), auth.Principal{}, auth.Credential{}), tc.status, tc.code, tc.msg)
	}
	if evs := auditOf(t, f.st, auth.ActionTokenCreate); len(evs) != 0 {
		t.Errorf("%d token.create rows, want none", len(evs))
	}
}

// TestLoginProof checks the DPoP proof of a sign-in (docs/spec/http-routing.md §4): without one,
// or with one that is refused or used already, the sign-in is 400 saying why before the password
// is checked, and no token is made; a proof's ath is ignored.
func TestLoginProof(t *testing.T) {
	f := loginFixture(t, Options{})
	key := dpoptest.NewKey(t)
	body := fmt.Sprintf(`{"username": "alice", "password": %q}`, loginPassword)
	htu := loginURL + "/api/v1/login"
	used := loginProof(t, key)
	wantStatus(t, f.signIn(body, used, auth.Principal{}, auth.Credential{}), http.StatusCreated)
	for _, tc := range []struct {
		name, proof, why string
	}{
		{"no proof", "", "DPoP: the request has no DPoP header"},
		{"a proof used already", used, "DPoP: the proof was used already"},
		{"htm GET", key.Proof(t, dpoptest.Claims{HTM: http.MethodGet, HTU: htu, IAT: testNow}), "htm is not the method"},
		{"htu of another path", key.Proof(t, dpoptest.Claims{HTM: http.MethodPost, HTU: loginURL + "/api/v1/whoami", IAT: testNow}),
			"htu is not the URI"},
		{"htu of the edge's upstream", key.Proof(t, dpoptest.Claims{HTM: http.MethodPost, HTU: "http://example.com/api/v1/login", IAT: testNow}),
			"htu is not the URI"},
		{"iat 10 minutes ago", key.Proof(t, dpoptest.Claims{HTM: http.MethodPost, HTU: htu, IAT: testNow.Add(-10 * time.Minute)}),
			"iat is not within 5 minutes"},
		{"alg HS256", key.Proof(t, dpoptest.Claims{HTM: http.MethodPost, HTU: htu, IAT: testNow, Header: map[string]any{"alg": "HS256"}}),
			"alg is not ES256"},
	} {
		wantError(t, f.signIn(body, tc.proof, auth.Principal{}, auth.Credential{}), http.StatusBadRequest, codeInvalid, tc.why)
	}
	withATH := key.Proof(t, dpoptest.Claims{HTM: http.MethodPost, HTU: htu, IAT: testNow, Token: "another token"})
	wantStatus(t, f.signIn(body, withATH, auth.Principal{}, auth.Credential{}), http.StatusCreated)
	if evs := auditOf(t, f.st, auth.ActionTokenCreate); len(evs) != 2 {
		t.Errorf("%d token.create rows, want 2", len(evs))
	}
}

// throttledPasswords refuses every password as a throttled account.
type throttledPasswords struct{}

func (throttledPasswords) AuthenticatePassword(context.Context, string, string) (auth.Principal, auth.Credential, error) {
	return auth.Principal{}, auth.Credential{}, &auth.ThrottledError{Throttle: "account", Account: "user:x", RetryAfter: 1500 * time.Millisecond}
}

// TestLoginThrottled checks that a throttled account or address is 429 with Retry-After in whole
// seconds, rounded up, and that a server without passwords or without a DPoP verifier answers
// 501.
func TestLoginThrottled(t *testing.T) {
	f := loginFixture(t, Options{Passwords: throttledPasswords{}})
	key := dpoptest.NewKey(t)
	rr := f.signIn(`{"username": "alice", "password": "x"}`, loginProof(t, key), auth.Principal{}, auth.Credential{})
	wantError(t, rr, http.StatusTooManyRequests, codeUnavailable, "too many failed logins")
	if got := rr.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2", got)
	}
	f = newFixture(t, Options{})
	wantError(t, f.signIn(`{"username": "alice", "password": "x"}`, loginProof(t, key), auth.Principal{}, auth.Credential{}),
		http.StatusNotImplemented, codeUnavailable, "checks no DPoP proofs")
	f = newFixture(t, Options{})
	f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log, DPoP: loginVerifier(f.st)}))
	wantError(t, f.signIn(`{"username": "alice", "password": "x"}`, loginProof(t, key), auth.Principal{}, auth.Credential{}),
		http.StatusNotImplemented, codeUnavailable, "checks no passwords")
}

// fakeHandoffs redeems the code "good" with the verifier "v" and the key jkt into a token of
// alice bound to it, refuses "disabled" as a user disabled since its login, and every other code
// as one that does not redeem; it records the origin it was given.
type fakeHandoffs struct {
	jkt    string
	origin auth.Origin
}

func (f *fakeHandoffs) Redeem(_ context.Context, code, verifier, jkt string, o auth.Origin) (*auth.Token, string, error) {
	f.origin = o
	switch {
	case code == "good" && verifier == "v" && jkt == f.jkt:
		exp := testNow.Add(time.Hour)
		return &auth.Token{ID: 7, Kind: auth.TokenPAT, User: "alice", Name: "oidc 2026-01-02T12:00:00Z 0a1b", Scopes: []string{},
			ExpiresAt: &exp, CreatedAt: testNow, HashPrefix: "01234567", DPoPJKT: jkt}, auth.PATPrefix + "secret", nil
	case code == "disabled":
		return nil, "", fmt.Errorf("the user alice is disabled: %w", auth.ErrBadCredentials)
	}
	return nil, "", fmt.Errorf("%w: code: unknown, used, expired, or of another verifier or key: sign in again", auth.ErrInvalid)
}

// TestLoginCode checks POST /api/v1/login with the code of an OIDC login (docs/spec/rbac.md §10):
// 201 with the redeemed token, bound to the key of the proof, and its Location; 400 for a code
// that does not redeem, of another key too, for every code while OIDC is off, for a body of both
// forms or of half of one, and without a proof; 401 for a user disabled since its login.
func TestLoginCode(t *testing.T) {
	key, other := dpoptest.NewKey(t), dpoptest.NewKey(t)
	h := &fakeHandoffs{jkt: key.JKT}
	f := loginFixture(t, Options{Handoffs: h})
	rr := f.signIn(`{"code": "good", "codeVerifier": "v"}`, loginProof(t, key), auth.Principal{}, auth.Credential{})
	var c signedIn
	decodeJSON(t, rr, http.StatusCreated, &c)
	if c.ID != 7 || c.User != "alice" || c.Secret != auth.PATPrefix+"secret" || c.DPoPJKT != key.JKT || c.TokenType != "DPoP" ||
		rr.Header().Get("Location") != "/api/v1/users/alice/tokens/7" {
		t.Errorf("the answer: %+v, tokenType %q, Location %q", redacted(c.created), c.TokenType, rr.Header().Get("Location"))
	}
	wantError(t, f.signIn(`{"code": "good", "codeVerifier": "v"}`, loginProof(t, other), auth.Principal{}, auth.Credential{}),
		http.StatusBadRequest, codeInvalid, "of another verifier or key")
	wantError(t, f.signIn(`{"code": "good", "codeVerifier": "v"}`, "", auth.Principal{}, auth.Credential{}),
		http.StatusBadRequest, codeInvalid, "DPoP: the request has no DPoP header")
	if h.origin.Source != SourceAPI || h.origin.IP != "192.0.2.1" {
		t.Errorf("the origin of the redemption: %+v", h.origin)
	}
	for _, tc := range []struct {
		body   string
		status int
		code   string
		msg    string
	}{
		{`{"code": "good", "codeVerifier": "w"}`, http.StatusBadRequest, codeInvalid, "code: unknown, used, expired, or of another verifier"},
		{`{"code": "disabled", "codeVerifier": "v"}`, http.StatusUnauthorized, codeUnauthenticated, "disabled"},
		{`{"code": "good"}`, http.StatusBadRequest, codeInvalid, `or {"code", "codeVerifier"}`},
		{`{"code": "good", "codeVerifier": "v", "username": "alice", "password": "x"}`, http.StatusBadRequest, codeInvalid, `or {"code", "codeVerifier"}`},
		{`{"codeVerifier": "v", "password": "x"}`, http.StatusBadRequest, codeInvalid, `or {"code", "codeVerifier"}`},
	} {
		wantError(t, f.signIn(tc.body, loginProof(t, key), auth.Principal{}, auth.Credential{}), tc.status, tc.code, tc.msg)
	}
	f = loginFixture(t, Options{})
	wantError(t, f.signIn(`{"code": "good", "codeVerifier": "v"}`, loginProof(t, key), auth.Principal{}, auth.Credential{}),
		http.StatusBadRequest, codeInvalid, "code: unknown, used, expired, or of another verifier")
}

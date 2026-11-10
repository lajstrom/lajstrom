package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// alicePassword has a colon: the first colon of Basic credentials splits (RFC 7617).
const alicePassword = "alice:password-1"

// aliceHash is the hash of alicePassword, made once: argon2id is slow under the race detector.
var aliceHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword(alicePassword)
	if err != nil {
		panic(err)
	}
	return h
})

// usersReader is an index.Store with the user alice, the repo raw/scratch, and the tokens of
// tokenRows, and nothing else; err, when set, is the answer of every GetUser.
type usersReader struct {
	index.Store
	err error
}

// Secrets of the tokens of usersReader: a personal access token of alice, one of hers that is
// revoked, and a deploy token of raw/scratch with read.
const (
	alicePAT     = auth.PATPrefix + "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	revokedPAT   = auth.PATPrefix + "GFEDCBAzyxwvutsrqponmlkjihgfedcba9876543210"
	scratchToken = auth.DeployPrefix + "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
)

// tokenRows are the tokens of usersReader by the hash of their secret.
var tokenRows = func() map[string]index.Token {
	alice, scratch := int64(1), int64(10)
	hour := time.Now().Add(time.Hour)
	revoked := time.Now().Add(-time.Hour)
	return map[string]index.Token{
		auth.HashSecret(alicePAT):     {ID: 1, Kind: auth.TokenPAT, UserID: &alice, ExpiresAt: &hour},
		auth.HashSecret(revokedPAT):   {ID: 2, Kind: auth.TokenPAT, UserID: &alice, ExpiresAt: &hour, RevokedAt: &revoked},
		auth.HashSecret(scratchToken): {ID: 7, Kind: auth.TokenDeploy, RepoID: &scratch, Scopes: []string{"read"}},
	}
}()

func (u usersReader) GetTokenByHash(_ context.Context, hash string) (*index.Token, error) {
	if t, ok := tokenRows[hash]; ok {
		return &t, nil
	}
	return nil, index.ErrNotFound
}

func (u usersReader) ListUsers(ctx context.Context) ([]index.User, error) {
	a, err := u.GetUser(ctx, "alice")
	if err != nil {
		return nil, err
	}
	return []index.User{*a}, nil
}

func (u usersReader) GetRepoByID(_ context.Context, id int64) (*index.Repo, error) {
	if id == 10 {
		return &index.Repo{ID: 10, Format: "raw", Name: "scratch"}, nil
	}
	return nil, index.ErrNotFound
}

func (u usersReader) TouchToken(context.Context, int64, time.Time) error { return nil }

func (u usersReader) GetUser(_ context.Context, name string) (*index.User, error) {
	switch {
	case u.err != nil:
		return nil, u.err
	case name == "alice":
		return &index.User{ID: 1, UID: aliceUID, Name: "alice", PasswordHash: aliceHash(), Kind: auth.KindLocal}, nil
	}
	return nil, index.ErrNotFound
}

// aliceUID is alice's uid.
const aliceUID = "019a6c3e-7b1e-7c3d-8e2f-00000000a11c"

func (u usersReader) GetUserByID(ctx context.Context, id int64) (*index.User, error) {
	if id != 1 {
		return nil, index.ErrNotFound
	}
	return u.GetUser(ctx, "alice")
}

// basic returns the Authorization header of HTTP Basic credentials.
func basic(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

// seen records what the handlers behind the middleware saw.
type seen struct {
	called        bool
	principal     auth.Principal
	credential    auth.Credential
	authorization string
}

// authServer returns a server with the authenticator over r, or none when r is nil, whose API,
// facade and /test/ handlers record what they see.
func authServer(t *testing.T, r index.Store) (*Server, *seen, *bytes.Buffer) {
	t.Helper()
	var sw seen
	record := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw = seen{called: true, principal: auth.PrincipalFrom(r.Context()), credential: auth.CredentialFrom(r.Context()),
			authorization: r.Header.Get("Authorization")}
		w.WriteHeader(http.StatusNoContent)
	})
	o := Options{API: record, Facade: record}
	if r != nil {
		o.Authenticator = auth.NewAuthenticator(auth.Options{Index: r})
	}
	s, buf := newTestServer(t, o)
	s.mux.Handle("/test/", record)
	s.mux.Handle("/raw/", record)
	return s, &sw, buf
}

// TestAuthenticateTokens checks the tokens in the middleware: a personal access token in Basic
// credentials is its user, a deploy token is token:<id> whatever the user name, both with their
// credential in the context; a personal access token as a bearer is its user on the API and
// the frontends and goes on untouched under /v2/, whose bearer is the facade's, and so is a
// deploy token as a bearer, token:<id> (ADR 0012); a revoked token is 401 in the route's format;
// the access log has the principal, and no log has a secret.
func TestAuthenticateTokens(t *testing.T) {
	alice := auth.Principal{Kind: auth.KindUser, Name: aliceUID}
	deploy := auth.Principal{Kind: auth.KindToken, Name: "7"}
	pat := auth.Credential{Kind: auth.CredPAT, TokenID: 1, UserID: 1}
	dep := auth.Credential{Kind: auth.CredDeploy, TokenID: 7, RepoID: 10, Repo: "raw/scratch", Scopes: []string{"read"}}
	const textBody = "credentials are needed: the ones sent were refused\n"
	const apiBody = `{"error":{"code":"FORBIDDEN","message":"credentials are needed: the ones sent were refused"}}` + "\n"
	tests := []struct {
		name          string
		path          string
		authorization string
		status        int
		principal     auth.Principal
		cred          auth.Credential
		body, ctype   string // of a 401
	}{
		{name: "pat in basic", path: "/test/x", authorization: basic("alice", alicePAT), status: 204, principal: alice, cred: pat},
		{name: "pat in basic on /v2/token", path: "/v2/token", authorization: basic("alice", alicePAT), status: 204, principal: alice, cred: pat},
		{name: "deploy token in basic", path: "/raw/scratch/file", authorization: basic("deploy", scratchToken), status: 204, principal: deploy, cred: dep},
		{name: "deploy token with any user", path: "/raw/scratch/file", authorization: basic("gitlab-ci-token", scratchToken), status: 204, principal: deploy, cred: dep},
		{name: "pat bearer on whoami", path: "/api/v1/whoami", authorization: "Bearer " + alicePAT, status: 204, principal: alice, cred: pat},
		{name: "pat bearer on raw", path: "/raw/scratch/file", authorization: "Bearer " + alicePAT, status: 204, principal: alice, cred: pat},
		{name: "pat bearer in lower case", path: "/raw/scratch/file", authorization: "bearer " + alicePAT, status: 204, principal: alice, cred: pat},
		{name: "pat bearer on /v2 goes on", path: "/v2/raw/scratch/x/tags/list", authorization: "Bearer " + alicePAT, status: 204, principal: auth.Anonymous},
		{name: "pat bearer on /v2/token goes on", path: "/v2/token", authorization: "Bearer " + alicePAT, status: 204, principal: auth.Anonymous},
		{name: "revoked bearer on /v2 goes on", path: "/v2/raw/scratch/x/tags/list", authorization: "Bearer " + revokedPAT, status: 204, principal: auth.Anonymous},
		{name: "other bearer goes on", path: "/api/v1/whoami", authorization: "Bearer eyJhbGciOi.x.y", status: 204, principal: auth.Anonymous},
		{name: "revoked pat in basic", path: "/raw/scratch/file", authorization: basic("alice", revokedPAT), status: 401, body: textBody, ctype: "text/plain; charset=utf-8"},
		{name: "revoked pat bearer on the API", path: "/api/v1/repos", authorization: "Bearer " + revokedPAT, status: 401, body: apiBody, ctype: "application/json"},
		{name: "revoked pat bearer on raw", path: "/raw/scratch/file", authorization: "Bearer " + revokedPAT, status: 401, body: textBody, ctype: "text/plain; charset=utf-8"},
		{name: "deploy token bearer", path: "/raw/scratch/file", authorization: "Bearer " + scratchToken, status: 204, principal: deploy, cred: dep},
		{name: "deploy token bearer on /v2 goes on", path: "/v2/raw/scratch/x/tags/list", authorization: "Bearer " + scratchToken, status: 204, principal: auth.Anonymous},
		{name: "pat of alice as bob", path: "/test/x", authorization: basic("bob", alicePAT), status: 401, body: textBody, ctype: "text/plain; charset=utf-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, sw, logs := authServer(t, usersReader{})
			r := httptest.NewRequest(http.MethodGet, tt.path, nil)
			r.Header.Set("Authorization", tt.authorization)
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, r)
			if rr.Code != tt.status {
				t.Fatalf("status = %d, want %d; body %s", rr.Code, tt.status, rr.Body)
			}
			l := findLog(t, logs, "request")
			if tt.status == http.StatusUnauthorized {
				wantRefusal(t, rr, sw, l, tt.body, tt.ctype, basicChallenge)
			} else {
				wantPassed(t, sw, l, tt.principal, tt.authorization)
				if fmt.Sprint(sw.credential) != fmt.Sprint(tt.cred) {
					t.Errorf("the handler saw the credential %+v, want %+v", sw.credential, tt.cred)
				}
			}
			for i, secret := range []string{alicePAT, revokedPAT, scratchToken, auth.HashSecret(alicePAT), auth.HashSecret(scratchToken)} {
				if strings.Contains(logs.String(), secret) {
					t.Errorf("the log contains secret or hash %d", i)
				}
			}
		})
	}
}

// TestAuthenticate checks the middleware: no credentials and other schemes go on as anonymous,
// Basic credentials as the user, in the handler's context and in the access log; credentials
// that are refused or do not decode are 401 at once, with the challenge and the error format
// of the route's family; the system endpoints are not special.
func TestAuthenticate(t *testing.T) {
	alice := auth.Principal{Kind: auth.KindUser, Name: aliceUID}
	const textBody = "credentials are needed: the ones sent were refused\n"
	tests := []struct {
		name          string
		path          string
		authorization string
		status        int
		principal     auth.Principal // in the handler's context and the access log, when it is called
		body          string         // exact body of a 401
		ctype         string
		challenge     string // WWW-Authenticate of a 401; "" is the Basic challenge
	}{
		{name: "no credentials", path: "/test/x", status: 204, principal: auth.Anonymous},
		{name: "basic", path: "/test/x", authorization: basic("alice", alicePassword), status: 204, principal: alice},
		{name: "basic in lower case", path: "/test/x", authorization: "basic " + strings.TrimPrefix(basic("alice", alicePassword), "Basic "), status: 204, principal: alice},
		{name: "basic on the API", path: "/api/v1/whoami", authorization: basic("alice", alicePassword), status: 204, principal: alice},
		{name: "basic on /v2", path: "/v2/raw/scratch/x/tags/list", authorization: basic("alice", alicePassword), status: 204, principal: alice},
		{name: "bearer goes on", path: "/v2/raw/scratch/x/tags/list", authorization: "Bearer eyJhbGciOi.x.y", status: 204, principal: auth.Anonymous},
		{name: "other scheme goes on", path: "/test/x", authorization: `Digest username="alice"`, status: 204, principal: auth.Anonymous},
		{name: "wrong password, text", path: "/test/x", authorization: basic("alice", "wrong-password"), status: 401,
			body: textBody, ctype: "text/plain; charset=utf-8"},
		{name: "unknown user, text", path: "/raw/scratch/file", authorization: basic("mallory", alicePassword), status: 401,
			body: textBody, ctype: "text/plain; charset=utf-8"},
		{name: "wrong password, healthz", path: "/-/healthz", authorization: basic("alice", "wrong-password"), status: 401,
			body: textBody, ctype: "text/plain; charset=utf-8"},
		{name: "wrong password, /v2/", path: "/v2/raw/scratch/x/tags/list", authorization: basic("alice", "wrong-password"), status: 401,
			body: `{"errors":[{"code":"UNAUTHORIZED","message":"credentials are needed: the ones sent were refused"}]}` + "\n", ctype: "application/json",
			challenge: v2Challenge},
		{name: "wrong password, /v2", path: "/v2", authorization: basic("alice", "wrong-password"), status: 401,
			body: `{"errors":[{"code":"UNAUTHORIZED","message":"credentials are needed: the ones sent were refused"}]}` + "\n", ctype: "application/json",
			challenge: v2Challenge},
		{name: "wrong password, /v2/token", path: "/v2/token", authorization: basic("alice", "wrong-password"), status: 401,
			body: `{"errors":[{"code":"UNAUTHORIZED","message":"credentials are needed: the ones sent were refused"}]}` + "\n", ctype: "application/json"},
		{name: "basic on /v2/token", path: "/v2/token", authorization: basic("alice", alicePassword), status: 204, principal: alice},
		{name: "wrong password, API", path: "/api/v1/repos", authorization: basic("alice", "wrong-password"), status: 401,
			body: `{"error":{"code":"FORBIDDEN","message":"credentials are needed: the ones sent were refused"}}` + "\n", ctype: "application/json"},
		{name: "not base64", path: "/test/x", authorization: "Basic !!!not-base64", status: 401, body: textBody, ctype: "text/plain; charset=utf-8"},
		{name: "no colon", path: "/test/x", authorization: "Basic " + base64.StdEncoding.EncodeToString([]byte("alice")), status: 401,
			body: textBody, ctype: "text/plain; charset=utf-8"},
		{name: "scheme only", path: "/api/v1/repos", authorization: "Basic", status: 401,
			body: `{"error":{"code":"FORBIDDEN","message":"credentials are needed: the ones sent were refused"}}` + "\n", ctype: "application/json"},
		{name: "no user name", path: "/test/x", authorization: basic("", alicePassword), status: 401,
			body: textBody, ctype: "text/plain; charset=utf-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, sw, logs := authServer(t, usersReader{})
			r := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.authorization != "" {
				r.Header.Set("Authorization", tt.authorization)
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, r)
			if rr.Code != tt.status {
				t.Fatalf("status = %d, want %d; body %s", rr.Code, tt.status, rr.Body)
			}
			l := findLog(t, logs, "request")
			if tt.status == http.StatusUnauthorized {
				wantRefusal(t, rr, sw, l, tt.body, tt.ctype, cmp.Or(tt.challenge, basicChallenge))
			} else {
				wantPassed(t, sw, l, tt.principal, tt.authorization)
			}
			for _, secret := range []string{alicePassword, "wrong-password", tt.authorization} {
				if secret != "" && strings.Contains(logs.String(), secret) {
					t.Errorf("the log contains %q:\n%s", secret, logs.String())
				}
			}
		})
	}
}

// v2Challenge is the challenge of a refusal under /v2/ of a server without an external URL,
// for a request to httptest's host.
const v2Challenge = `Bearer realm="http://example.com/v2/token",service="example.com"`

// wantRefusal checks a 401 of the middleware: the handler was not called, the answer has the
// challenge, body and ctype, and the access log line l has the anonymous principal.
func wantRefusal(t *testing.T, rr *httptest.ResponseRecorder, sw *seen, l map[string]any, body, ctype, challenge string) {
	t.Helper()
	if sw.called {
		t.Error("the handler was called after a refusal")
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != challenge {
		t.Errorf("WWW-Authenticate = %q, want %q", got, challenge)
	}
	if rr.Body.String() != body || rr.Header().Get("Content-Type") != ctype {
		t.Errorf("answer %q (%s), want %q (%s)", rr.Body, rr.Header().Get("Content-Type"), body, ctype)
	}
	if l["principal"] != "anonymous" || l["status"] != float64(401) {
		t.Errorf("access log principal %v status %v, want anonymous and 401", l["principal"], l["status"])
	}
}

// wantPassed checks a request the middleware let through: the handler saw the principal p and
// the Authorization header untouched, and the access log line l has p.
func wantPassed(t *testing.T, sw *seen, l map[string]any, p auth.Principal, authorization string) {
	t.Helper()
	if !sw.called || sw.principal != p {
		t.Errorf("the handler saw %+v, want the principal %v", sw, p)
	}
	if sw.authorization != authorization {
		t.Errorf("the handler saw Authorization %q, want it untouched", sw.authorization)
	}
	if l["principal"] != p.String() {
		t.Errorf("access log principal = %v, want %s", l["principal"], p)
	}
}

// TestRefusalChallenge checks the challenge of a refused Basic under /v2/ with an external URL:
// the realm is the token endpoint of the external URL and the service its host and port,
// whatever the request's Host; /v2/token and the other routes keep the Basic challenge.
func TestRefusalChallenge(t *testing.T) {
	s, _ := newTestServer(t, Options{ExternalURL: "https://art.example.com:8443",
		Authenticator: auth.NewAuthenticator(auth.Options{Index: usersReader{}})})
	for path, want := range map[string]string{
		"/v2/raw/scratch/x/manifests/v1": `Bearer realm="https://art.example.com:8443/v2/token",service="art.example.com:8443"`,
		"/v2/":                           `Bearer realm="https://art.example.com:8443/v2/token",service="art.example.com:8443"`,
		"/v2/token":                      basicChallenge,
		"/api/v1/repos":                  basicChallenge,
		"/raw/scratch/x":                 basicChallenge,
	} {
		r := httptest.NewRequest(http.MethodGet, "http://10.0.0.7:8080"+path, nil)
		r.Header.Set("Authorization", basic("alice", "wrong-password"))
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, r)
		if got := rr.Header().Get("WWW-Authenticate"); rr.Code != http.StatusUnauthorized || got != want {
			t.Errorf("%s: %d %q, want 401 %q", path, rr.Code, got, want)
		}
	}
}

// TestHoldPrincipal checks that the principal HoldPrincipal records is the access log's, as the
// facade records the principal of a bearer, and that it does nothing without the holder.
func TestHoldPrincipal(t *testing.T) {
	bob := auth.Principal{Kind: auth.KindUser, Name: "bob"}
	s, logs := newTestServer(t, Options{})
	s.mux.HandleFunc("/hold", func(w http.ResponseWriter, r *http.Request) {
		HoldPrincipal(r.Context(), bob)
		w.WriteHeader(http.StatusNoContent)
	})
	s.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/hold", nil))
	if l := findLog(t, logs, "request"); l["principal"] != "user:bob" {
		t.Errorf("access log principal = %v, want user:bob", l["principal"])
	}
	HoldPrincipal(context.Background(), bob) // no holder: nothing to record, and no panic
}

// TestAuthenticateNil checks that a nil Authenticator authenticates nobody: Basic credentials,
// even ones that do not decode, go on as anonymous.
func TestAuthenticateNil(t *testing.T) {
	s, sw, logs := authServer(t, nil)
	r := httptest.NewRequest(http.MethodGet, "/test/x", nil)
	r.Header.Set("Authorization", "Basic !!!")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusNoContent || !sw.called || sw.principal != auth.Anonymous {
		t.Errorf("status %d, the handler saw %+v; want 204 as anonymous", rr.Code, sw)
	}
	if l := findLog(t, logs, "request"); l["principal"] != "anonymous" {
		t.Errorf("access log principal = %v, want anonymous", l["principal"])
	}
}

// TestAuthenticateFailure checks a failure to decide: 500 in the error format of the route's
// family, without a challenge, the cause in the log with the request id and without the
// secret.
func TestAuthenticateFailure(t *testing.T) {
	errDown := errors.New("index: database is gone")
	tests := []struct{ path, body string }{
		{"/test/x", "Internal Server Error\n"},
		{"/v2/raw/scratch/x/tags/list", `{"errors":[{"code":"UNKNOWN","message":"Internal Server Error"}]}` + "\n"},
		{"/api/v1/repos", `{"error":{"code":"INTERNAL","message":"Internal Server Error"}}` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			s, sw, logs := authServer(t, usersReader{err: errDown})
			r := httptest.NewRequest(http.MethodGet, tt.path, nil)
			r.Header.Set("Authorization", basic("alice", alicePassword))
			r.Header.Set(requestIDHeader, "req-9")
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, r)
			if rr.Code != http.StatusInternalServerError || rr.Body.String() != tt.body || sw.called {
				t.Errorf("answer %d %q (handler called %v), want 500 %q", rr.Code, rr.Body, sw.called, tt.body)
			}
			if c := rr.Header().Get("WWW-Authenticate"); c != "" {
				t.Errorf("a 500 has the challenge %q", c)
			}
			l := findLog(t, logs, "authentication failed")
			if l["level"] != "ERROR" || l["user"] != "alice" || l["requestId"] != "req-9" || !strings.Contains(fmt.Sprint(l["error"]), errDown.Error()) {
				t.Errorf("log line = %v", l)
			}
			if strings.Contains(logs.String(), alicePassword) {
				t.Errorf("the log contains the password:\n%s", logs.String())
			}
		})
	}
}

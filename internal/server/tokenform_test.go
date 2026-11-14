package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// postForm answers a POST of /v2/token with the form body and the Authorization header authz,
// when it is not empty.
func postForm(s *Server, body, authz string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v2/token", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	s.Handler().ServeHTTP(rr, r)
	return rr
}

// passwordForm is the form containerd posts for user and secret.
func passwordForm(user, secret string) string {
	return url.Values{"grant_type": {"password"}, "username": {user}, "password": {secret},
		"service": {"example.com"}, "client_id": {"containerd-client"}}.Encode()
}

// TestAuthenticateTokenForm: on POST /v2/token the credential is the form's username and
// password, checked as Basic credentials are: a password, a personal access token of the user,
// a deploy token under any user name, each reaching the facade with its principal and
// credential; an Authorization header is not read. What Basic credentials would get refused
// is refused alike, with GET's answer; the facade is not called.
func TestAuthenticateTokenForm(t *testing.T) {
	s, sw, logs := authServer(t, usersReader{})
	alice := auth.Principal{Kind: auth.KindUser, Name: aliceUID}
	tests := []struct {
		name, user, secret, authz string
		principal                 auth.Principal
		cred                      auth.CredentialKind
	}{
		{"a password", "alice", alicePassword, "", alice, auth.CredPassword},
		{"a personal access token", "alice", alicePAT, "", alice, auth.CredPAT},
		{"a deploy token under any user name", "containerd", scratchToken, "", auth.Principal{Kind: auth.KindToken, Name: "7"}, auth.CredDeploy},
		{"a deploy token under no user name", "", scratchToken, "", auth.Principal{Kind: auth.KindToken, Name: "7"}, auth.CredDeploy},
		{"the form's credential, not the header's", "alice", alicePassword, basic("alice", "not-alice-0"), alice, auth.CredPassword},
		{"a wrong password", "alice", "not-alice-1", "", auth.Principal{}, ""},
		{"a revoked token", "alice", revokedPAT, "", auth.Principal{}, ""},
		{"a token under another user's name", "bob", alicePAT, "", auth.Principal{}, ""},
		{"a user that does not exist", "nobody", "a password", "", auth.Principal{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			*sw = seen{}
			rr := postForm(s, passwordForm(tt.user, tt.secret), tt.authz)
			if tt.cred == "" {
				get := serveAs(s, "/v2/token", basic(tt.user, tt.secret))
				if rr.Code != http.StatusUnauthorized || rr.Body.String() != get.Body.String() ||
					rr.Header().Get("WWW-Authenticate") != `Basic realm="lajstrom"` || get.Code != http.StatusUnauthorized {
					t.Errorf("POST: %d %q %q; GET: %d %q", rr.Code, rr.Body, rr.Header().Get("WWW-Authenticate"), get.Code, get.Body)
				}
				if sw.called {
					t.Error("the facade was called")
				}
				return
			}
			if rr.Code != http.StatusNoContent || sw.principal != tt.principal || sw.credential.Kind != tt.cred {
				t.Errorf("answer %d %q as %v with %v, want 204 as %v with %v", rr.Code, rr.Body, sw.principal, sw.credential.Kind, tt.principal, tt.cred)
			}
		})
	}
	for _, secret := range []string{alicePassword, "not-alice-0", "not-alice-1", alicePAT, scratchToken} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the log has a secret of a form:\n%s", logs)
		}
	}
}

// TestTokenFormRefused: a form that is refused, a body that is no form or too large, another
// grant type, is answered 400 or 413 in OAuth2's format before its credential is checked: the
// facade is not called, and the wrong passwords it carries count as no failed login, so that
// alice still logs in after more of them than the throttle allows. Basic credentials do not
// make up for a form without a credential.
func TestTokenFormRefused(t *testing.T) {
	s, sw, _ := authServer(t, usersReader{})
	refused := []struct{ body, authz, ctype string }{
		{"grant_type=refresh_token&refresh_token=" + alicePAT, "", ""},
		{"grant_type=refresh_token&username=alice&password=not-alice-2", "", ""},
		{"grant_type=password&username=alice", basic("alice", alicePassword), ""},
		{passwordForm("alice", "not-alice-3"), "", "text/plain"},
		{passwordForm("alice", "not-alice-4") + "&password=again", "", ""},
		{passwordForm("alice", "not-alice-5") + "&client_id=" + strings.Repeat("c", 8<<10), "", ""},
	}
	for i := range auth.LoginFailures + 1 {
		f := refused[i%len(refused)]
		*sw = seen{}
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v2/token", strings.NewReader(f.body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if f.ctype != "" {
			r.Header.Set("Content-Type", f.ctype)
		}
		if f.authz != "" {
			r.Header.Set("Authorization", f.authz)
		}
		s.Handler().ServeHTTP(rr, r)
		if (rr.Code != http.StatusBadRequest && rr.Code != http.StatusRequestEntityTooLarge) || !strings.HasPrefix(rr.Body.String(), `{"error":`) ||
			rr.Header().Get("Cache-Control") != "no-store" || sw.called {
			t.Errorf("form %d: %d %q, facade called: %t", i%len(refused), rr.Code, rr.Body, sw.called)
		}
	}
	if rr := postForm(s, passwordForm("alice", alicePassword), ""); rr.Code != http.StatusNoContent {
		t.Errorf("alice after %d refused forms: %d %q", auth.LoginFailures+1, rr.Code, rr.Body)
	}
}

// TestTokenFormThrottle: the failed logins of the form and of Basic credentials count in one
// throttle: LoginFailures wrong passwords, half posted and half sent to GET, throttle alice on
// both, with GET's answer.
func TestTokenFormThrottle(t *testing.T) {
	s, _, _ := authServer(t, usersReader{})
	for i := range auth.LoginFailures {
		var rr *httptest.ResponseRecorder
		if i%2 == 0 {
			rr = postForm(s, passwordForm("alice", "not-alice-6"), "")
		} else {
			rr = serveAs(s, "/v2/token", basic("alice", "not-alice-6"))
		}
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d: %d", i, rr.Code)
		}
	}
	body := `{"errors":[{"code":"TOOMANYREQUESTS","message":"too many failed logins: try again after Retry-After seconds"}]}` + "\n"
	wantThrottled(t, postForm(s, passwordForm("alice", alicePassword), ""), body, 60)
	wantThrottled(t, serveAs(s, "/v2/token", basic("alice", alicePassword)), body, 60)
}

package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// The secrets of fakeCI: a CI token of acme/app, one that is refused, and one whose keys do not
// load.
const (
	goodCI = auth.CIPrefix + "good.token.x"
	badCI  = auth.CIPrefix + "bad.token.x"
	downCI = auth.CIPrefix + "down.token.x"
)

var acmeApp = auth.Principal{Kind: auth.KindCI, Name: "github/acme/app"}

// fakeCI authenticates the secrets above.
type fakeCI struct{}

func (fakeCI) Authenticate(_ context.Context, secret string) (auth.Principal, auth.Credential, error) {
	switch secret {
	case goodCI:
		return acmeApp, auth.Credential{Kind: auth.CredCI, CIToken: "jti-1", Grants: []auth.Grant{{Role: auth.RoleWriter, Mask: "raw/*"}}}, nil
	case downCI:
		return auth.Principal{}, auth.Credential{}, errors.New("the keys do not load")
	}
	return auth.Principal{}, auth.Credential{}, auth.ErrBadCredentials
}

// TestCIToken: a CI token is the password of Basic under any user name on every route, of the
// form of POST /v2/token, and a bearer outside /v2 (docs/spec/rbac.md §11); a bearer on /v2 is
// the facade's. A refused one is 401, a failure to decide 500; without Options.CI it is refused.
func TestCIToken(t *testing.T) {
	sw := &seen{}
	record := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*sw = seen{called: true, principal: auth.PrincipalFrom(r.Context()), credential: auth.CredentialFrom(r.Context()),
			authorization: r.Header.Get("Authorization")}
		w.WriteHeader(http.StatusNoContent)
	})
	s, logs := newTestServer(t, Options{Facade: record, API: record, CI: fakeCI{},
		Authenticator: auth.NewAuthenticator(auth.Options{Index: usersReader{}})})
	s.mux.Handle("/raw/", record)
	for _, tt := range []struct {
		name, path, authz string
		status            int
		as                auth.Principal
	}{
		{"Basic on a frontend", "/raw/assets/x", basic("__token__", goodCI), http.StatusNoContent, acmeApp},
		{"Basic on /v2/token", "/v2/token?scope=repository:raw/assets/x:push", basic("ci", goodCI), http.StatusNoContent, acmeApp},
		{"Basic on the API", "/api/v1/whoami", basic("anything", goodCI), http.StatusNoContent, acmeApp},
		{"a bearer outside /v2", "/raw/assets/x", "Bearer " + goodCI, http.StatusNoContent, acmeApp},
		{"a bearer on /v2 is the facade's", "/v2/raw/assets/x/manifests/1", "Bearer " + goodCI, http.StatusNoContent, auth.Anonymous},
		{"a refused token", "/raw/assets/x", basic("ci", badCI), http.StatusUnauthorized, auth.Principal{}},
		{"a refused bearer", "/api/v1/whoami", "Bearer " + badCI, http.StatusUnauthorized, auth.Principal{}},
		{"keys that do not load", "/raw/assets/x", basic("ci", downCI), http.StatusInternalServerError, auth.Principal{}},
	} {
		*sw = seen{}
		rr := serveAs(s, tt.path, tt.authz)
		if rr.Code != tt.status || sw.principal != tt.as {
			t.Errorf("%s: %d as %v, want %d as %v", tt.name, rr.Code, sw.principal, tt.status, tt.as)
		}
		if tt.as == acmeApp && (sw.credential.Kind != auth.CredCI || sw.credential.CIToken != "jti-1") {
			t.Errorf("%s: credential %+v", tt.name, sw.credential)
		}
	}
	*sw = seen{}
	if rr := postForm(s, passwordForm("__token__", goodCI), ""); rr.Code != http.StatusNoContent || sw.principal != acmeApp {
		t.Errorf("POST /v2/token: %d as %v", rr.Code, sw.principal)
	}
	for _, secret := range []string{goodCI, badCI} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the log has a CI token:\n%s", logs)
		}
	}

	off, _ := newTestServer(t, Options{Facade: record, Authenticator: auth.NewAuthenticator(auth.Options{Index: usersReader{}})})
	*sw = seen{}
	if rr := serveAs(off, "/v2/token", basic("ci", goodCI)); rr.Code != http.StatusUnauthorized || sw.called {
		t.Errorf("without auth.ci: %d, called %v", rr.Code, sw.called)
	}
}

// TestOIDCTokenEndpoint: with Options.CIExchange, POST /-/oidc/token hands the token exchange to
// it and the device grant to the login, or answers it unsupported_grant_type without one; a body
// over 16 KiB is 413; and the route counts against the rate limit of /v2/token.
func TestOIDCTokenEndpoint(t *testing.T) {
	var got string
	handler := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = name + " " + r.PostForm.Get("subject_token") + r.PostForm.Get("device_code")
			w.WriteHeader(http.StatusOK)
		})
	}
	post := func(s *Server, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/-/oidc/token", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Forwarded-For", "198.51.100.7") // httptest's peer, 192.0.2.1, is the trusted edge
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, r)
		return rr
	}
	exchange := url.Values{"grant_type": {tokenExchangeGrant}, "subject_token": {"jwt"}}
	device := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {"dc"}}

	both, _ := newTestServer(t, Options{OIDC: handler("login"), CIExchange: handler("ci"),
		Authenticator: auth.NewAuthenticator(auth.Options{Index: usersReader{}})})
	for form, want := range map[*url.Values]string{&exchange: "ci jwt", &device: "login dc"} {
		got = ""
		if rr := post(both, *form); rr.Code != http.StatusOK || got != want || rr.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%v: %d to %q, want %q", *form, rr.Code, got, want)
		}
	}
	ciOnly, _ := newTestServer(t, Options{CIExchange: handler("ci"), TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
		Authenticator: auth.NewAuthenticator(auth.Options{Index: usersReader{}})})
	if rr := post(ciOnly, device); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "unsupported_grant_type") {
		t.Errorf("a device grant without the login: %d %s", rr.Code, rr.Body)
	}
	if rr := post(ciOnly, url.Values{"grant_type": {tokenExchangeGrant}, "x": {strings.Repeat("a", 17<<10)}}); rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over 16 KiB: %d", rr.Code)
	}
	if rr := serveAs(ciOnly, "/-/oidc/login", ""); rr.Code != http.StatusNotFound {
		t.Errorf("the login's routes without the login: %d", rr.Code)
	}
	limited := 0
	for range auth.TokenRequests + 1 {
		if post(ciOnly, exchange).Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Errorf("no exchange was throttled after %d", auth.TokenRequests)
	}
}

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/server"
)

// TestWhoami checks GET /api/v1/whoami: the principal of the request, whatever the
// authorizer says, and 401 with the Basic challenge for the anonymous principal.
func TestWhoami(t *testing.T) {
	deny := fakeAuth{grants: map[string]bool{}} // allows nothing: whoami does not ask
	tests := []struct {
		name string
		o    Options
		p    auth.Principal
		want Whoami
	}{
		{name: "user", p: alice, want: Whoami{Principal: "user:alice", Kind: "user", Name: "alice"}},
		{name: "user without any grant", o: Options{Auth: deny}, p: bob, want: Whoami{Principal: "user:bob", Kind: "user", Name: "bob"}},
		{name: "deploy token", p: auth.Principal{Kind: auth.KindToken, Name: "42"}, want: Whoami{Principal: "token:42", Kind: "token", Name: "42"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.o)
			var got Whoami
			decodeJSON(t, f.do(http.MethodGet, "/api/v1/whoami", "", tt.p), http.StatusOK, &got)
			if got != tt.want {
				t.Errorf("whoami = %+v, want %+v", got, tt.want)
			}
		})
	}
	for _, o := range []Options{{}, {Auth: deny}} {
		f := newFixture(t, o)
		rr := f.do(http.MethodGet, "/api/v1/whoami", "", auth.Anonymous)
		wantError(t, rr, http.StatusUnauthorized, codeForbidden, "credentials are needed")
		if c := rr.Header().Get("WWW-Authenticate"); c != basicChallenge {
			t.Errorf("WWW-Authenticate = %q, want %q", c, basicChallenge)
		}
	}
}

// TestWhoamiThroughServer runs whoami behind the server's authentication middleware with a
// user of the index: none, the right and a wrong password.
func TestWhoamiThroughServer(t *testing.T) {
	f := newFixture(t, Options{})
	users := &auth.Users{Index: f.st}
	if _, err := users.Create(context.Background(), "alice", "alice-password", false, auth.Origin{Source: SourceCLI}); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Options{Listen: "127.0.0.1:0", API: f.h, Log: f.repos.Log,
		Authenticator: auth.NewAuthenticator(auth.Options{Index: f.st})})
	if err != nil {
		t.Fatal(err)
	}
	get := func(user, password string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
		if user != "" {
			r.SetBasicAuth(user, password)
		}
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, r)
		return rr
	}

	wantError(t, get("", ""), http.StatusUnauthorized, codeForbidden, "credentials are needed")
	var got Whoami
	decodeJSON(t, get("alice", "alice-password"), http.StatusOK, &got)
	if got != (Whoami{Principal: "user:alice", Kind: "user", Name: "alice"}) {
		t.Errorf("whoami = %+v, want alice", got)
	}
	rr := get("alice", "wrong-password")
	wantError(t, rr, http.StatusUnauthorized, codeForbidden, "credentials are needed")
	if c := rr.Header().Get("WWW-Authenticate"); c != basicChallenge {
		t.Errorf("WWW-Authenticate = %q, want %q", c, basicChallenge)
	}
}

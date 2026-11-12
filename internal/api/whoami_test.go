package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
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
		{name: "user", p: alice, want: Whoami{Kind: "user", Name: "alice"}},
		{name: "user without any grant", o: Options{Auth: deny}, p: bob, want: Whoami{Kind: "user", Name: "bob"}},
		{name: "deploy token", p: auth.Principal{Kind: auth.KindToken, Name: "42"}, want: Whoami{Principal: "token:42", Kind: "token", Name: "42"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.o)
			f.addUsers(t, "alice", "bob")
			if tt.p.Kind == auth.KindUser { // user:<uid>, both in the principal and as the uid
				tt.want.UID = f.uidOf(t, tt.p.Name)
				tt.want.Principal = "user:" + tt.want.UID
			}
			var got Whoami
			decodeJSON(t, f.do(http.MethodGet, "/api/v1/whoami", "", tt.p), http.StatusOK, &got)
			if d := cmp.Diff(tt.want, got); d != "" {
				t.Errorf("whoami: (-want +got)\n%s", d)
			}
		})
	}
	for _, o := range []Options{{}, {Auth: deny}} {
		f := newFixture(t, o)
		rr := f.do(http.MethodGet, "/api/v1/whoami", "", auth.Anonymous)
		wantError(t, rr, http.StatusUnauthorized, codeUnauthenticated, "credentials are needed")
		if c := rr.Header().Get("WWW-Authenticate"); c != basicChallenge {
			t.Errorf("WWW-Authenticate = %q, want %q", c, basicChallenge)
		}
	}
}

// TestWhoamiGroups: a user's groups are those of its identity's latest login while they count
// (docs/spec/rbac.md §8); a local user has none.
func TestWhoamiGroups(t *testing.T) {
	f := newFixture(t, Options{})
	f.addUsers(t, "alice", "bob")
	ctx := context.Background()
	u, err := f.st.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	id := index.Identity{UserID: u.ID, Issuer: "https://idp", Subject: "a"}
	if err := f.st.CreateIdentity(ctx, &id); err != nil {
		t.Fatal(err)
	}
	now := testNow // the fixture's clock
	for _, tt := range []struct {
		name   string
		expiry time.Time
		want   []string
	}{
		{"groups of a login that counts", now.Add(time.Hour), []string{"dev", "ops"}},
		{"groups of a login that lapsed", now.Add(-time.Minute), nil},
	} {
		if err := f.st.SetIdentityGroups(ctx, id.ID, []string{"ops", "dev"}, now, tt.expiry); err != nil {
			t.Fatal(err)
		}
		var got Whoami
		decodeJSON(t, f.do(http.MethodGet, "/api/v1/whoami", "", alice), http.StatusOK, &got)
		if d := cmp.Diff(tt.want, got.Groups); d != "" {
			t.Errorf("%s: groups (-want +got)\n%s", tt.name, d)
		}
	}
	var got Whoami
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/whoami", "", bob), http.StatusOK, &got)
	if got.Groups != nil {
		t.Errorf("a local user's groups = %q, want none", got.Groups)
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

	wantError(t, get("", ""), http.StatusUnauthorized, codeUnauthenticated, "credentials are needed")
	var got Whoami
	decodeJSON(t, get("alice", "alice-password"), http.StatusOK, &got)
	if got.Kind != "user" || got.Name != "alice" || !index.IsUID(got.UID) || got.Principal != "user:"+got.UID {
		t.Errorf("whoami = %+v, want alice", got)
	}
	rr := get("alice", "wrong-password")
	wantError(t, rr, http.StatusUnauthorized, codeUnauthenticated, "credentials are needed")
	if c := rr.Header().Get("WWW-Authenticate"); c != basicChallenge {
		t.Errorf("WWW-Authenticate = %q, want %q", c, basicChallenge)
	}
}

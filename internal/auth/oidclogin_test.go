package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestLoginToken: the token of a login is a personal access token of the user without scopes,
// expiring with the lifetime, created under the credential oidc, which MayCreatePAT allows.
func TestLoginToken(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	u, err := users.LoginOIDC(ctx, OIDCLogin{Issuer: "https://idp", Subject: "s1", Username: "olga", Lifetime: time.Hour}, Origin{})
	if err != nil {
		t.Fatal(err)
	}
	tokens := &Tokens{Index: st, Now: users.Now}
	tok, secret, err := tokens.LoginToken(ctx, u, 2*time.Hour, Origin{IP: "192.0.2.1"})
	if err != nil || !strings.HasPrefix(secret, PATPrefix) || tok.User != "olga" || len(tok.Scopes) != 0 ||
		tok.ExpiresAt == nil || !tok.ExpiresAt.Equal(users.Now().UTC().Add(2*time.Hour)) || !strings.HasPrefix(tok.Name, "oidc ") {
		t.Fatalf("LoginToken = %+v, %v", tok, err)
	}
	evs, err := st.ListAudit(ctx, index.AuditFilter{Action: ActionTokenCreate})
	if err != nil || len(evs) != 1 || evs[0].Principal != UserPrincipal(u).String() ||
		!strings.Contains(string(evs[0].Details), `"createdBy":"oidc"`) || !strings.Contains(string(evs[0].Details), `"source":"oidc"`) {
		t.Errorf("token.create = %+v, %v", evs, err)
	}
	if err := MayCreatePAT(WithCredential(ctx, Credential{Kind: CredOIDC})); err != nil {
		t.Errorf("MayCreatePAT with an OIDC login = %v", err)
	}
}

// TestLoginOIDC: a first login makes the user and its identity; a later one finds it by the
// identity, not by the name; a disabled user is refused; a deleted user lets its identity go,
// and the next login makes a new user with a new uid.
func TestLoginOIDC(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	l := OIDCLogin{Issuer: "https://idp", Subject: "s1", Username: "olga", Groups: []string{"dev"}, Lifetime: time.Hour}
	first, err := users.LoginOIDC(ctx, l, Origin{})
	if err != nil || first.Kind != KindOIDC || first.Name != "olga" {
		t.Fatalf("first login = %+v, %v", first, err)
	}
	l.Username = "olga2"
	again, err := users.LoginOIDC(ctx, l, Origin{})
	if err != nil || again.UID != first.UID || again.Name != "olga" {
		t.Fatalf("second login = %+v, %v; want the same user, its name kept", again, err)
	}
	if err := users.Disable(ctx, "olga", Origin{}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.LoginOIDC(ctx, l, Origin{}); !errors.Is(err, ErrLoginRefused) {
		t.Errorf("a disabled user's login = %v, want ErrLoginRefused", err)
	}
	if err := users.Delete(ctx, "olga", Origin{}); err != nil {
		t.Fatal(err)
	}
	fresh, err := users.LoginOIDC(ctx, l, Origin{})
	if err != nil || fresh.UID == first.UID || fresh.Name != "olga2" {
		t.Fatalf("the login after the delete = %+v, %v; want a new user named by the claim", fresh, err)
	}
	groups, err := UserGroups(ctx, st, fresh, users.Now())
	if err != nil || len(groups) != 1 {
		t.Errorf("the new user's groups = %q, %v", groups, err)
	}
}

// TestLoginOIDCNames: a name a live user has, a malformed one and none refuse a first login.
func TestLoginOIDCNames(t *testing.T) {
	ctx := context.Background()
	users, _, _ := newUsers(t)
	if _, err := users.LoginOIDC(ctx, OIDCLogin{Issuer: "https://idp", Subject: "s1", Username: "olga", Lifetime: time.Hour}, Origin{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"olga", "", "deploy", "01a1b5e1-3e00-76b9-8c64-d89d2e318b76"} {
		other := OIDCLogin{Issuer: "https://idp", Subject: "s2", Username: name, Lifetime: time.Hour}
		if _, err := users.LoginOIDC(ctx, other, Origin{}); !errors.Is(err, ErrLoginRefused) {
			t.Errorf("a first login named %q = %v, want ErrLoginRefused", name, err)
		}
	}
}

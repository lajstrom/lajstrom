package npm

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// accounts are the real authenticator and tokens service over the pipeline's index.
type accounts struct {
	*auth.Authenticator
	*auth.Tokens
}

// loginRoutes are the npm routes with accounts, and the users alice (password "alice's
// password"), carol (disabled) and dave, who may not read the repo; authz decides.
func (p *pipeline) loginRoutes(t *testing.T, authz auth.Authorizer) http.Handler {
	t.Helper()
	ctx := t.Context()
	for _, u := range []struct {
		name     string
		disabled bool
	}{{"alice", false}, {"carol", true}, {"dave", false}} {
		hash, err := auth.HashPassword(u.name + "'s password")
		must(t, err)
		must(t, p.st.CreateUser(ctx, &index.User{Name: u.name, PasswordHash: hash, Kind: "local", Disabled: u.disabled}))
	}
	a := accounts{auth.NewAuthenticator(auth.Options{Index: p.st, Log: slog.New(slog.DiscardHandler)}), &auth.Tokens{Index: p.st}}
	deps := format.Deps{Index: p.st, Ingest: p.pipe, Providers: provider.Map{"local": p.fake}, Auth: authz, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{Accounts: a}.Routes(format.NewRouter(mux, Name, deps), deps)
	return mux
}

// readers lets every user but dave read, and nobody write.
type readers struct{}

func (readers) Allowed(_ context.Context, p auth.Principal, a auth.Action, _, _ string) (bool, error) {
	return p.Kind == auth.KindUser && a == auth.ActionRead && p != daveP, nil
}

var daveP auth.Principal

// loginBody is the body npm login sends.
func loginBody(name, password string) []byte {
	b, _ := json.Marshal(map[string]any{"_id": "org.couchdb.user:" + name, "name": name, "password": password,
		"type": "user", "roles": []string{}, "date": "2026-11-10T10:00:00.000Z"})
	return b
}

// TestLogin is npm login (#308): the password of the body buys a personal access token, which
// the tokens of the user list and which logs in; the request's own credentials do not count,
// a token is no password, and every refusal makes no token.
func TestLogin(t *testing.T) {
	ctx := t.Context()
	p := newPipeline(t)
	h := p.loginRoutes(t, readers{})
	dave, err := p.st.GetUser(ctx, "dave")
	must(t, err)
	daveP = auth.UserPrincipal(dave)
	path := "/npm/internal/-/user/org.couchdb.user:"

	// As npm sends it: with the stale token of another user in the request, which does not count.
	bob := auth.Principal{Kind: auth.KindUser, Name: "bob"}
	rr := as(h, bob, http.MethodPut, path+"alice", loginBody("alice", "alice's password"))
	var answer struct {
		OK    bool
		ID    string
		Token string
	}
	must(t, json.Unmarshal(rr.Body.Bytes(), &answer))
	if rr.Code != http.StatusCreated || !answer.OK || answer.ID != "org.couchdb.user:alice" || !strings.HasPrefix(answer.Token, auth.PATPrefix) {
		t.Fatalf("login: %d %s", rr.Code, rr.Body)
	}
	alice, err := p.st.GetUser(ctx, "alice")
	must(t, err)
	tokens, err := p.st.ListTokens(ctx, &alice.ID, nil)
	must(t, err)
	if len(tokens) != 1 || !strings.HasPrefix(tokens[0].Name, "npm login npm/internal ") || tokens[0].ExpiresAt == nil || len(tokens[0].Scopes) != 0 {
		t.Errorf("the tokens of alice: %+v", tokens)
	}
	audit, err := p.st.ListAudit(ctx, index.AuditFilter{Action: "token.create"})
	must(t, err)
	if len(audit) != 1 || audit[0].Principal != auth.UserPrincipal(alice).String() || !strings.Contains(string(audit[0].Details), `"createdBy":"password"`) {
		t.Errorf("the audit of the login: %+v", audit)
	}
	if again := as(h, bob, http.MethodPut, path+"alice", loginBody("alice", "alice's password")); again.Code != http.StatusCreated {
		t.Errorf("a second login in the same second: %d %s", again.Code, again.Body)
	}
}

// TestLoginRefused: a wrong password, an unknown or a disabled user, a token in place of the
// password are 401 alike, a body of another name 400, a user who may not read the repo 404;
// none makes a token. A server without accounts has no login.
func TestLoginRefused(t *testing.T) {
	ctx := t.Context()
	p := newPipeline(t)
	h := p.loginRoutes(t, readers{})
	dave, err := p.st.GetUser(ctx, "dave")
	must(t, err)
	daveP = auth.UserPrincipal(dave)
	path := "/npm/internal/-/user/org.couchdb.user:"
	token := auth.PATPrefix + strings.Repeat("a", 43)
	for _, tc := range []struct {
		name, user string
		body       []byte
		status     int
	}{
		{"wrong password", "alice", loginBody("alice", "wrong"), http.StatusUnauthorized},
		{"unknown user", "nobody", loginBody("nobody", "nobody's password"), http.StatusUnauthorized},
		{"disabled user", "carol", loginBody("carol", "carol's password"), http.StatusUnauthorized},
		{"a token as the password", "alice", loginBody("alice", token), http.StatusUnauthorized},
		{"another name in the body", "alice", loginBody("bob", "alice's password"), http.StatusBadRequest},
		{"not JSON", "alice", []byte("name=alice"), http.StatusBadRequest},
		{"a user who may not read the repo", "dave", loginBody("dave", "dave's password"), http.StatusNotFound},
	} {
		rr := as(h, auth.Anonymous, http.MethodPut, path+tc.user, tc.body)
		if rr.Code != tc.status || rr.Header().Get("Content-Type") != "application/json" || strings.Contains(rr.Body.String(), auth.PATPrefix) {
			t.Errorf("%s: %d %s, want %d", tc.name, rr.Code, rr.Body, tc.status)
		}
	}
	if audit, _ := p.st.ListAudit(ctx, index.AuditFilter{Action: "token.create"}); len(audit) != 0 {
		t.Errorf("%d tokens made by refused logins", len(audit))
	}
	if rr := do(p.routes(nil), http.MethodPut, path+"alice", loginBody("alice", "alice's password"), true); rr.Code != http.StatusNotImplemented {
		t.Errorf("a login without accounts: %d", rr.Code)
	}
}

// TestLogout is npm logout: the request's own personal access token is revoked; another token,
// or a request without one, is 404.
func TestLogout(t *testing.T) {
	ctx := t.Context()
	p := newPipeline(t)
	h := p.loginRoutes(t, readers{})
	login := func() (string, auth.Credential, auth.Principal) {
		rr := as(h, auth.Anonymous, http.MethodPut, "/npm/internal/-/user/org.couchdb.user:alice", loginBody("alice", "alice's password"))
		var answer struct{ Token string }
		must(t, json.Unmarshal(rr.Body.Bytes(), &answer))
		tk, err := p.st.GetTokenByHash(ctx, auth.HashSecret(answer.Token))
		must(t, err)
		alice, err := p.st.GetUser(ctx, "alice")
		must(t, err)
		return answer.Token, auth.Credential{Kind: auth.CredPAT, TokenID: tk.ID, UserID: alice.ID}, auth.UserPrincipal(alice)
	}
	mine, cred, alice := login()
	other, _, _ := login()
	logout := func(secret string, c auth.Credential) int {
		req := httptest.NewRequest(http.MethodDelete, "/npm/internal/-/user/token/"+secret, nil)
		req = req.WithContext(auth.WithCredential(auth.WithPrincipal(req.Context(), alice), c))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if code := logout(other, cred); code != http.StatusNotFound {
		t.Errorf("logout of another token: %d", code)
	}
	if code := logout(mine, auth.Credential{Kind: auth.CredPassword}); code != http.StatusNotFound {
		t.Errorf("logout with a password: %d", code)
	}
	if code := logout(mine, cred); code != http.StatusOK {
		t.Errorf("logout: %d", code)
	}
	for secret, revoked := range map[string]bool{mine: true, other: false} {
		tk, err := p.st.GetTokenByHash(ctx, auth.HashSecret(secret))
		if must(t, err); (tk.RevokedAt != nil) != revoked {
			t.Errorf("token %d revoked %v, want %v", tk.ID, tk.RevokedAt != nil, revoked)
		}
	}
}

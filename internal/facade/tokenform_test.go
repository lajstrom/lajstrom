package facade

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// formType is the Content-Type containerd sends with its form.
const formType = "application/x-www-form-urlencoded; charset=utf-8"

// passwordForm is the form containerd posts for user and secret, with the extra fields.
func passwordForm(user, secret string, extra ...string) string {
	v := url.Values{"grant_type": {"password"}, "username": {user}, "password": {secret},
		"service": {"example.com"}, "client_id": {"containerd-client"}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Add(extra[i], extra[i+1])
	}
	return v.Encode()
}

// oauthError is the body of a refused form (RFC 6749 §5.2).
type oauthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

// TestTokenForm checks POST /v2/token with the OAuth2 form, behind a principal the test sets as
// the middleware would after it checked the form's credential: the token and the answer of GET
// for the same principal and scopes, the scopes of the form only, one parameter holding several
// as containerd sends them; no refresh_token.
func TestTokenForm(t *testing.T) {
	e := newEnv(t, envOptions{authz: authorizer{"bob": {"raw/assets": {auth.ActionRead}}}})
	scopes := []string{"repository:raw/assets/hello:pull,push", "repository:raw/none/x:pull", "registry:catalog:*"}
	for _, user := range []string{"alice", "bob"} {
		get := e.do(e.h, http.MethodGet, "/v2/token?service=example.com&scope="+strings.Join(scopes, "&scope="), nil, principalHeader, user)
		wantStatus(t, "GET /v2/token as "+user, get, http.StatusOK)
		body := passwordForm(user, "checked by the middleware", "scope", scopes[0]+" "+scopes[1], "scope", scopes[2])
		post := e.do(e.h, http.MethodPost, "/v2/token?scope=repository:raw/assets/other:pull", strings.NewReader(body),
			principalHeader, user, "Content-Type", formType)
		wantStatus(t, "POST /v2/token as "+user, post, http.StatusOK)
		wantHeader(t, "POST /v2/token", post, "Cache-Control", "no-store")
		wantHeader(t, "POST /v2/token", post, "Content-Type", "application/json")
		var g, p tokenAnswer
		must(t, json.Unmarshal(get.Body.Bytes(), &g))
		must(t, json.Unmarshal(post.Body.Bytes(), &p))
		if p.Token == "" || p.AccessToken != p.Token || p.ExpiresIn != g.ExpiresIn || p.IssuedAt != g.IssuedAt {
			t.Errorf("POST as %s: answer %+v, GET's %+v", user, p, g)
		}
		gc, pc := payload(t, g.Token), payload(t, p.Token)
		for _, claim := range []string{"sub", "iss", "aud", "access"} {
			if fmt.Sprint(gc[claim]) != fmt.Sprint(pc[claim]) {
				t.Errorf("POST as %s: %s %v, GET's %v", user, claim, pc[claim], gc[claim])
			}
		}
		if strings.Contains(post.Body.String(), "refresh_token") {
			t.Errorf("POST as %s: %s", user, post.Body)
		}
	}
}

// TestTokenFormRefused checks the refusals of the form, OAuth2's with Cache-Control: no-store
// and no secret repeated, and the bound: a form of exactly MaxFormSize bytes is taken.
func TestTokenFormRefused(t *testing.T) {
	e := newEnv(t, envOptions{})
	big := url.Values{"grant_type": {"password"}, "username": {"alice"}, "password": {"x"}, "client_id": {strings.Repeat("c", token.MaxFormSize)}}.Encode()
	for _, tt := range []struct {
		name, ctype, body string
		length            string // the Content-Length header: "" for the body's, -1 for chunked
		status            int
		code              string
	}{
		{"another media type", "text/plain", passwordForm("alice", "hunter2"), "", http.StatusBadRequest, token.ErrorInvalidRequest},
		{"no media type", "", passwordForm("alice", "hunter2"), "", http.StatusBadRequest, token.ErrorInvalidRequest},
		{"JSON", "application/json", `{"grant_type":"password"}`, "", http.StatusBadRequest, token.ErrorInvalidRequest},
		{"a refresh token, as an identityToken is sent", formType, "grant_type=refresh_token&refresh_token=ljs_pat_x&service=example.com", "",
			http.StatusBadRequest, token.ErrorUnsupportedGrantType},
		{"client credentials", formType, "grant_type=client_credentials", "", http.StatusBadRequest, token.ErrorUnsupportedGrantType},
		{"an empty grant type", formType, "grant_type=&username=alice&password=hunter2", "", http.StatusBadRequest, token.ErrorUnsupportedGrantType},
		{"no grant type", formType, "username=alice&password=hunter2", "", http.StatusBadRequest, token.ErrorInvalidRequest},
		{"no user name", formType, "grant_type=password&password=hunter2", "", http.StatusBadRequest, token.ErrorInvalidRequest},
		{"no password", formType, "grant_type=password&username=alice", "", http.StatusBadRequest, token.ErrorInvalidRequest},
		{"two grant types", formType, passwordForm("alice", "hunter2", "grant_type", "refresh_token"), "", http.StatusBadRequest, token.ErrorInvalidRequest},
		{"two passwords", formType, passwordForm("alice", "hunter2", "password", "other"), "", http.StatusBadRequest, token.ErrorInvalidRequest},
		{"two services", formType, passwordForm("alice", "hunter2", "service", "other.example"), "", http.StatusBadRequest, token.ErrorInvalidRequest},
		{"not a form", formType, "grant_type=password&username=%zz", "", http.StatusBadRequest, token.ErrorInvalidRequest},
		{"over the bound, by its Content-Length", formType, big, "", http.StatusRequestEntityTooLarge, token.ErrorInvalidRequest},
		{"over the bound, chunked", formType, big, "-1", http.StatusRequestEntityTooLarge, token.ErrorInvalidRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hdr := []string{principalHeader, "alice"}
			if tt.ctype != "" {
				hdr = append(hdr, "Content-Type", tt.ctype)
			}
			if tt.length != "" {
				hdr = append(hdr, "Content-Length", tt.length)
			}
			rr := e.do(e.h, http.MethodPost, "/v2/token", strings.NewReader(tt.body), hdr...)
			wantStatus(t, tt.name, rr, tt.status)
			wantHeader(t, tt.name, rr, "Cache-Control", "no-store")
			var b oauthError
			if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil || b.Error != tt.code || b.Description == "" {
				t.Errorf("%s: body %s, want the error %s (%v)", tt.name, rr.Body, tt.code, err)
			}
			if strings.Contains(rr.Body.String(), "hunter2") || strings.Contains(rr.Body.String(), "ljs_pat_x") {
				t.Errorf("%s: the answer repeats a secret: %s", tt.name, rr.Body)
			}
		})
	}

	exact := url.Values{"grant_type": {"password"}, "username": {"alice"}, "password": {"x"}, "client_id": {""}}
	exact.Set("client_id", strings.Repeat("c", token.MaxFormSize-len(exact.Encode())))
	rr := e.do(e.h, http.MethodPost, "/v2/token", strings.NewReader(exact.Encode()), principalHeader, "alice", "Content-Type", formType)
	wantStatus(t, "a form of exactly the bound", rr, http.StatusOK)
}

// TestTokenFormUnchecked checks a POST that no authenticator checked, 401 with the Basic
// challenge, and a form of more than 100 repositories, GET's 400 UNSUPPORTED.
func TestTokenFormUnchecked(t *testing.T) {
	e := newEnv(t, envOptions{})
	rr := e.do(e.h, http.MethodPost, "/v2/token", strings.NewReader(passwordForm("alice", "hunter2")), "Content-Type", formType)
	wantError(t, "a form no authenticator checked", rr, http.StatusUnauthorized, codeUnauthorized)
	wantHeader(t, "a form no authenticator checked", rr, "WWW-Authenticate", `Basic realm="lajstrom"`)

	many := make([]string, maxTokenScopes+1)
	for i := range many {
		many[i] = fmt.Sprintf("repository:raw/assets/p%d:pull", i)
	}
	rr = e.do(e.h, http.MethodPost, "/v2/token", strings.NewReader(passwordForm("alice", "hunter2", "scope", strings.Join(many, " "))),
		principalHeader, "alice", "Content-Type", formType)
	wantError(t, "a form of 101 repositories", rr, http.StatusBadRequest, codeUnsupported)
}

// TestTokenFormThroughServer runs the server, the authenticator and the facade, and asks for a
// token with GET and Basic credentials and with POST and the form, for a password, a personal
// access token with read and a deploy token with read, each with the same scopes: the two
// tokens have the same subject and access. A wrong password is 401 to both, with the same body
// and challenge.
func TestTokenFormThroughServer(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	hash, err := auth.HashPassword("correct horse battery staple")
	must(t, err)
	must(t, e.st.CreateUser(ctx, &index.User{Name: "alice", PasswordHash: hash, Kind: auth.KindLocal}))
	alice, err := e.st.GetUser(ctx, "alice")
	must(t, err)
	pat, deploy := auth.PATPrefix+strings.Repeat("p4t", 14)+"x", auth.DeployPrefix+strings.Repeat("d3p", 14)+"x"
	expires := time.Now().Add(time.Hour)
	must(t, e.st.CreateToken(ctx, &index.Token{Kind: auth.TokenPAT, UserID: &alice.ID, Name: "read", Hash: auth.HashSecret(pat),
		Scopes: []string{"read"}, ExpiresAt: &expires}))
	must(t, e.st.CreateToken(ctx, &index.Token{Kind: auth.TokenDeploy, RepoID: &e.raw.ID, Name: "ci", Hash: auth.HashSecret(deploy), Scopes: []string{"read"}}))
	srv, err := server.New(server.Options{Listen: "127.0.0.1:0", Facade: e.h, Log: slog.New(slog.DiscardHandler),
		Authenticator: auth.NewAuthenticator(auth.Options{Index: e.st})})
	must(t, err)
	serve := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, r)
		return rr
	}
	scope := "repository:raw/assets/hello:pull,push repository:oci/images/app:pull"
	for _, c := range []struct{ name, user, secret, sub string }{
		{"a password", "alice", "correct horse battery staple", "user:" + alice.UID},
		{"a personal access token with read", "alice", pat, "user:" + alice.UID},
		{"a deploy token with read", "containerd", deploy, "token:"},
		{"a wrong password", "alice", "a wrong password", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			get := httptest.NewRequest(http.MethodGet, "/v2/token?service=example.com&scope="+url.QueryEscape(scope), nil)
			get.SetBasicAuth(c.user, c.secret)
			post := httptest.NewRequest(http.MethodPost, "/v2/token", strings.NewReader(passwordForm(c.user, c.secret, "scope", scope)))
			post.Header.Set("Content-Type", formType)
			g, p := serve(get), serve(post)
			if c.sub == "" {
				wantStatus(t, "GET", g, http.StatusUnauthorized)
				if p.Code != g.Code || p.Body.String() != g.Body.String() || p.Header().Get("WWW-Authenticate") != g.Header().Get("WWW-Authenticate") {
					t.Errorf("POST: %d %s %q; GET: %d %s %q", p.Code, p.Body, p.Header().Get("WWW-Authenticate"),
						g.Code, g.Body, g.Header().Get("WWW-Authenticate"))
				}
				return
			}
			wantStatus(t, "GET", g, http.StatusOK)
			wantStatus(t, "POST", p, http.StatusOK)
			var ga, pa tokenAnswer
			must(t, json.Unmarshal(g.Body.Bytes(), &ga))
			must(t, json.Unmarshal(p.Body.Bytes(), &pa))
			gc, pc := payload(t, ga.Token), payload(t, pa.Token)
			if sub := fmt.Sprint(pc["sub"]); !strings.HasPrefix(sub, c.sub) || sub != fmt.Sprint(gc["sub"]) ||
				fmt.Sprint(pc["access"]) != fmt.Sprint(gc["access"]) {
				t.Errorf("POST: sub %v access %v; GET: sub %v access %v", pc["sub"], pc["access"], gc["sub"], gc["access"])
			}
		})
	}
}

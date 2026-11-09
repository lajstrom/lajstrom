package facade

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/server"
)

// realm is the challenge of a facade without an external URL, for a request to httptest's host.
const realm = `Bearer realm="http://example.com/v2/token",service="example.com"`

// tokenAnswer is the body of GET /v2/token.
type tokenAnswer struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	IssuedAt    string `json:"issued_at"`
}

// getToken asks h for a token with the scopes, as user (none: anonymous), and returns it.
func (e *env) getToken(h http.Handler, user string, scopes ...string) string {
	e.t.Helper()
	target := "/v2/token?service=example.com"
	for _, s := range scopes {
		target += "&scope=" + s
	}
	rr := e.do(h, http.MethodGet, target, nil, principalHeader, user)
	wantStatus(e.t, "GET /v2/token", rr, http.StatusOK)
	var a tokenAnswer
	must(e.t, json.Unmarshal(rr.Body.Bytes(), &a))
	return a.Token
}

// issue signs a token for p with access, with the env's key, as the token endpoint would.
func (e *env) issue(p auth.Principal, access ...token.Access) string {
	e.t.Helper()
	tok, err := token.NewIssuer(token.Options{Keys: e.tokens, Now: e.clock.Now}).Issue(e.t.Context(), p, "example.com", access)
	must(e.t, err)
	return tok.Raw
}

// repoAccess is the access entry of a repository with actions.
func repoAccess(name string, actions ...string) token.Access {
	return token.Access{Type: token.TypeRepository, Name: name, Actions: actions}
}

// payload decodes the claims of a JWT.
func payload(t *testing.T, raw string) map[string]any {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("a JWT of %d segments", len(parts))
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	must(t, err)
	var m map[string]any
	must(t, json.Unmarshal(b, &m))
	return m
}

// TestTokenEndpoint checks GET /v2/token: the body and headers; the scopes reduced by the
// Authorizer for a user and for anonymous, every scope granted without one; the scope of a repo
// that does not exist without actions, as that of a repo the principal may not read.
func TestTokenEndpoint(t *testing.T) {
	scopes := []string{"repository:raw/assets/hello:pull,push", "repository:raw/none/x:pull", "registry:catalog:*", "repository:oci/images/a:delete"}
	for _, tt := range []struct {
		name   string
		authz  auth.Authorizer
		user   string
		sub    string
		access string
	}{
		{"anonymous, no Authorizer", nil, "", "anonymous",
			"[map[actions:[*] name:catalog type:registry] map[actions:[delete] name:oci/images/a type:repository] " +
				"map[actions:[pull push] name:raw/assets/hello type:repository] map[actions:[] name:raw/none/x type:repository]]"},
		{"alice, no Authorizer", nil, "alice", userP("alice").String(),
			"[map[actions:[*] name:catalog type:registry] map[actions:[delete] name:oci/images/a type:repository] " +
				"map[actions:[pull push] name:raw/assets/hello type:repository] map[actions:[] name:raw/none/x type:repository]]"},
		{"bob, a reader of raw/assets (O-12)", authorizer{"bob": {"raw/assets": {auth.ActionRead}}}, "bob", userP("bob").String(),
			"[map[actions:[*] name:catalog type:registry] map[actions:[] name:oci/images/a type:repository] " +
				"map[actions:[pull] name:raw/assets/hello type:repository] map[actions:[] name:raw/none/x type:repository]]"},
		{"anonymous, an Authorizer that grants it nothing", authorizer{}, "", "anonymous",
			"[map[actions:[*] name:catalog type:registry] map[actions:[] name:oci/images/a type:repository] " +
				"map[actions:[] name:raw/assets/hello type:repository] map[actions:[] name:raw/none/x type:repository]]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, envOptions{authz: tt.authz})
			target := "/v2/token?service=example.com&scope=" + strings.Join(scopes, "&scope=")
			rr := e.do(e.h, http.MethodGet, target, nil, principalHeader, tt.user)
			wantStatus(t, "GET /v2/token", rr, http.StatusOK)
			wantHeader(t, "GET /v2/token", rr, "Cache-Control", "no-store")
			wantHeader(t, "GET /v2/token", rr, "Content-Type", "application/json")
			var a tokenAnswer
			must(t, json.Unmarshal(rr.Body.Bytes(), &a))
			if a.Token == "" || a.AccessToken != a.Token || a.ExpiresIn != 1200 || a.IssuedAt != "2026-11-07T10:00:00Z" {
				t.Errorf("answer %+v", a)
			}
			claims := payload(t, a.Token)
			if claims["sub"] != tt.sub || fmt.Sprint(claims["aud"]) != "[example.com]" || claims["iss"] != "example.com" ||
				fmt.Sprint(claims["access"]) != tt.access {
				t.Errorf("claims %v, want sub %s and access %s", claims, tt.sub, tt.access)
			}
		})
	}
}

// TestTokenEndpointService checks the service of the tokens: the configured one whatever the
// client sends, the external URL's host when it is set; a request without scopes gets a token
// without access; only GET.
func TestTokenEndpointService(t *testing.T) {
	e := newEnv(t, envOptions{})
	tok := e.getToken(e.h, "", "repository:raw/assets/a:pull")
	rr := e.do(e.h, http.MethodGet, "/v2/token?service=other.example&scope=repository:raw/assets/a:pull", nil)
	var a tokenAnswer
	must(t, json.Unmarshal(rr.Body.Bytes(), &a))
	if aud := fmt.Sprint(payload(t, a.Token)["aud"]); aud != "[example.com]" || fmt.Sprint(payload(t, tok)["aud"]) != aud {
		t.Errorf("a token asked for another service is for %s, want example.com", aud)
	}
	rr = e.do(e.h, http.MethodGet, "/v2/token", nil)
	must(t, json.Unmarshal(rr.Body.Bytes(), &a))
	if fmt.Sprint(payload(t, a.Token)["access"]) != "[]" {
		t.Errorf("a token without scopes: %v", payload(t, a.Token))
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodHead} {
		rr := e.do(e.h, m, "/v2/token", strings.NewReader("grant_type=password"), "Content-Type", "application/x-www-form-urlencoded")
		if m == http.MethodHead {
			wantStatus(t, "HEAD /v2/token", rr, http.StatusMethodNotAllowed)
			continue
		}
		if msg := wantError(t, m+" /v2/token", rr, http.StatusMethodNotAllowed, codeUnsupported); !strings.Contains(msg, "clients get a token with GET") {
			t.Errorf("%s /v2/token: %q", m, msg)
		}
	}

	ext := newEnv(t, envOptions{externalURL: "https://art.test:8443"})
	rr = ext.do(ext.h, http.MethodGet, "/v2/token?service=art.test:8443", nil)
	must(t, json.Unmarshal(rr.Body.Bytes(), &a))
	if claims := payload(t, a.Token); fmt.Sprint(claims["aud"]) != "[art.test:8443]" || claims["iss"] != "art.test:8443" {
		t.Errorf("with an external URL: %v", claims)
	}
	rr = ext.do(ext.h, http.MethodGet, "/v2/", nil)
	wantHeader(t, "the challenge with an external URL", rr, "WWW-Authenticate", `Bearer realm="https://art.test:8443/v2/token",service="art.test:8443"`)
}

// failingAuthorizer cannot decide.
type failingAuthorizer struct{}

func (failingAuthorizer) Allowed(context.Context, auth.Principal, auth.Action, string, string) (bool, error) {
	return false, errors.New("bindings unreachable")
}

// reposDown is the store whose repo reads fail.
type reposDown struct{ index.Store }

func (reposDown) GetRepo(context.Context, string, string) (*index.Repo, error) {
	return nil, errors.New("database is gone")
}

// TestTokenEndpointFails checks the failures to decide: an Authorizer that fails and an index
// that fails are 500, with the cause logged and not answered.
func TestTokenEndpointFails(t *testing.T) {
	e := newEnv(t, envOptions{authz: failingAuthorizer{}})
	rr := e.do(e.h, http.MethodGet, "/v2/token?scope=repository:raw/assets/a:pull", nil)
	wantError(t, "an Authorizer that fails", rr, http.StatusInternalServerError, codeUnknown)
	if !strings.Contains(e.logs.String(), "bindings unreachable") || strings.Contains(rr.Body.String(), "bindings") {
		t.Errorf("answer %s, log:\n%s", rr.Body, e.logs)
	}
	h := New(Options{Index: reposDown{e.st}, Tokens: e.tokens, Log: slog.New(slog.NewTextHandler(e.logs, nil))})
	rr = e.do(h, http.MethodGet, "/v2/token?scope=repository:raw/assets/a:pull", nil)
	wantError(t, "an index that fails", rr, http.StatusInternalServerError, codeUnknown)
	h = New(Options{Index: e.st, Tokens: failingKeys{}, Log: slog.New(slog.NewTextHandler(e.logs, nil))})
	rr = e.do(h, http.MethodGet, "/v2/token", nil)
	wantError(t, "keys that fail to sign", rr, http.StatusInternalServerError, codeUnknown)
	rr = e.do(h, http.MethodGet, "/v2/oci/images/x/tags/list", nil, "Authorization", "Bearer "+e.issue(auth.Anonymous))
	wantError(t, "keys that fail to load", rr, http.StatusInternalServerError, codeUnknown)
}

// failingKeys are token keys that cannot be read.
type failingKeys struct{}

func (failingKeys) TokenSigner(context.Context) (*keys.TokenKey, error) {
	return nil, errors.New("keys unreachable")
}

func (failingKeys) TokenVerifiers(context.Context) ([]*keys.TokenKey, error) {
	return nil, errors.New("keys unreachable")
}

// TestBearer checks the requests with a bearer: a good one decides by its claims, even against
// an Authorizer that allows nothing, and its principal is the request's; a bad one is 401 with
// the challenge; one that lacks the repository 401 with the scope and insufficient_scope; one
// that lacks the action 403 DENIED; GET /v2/ with any good one 200.
func TestBearer(t *testing.T) {
	e := newEnv(t, envOptions{authz: authorizer{}}) // the Authorizer allows nobody anything
	alice := userP("alice")
	e.seedPackage("hello", "v1")
	pull := e.issue(alice, repoAccess("raw/assets/hello", "pull"))
	pushPull := e.issue(alice, repoAccess("oci/images/app", "pull", "push"))
	bearer := func(raw string) []string { return []string{"Authorization", "Bearer " + raw} }

	rr := e.do(e.h, http.MethodGet, "/v2/raw/assets/hello/tags/list", nil, bearer(pull)...)
	wantStatus(t, "tags/list with pull", rr, http.StatusOK)
	rr = e.do(e.h, http.MethodPost, uploads, nil, bearer(pushPull)...)
	wantStatus(t, "an upload with push", rr, http.StatusAccepted)
	s, err := e.st.GetSession(t.Context(), e.oci.ID, uploadKind+uploadID(t, rr))
	if err != nil || s.Principal != userP("alice").String() {
		t.Errorf("the session of the upload: %+v, %v; want the principal of the token", s, err)
	}
	wantStatus(t, "GET /v2/ with a bearer", e.do(e.h, http.MethodGet, "/v2/", nil, bearer(pull)...), http.StatusOK)
	wantStatus(t, "GET /v2/ with an anonymous bearer", e.do(e.h, http.MethodGet, "/v2/", nil, bearer(e.issue(auth.Anonymous))...), http.StatusOK)
	wantStatus(t, "lower-case scheme", e.do(e.h, http.MethodGet, "/v2/raw/assets/hello/tags/list", nil, "Authorization", "bearer "+pull), http.StatusOK)

	changed := []byte(pull)
	changed[len(changed)-3] ^= 0x01
	for _, tt := range []struct {
		name, method, target, token string
		status                      int
		code, challenge             string
	}{
		{"a changed token", http.MethodGet, "/v2/raw/assets/hello/tags/list", string(changed), http.StatusUnauthorized, codeUnauthorized,
			realm + `,scope="repository:raw/assets/hello:pull"`},
		{"garbage", http.MethodGet, "/v2/", "garbage", http.StatusUnauthorized, codeUnauthorized, realm},
		{"a bad token on the catalog", http.MethodGet, "/v2/_catalog", "garbage", http.StatusUnauthorized, codeUnauthorized,
			realm + `,scope="registry:catalog:*"`},
		{"another repository", http.MethodGet, "/v2/raw/assets/other/tags/list", pull, http.StatusUnauthorized, codeUnauthorized,
			realm + `,scope="repository:raw/assets/other:pull",error="insufficient_scope"`},
		{"pull only, a push (O-12)", http.MethodPost, "/v2/raw/assets/hello/blobs/uploads/", pull, http.StatusForbidden, codeDenied, ""},
		{"pull only, a manifest PUT", http.MethodPut, "/v2/raw/assets/hello/manifests/v2", pull, http.StatusForbidden, codeDenied, ""},
		{"pull only, a delete", http.MethodDelete, "/v2/raw/assets/hello/manifests/v1", pull, http.StatusForbidden, codeDenied, ""},
		{"push on another name, a read", http.MethodGet, "/v2/oci/images/app/referrers/" + string(zeroDigest), pull,
			http.StatusUnauthorized, codeUnauthorized, realm + `,scope="repository:oci/images/app:pull",error="insufficient_scope"`},
		{"an expired token", http.MethodGet, "/v2/raw/assets/hello/tags/list", e.expired(alice), http.StatusUnauthorized, codeUnauthorized,
			realm + `,scope="repository:raw/assets/hello:pull"`},
	} {
		rr := e.do(e.h, tt.method, tt.target, strings.NewReader("{}"), append(bearer(tt.token), "Content-Type", ocispecManifest)...)
		msg := wantError(t, tt.name, rr, tt.status, tt.code)
		wantHeader(t, tt.name, rr, "WWW-Authenticate", tt.challenge)
		if strings.Contains(msg, "signature") || strings.Contains(msg, "expired") {
			t.Errorf("%s: the answer tells why: %q", tt.name, msg)
		}
	}
	if !strings.Contains(e.logs.String(), "bearer token refused") {
		t.Errorf("the refusals are not logged:\n%s", e.logs)
	}
	if strings.Contains(e.logs.String(), pull[strings.LastIndexByte(pull, '.')+1:]) {
		t.Error("the log shows a token's signature")
	}
}

// expired returns a token for p that expired an hour ago.
func (e *env) expired(p auth.Principal) string {
	e.t.Helper()
	tok, err := token.NewIssuer(token.Options{Keys: e.tokens, Now: func() time.Time { return e.clock.Now().Add(-time.Hour) }}).
		Issue(e.t.Context(), p, "example.com", []token.Access{repoAccess("raw/assets/hello", "pull")})
	must(e.t, err)
	return tok.Raw
}

// TestBearerAccessLog checks, through the server, that the principal of a bearer is the
// request's in the access log line, and a refused bearer anonymous.
func TestBearerAccessLog(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.seedPackage("hello", "v1")
	var logged bytes.Buffer
	srv, err := server.New(server.Options{Listen: "127.0.0.1:0", Facade: e.h, Log: slog.New(slog.NewJSONHandler(&logged, nil))})
	must(t, err)
	raw := e.issue(userP("alice"), repoAccess("raw/assets/hello", "pull"))
	for _, tt := range []struct{ bearer, principal string }{{raw, userP("alice").String()}, {"garbage", "anonymous"}} {
		logged.Reset()
		r := httptest.NewRequest(http.MethodGet, "/v2/raw/assets/hello/tags/list", nil)
		r.Header.Set("Authorization", "Bearer "+tt.bearer)
		srv.Handler().ServeHTTP(httptest.NewRecorder(), r)
		var line map[string]any
		for l := range strings.SplitSeq(strings.TrimSpace(logged.String()), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == "request" {
				line = m
			}
		}
		if line == nil || line["principal"] != tt.principal {
			t.Errorf("access log %v, want the principal %s", line, tt.principal)
		}
		if strings.Contains(logged.String(), raw) {
			t.Error("the log shows the token")
		}
	}
}

// TestBearerMount checks a cross-repo mount with a bearer: a token without the source is 401
// naming both repositories and insufficient_scope, so that the client gets a wider token,
// whether the source exists or not; one with both mounts; one that names the source without
// pull, or names one that does not exist, gets a plain session; the same repository needs no
// second scope.
func TestBearerMount(t *testing.T) {
	e := newEnv(t, envOptions{})
	d := e.pushBlob("oci/images/base", []byte("shared layer"))
	alice := userP("alice")
	target := uploads + "?mount=" + d.String() + "&from=oci/images/base"
	rr := e.do(e.h, http.MethodPost, target, nil, "Authorization", "Bearer "+e.issue(alice, repoAccess("oci/images/app", "pull", "push")))
	wantError(t, "a mount with one scope", rr, http.StatusUnauthorized, codeUnauthorized)
	wantHeader(t, "a mount with one scope", rr, "WWW-Authenticate",
		realm+`,scope="repository:oci/images/app:pull,push repository:oci/images/base:pull",error="insufficient_scope"`)

	both := e.getToken(e.h, "alice", "repository:oci/images/app:pull,push", "repository:oci/images/base:pull")
	rr = e.do(e.h, http.MethodPost, target, nil, "Authorization", "Bearer "+both)
	wantCreated(t, "a mount with two scopes", rr, d)

	pushOnly := e.issue(alice, repoAccess("oci/images/app", "pull", "push"), repoAccess("oci/images/base", "push"))
	rr = e.do(e.h, http.MethodPost, uploads+"?mount="+d.String()+"&from=oci/images/base", nil, "Authorization", "Bearer "+pushOnly)
	wantStatus(t, "a mount from a source without pull", rr, http.StatusAccepted)

	rr = e.do(e.h, http.MethodPost, uploads+"?mount="+d.String()+"&from=oci/images/app", nil,
		"Authorization", "Bearer "+e.issue(alice, repoAccess("oci/images/app", "pull", "push")))
	wantCreated(t, "a mount from the same repository", rr, d)
	rr = e.do(e.h, http.MethodPost, uploads+"?mount="+d.String()+"&from=oci/none/base", nil,
		"Authorization", "Bearer "+e.issue(alice, repoAccess("oci/images/app", "pull", "push")))
	wantError(t, "a mount from a repo that does not exist, with one scope", rr, http.StatusUnauthorized, codeUnauthorized)
	wantHeader(t, "a mount from a repo that does not exist, with one scope", rr, "WWW-Authenticate",
		realm+`,scope="repository:oci/images/app:pull,push repository:oci/none/base:pull",error="insufficient_scope"`)
	rr = e.do(e.h, http.MethodPost, uploads+"?mount="+d.String()+"&from=oci/none/base", nil,
		"Authorization", "Bearer "+e.getToken(e.h, "alice", "repository:oci/images/app:pull,push", "repository:oci/none/base:pull"))
	wantStatus(t, "a mount from a repo that does not exist", rr, http.StatusAccepted)
}

// TestBearerExistence checks X-05 on /v2: every answer about a repo that alice may not read is
// the answer about a repo that does not exist, with and without a bearer from the token
// endpoint: 404 NAME_UNKNOWN to alice, 401 to the anonymous principal, 401 with
// insufficient_scope for a token that does not name the repository, a plain session for a mount
// from it.
func TestBearerExistence(t *testing.T) {
	e := newEnv(t, envOptions{authz: authorizer{"alice": {"raw/assets": {auth.ActionRead, auth.ActionWrite}}}})
	d := e.pushBlob("oci/images/app", []byte("a layer"))
	const (
		tags  = "/v2/{repo}/tags/list"
		mount = "/v2/raw/assets/hello/blobs/uploads/?mount={digest}&from={repo}"
	)
	for _, tt := range []struct {
		name, user, method, target string
		bearer                     bool
		scopes                     []string
		status                     int
		code, challenge            string
	}{
		{"alice", "alice", http.MethodGet, tags, false, nil, http.StatusNotFound, codeNameUnknown, ""},
		{"anonymous", "", http.MethodGet, tags, false, nil, http.StatusUnauthorized, codeUnauthorized, realm + `,scope="repository:{repo}:pull"`},
		{"alice with a token for it", "alice", http.MethodGet, tags, true, []string{"repository:{repo}:pull,push"},
			http.StatusNotFound, codeNameUnknown, ""},
		{"alice with a token for it, a push", "alice", http.MethodPost, "/v2/{repo}/blobs/uploads/", true, []string{"repository:{repo}:pull,push"},
			http.StatusNotFound, codeNameUnknown, ""},
		{"anonymous with a token for it", "", http.MethodGet, tags, true, []string{"repository:{repo}:pull"},
			http.StatusUnauthorized, codeUnauthorized, realm + `,scope="repository:{repo}:pull"`},
		{"alice with a token for another", "alice", http.MethodGet, tags, true, []string{"repository:raw/assets/hello:pull"},
			http.StatusUnauthorized, codeUnauthorized, realm + `,scope="repository:{repo}:pull",error="insufficient_scope"`},
		{"a mount from it", "alice", http.MethodPost, mount, true, []string{"repository:raw/assets/hello:pull,push", "repository:{repo}:pull"},
			http.StatusAccepted, "", ""},
		{"a mount from it, a token that does not name it", "alice", http.MethodPost, mount, true, []string{"repository:raw/assets/hello:pull,push"},
			http.StatusUnauthorized, codeUnauthorized, realm + `,scope="repository:raw/assets/hello:pull,push repository:{repo}:pull",error="insufficient_scope"`},
	} {
		for _, repo := range []string{"oci/images/app", "oci/none/app"} {
			what := tt.name + " on " + repo
			r := strings.NewReplacer("{repo}", repo, "{digest}", d.String())
			var hdr []string
			if tt.user != "" {
				hdr = []string{principalHeader, tt.user}
			}
			if tt.bearer {
				scopes := make([]string, len(tt.scopes))
				for i, sc := range tt.scopes {
					scopes[i] = r.Replace(sc)
				}
				hdr = []string{"Authorization", "Bearer " + e.getToken(e.h, tt.user, scopes...)}
			}
			rr := e.do(e.h, tt.method, r.Replace(tt.target), nil, hdr...)
			if tt.code == "" {
				wantStatus(t, what, rr, tt.status)
			} else {
				wantError(t, what, rr, tt.status, tt.code)
			}
			wantHeader(t, what, rr, "WWW-Authenticate", r.Replace(tt.challenge))
		}
	}
}

// TestBearerCatalog checks the catalog under a bearer: the token's view, the repositories it
// grants pull on; with registry:catalog:*, every repository the principal may read.
func TestBearerCatalog(t *testing.T) {
	e := newEnv(t, envOptions{authz: authorizer{"alice": {"raw/assets": {auth.ActionRead}}}})
	e.seedPackage("hello", "v1")
	e.seedPackage("world", "v1")
	must(t, e.st.UpsertOCIRepo(t.Context(), &index.OCIRepo{RepoID: e.oci.ID, Path: "oci/images/app", SeenAt: e.clock.Now()}))
	alice := userP("alice")
	for _, tt := range []struct {
		name   string
		access []token.Access
		want   string
	}{
		{"pull on one", []token.Access{repoAccess("raw/assets/hello", "pull"), repoAccess("raw/assets/world", "push")}, `["raw/assets/hello"]`},
		{"pull on a repository of a repo the Authorizer denies", []token.Access{repoAccess("oci/images/app", "pull")}, `["oci/images/app"]`},
		{"nothing", nil, `[]`},
		{"the catalog scope", []token.Access{{Type: token.TypeRegistry, Name: "catalog", Actions: []string{"*"}}},
			`["raw/assets/hello","raw/assets/world"]`},
	} {
		rr := e.do(e.h, http.MethodGet, "/v2/_catalog", nil, "Authorization", "Bearer "+e.issue(alice, tt.access...))
		if want := `{"repositories":` + tt.want + "}\n"; rr.Code != http.StatusOK || rr.Body.String() != want {
			t.Errorf("%s: %d %s, want %s", tt.name, rr.Code, rr.Body, want)
		}
	}
}

// TestTokenAcrossReplicas is H-12 over the memory store: two replicas, each with token keys of
// its own over one database and one master key, the token of one accepted by the other; a
// replica with an ephemeral key of its own refuses it.
func TestTokenAcrossReplicas(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.seedPackage("hello", "v1")
	m, err := keys.ParseMaster(strings.Repeat("0123456789abcdef", 4))
	must(t, err)
	replica := func() http.Handler {
		tk, err := keys.NewTokens(keys.TokenOptions{Index: e.st, Master: m, Now: e.clock.Now, Log: slog.New(slog.DiscardHandler)})
		must(t, err)
		return e.replicaWith(tk)
	}
	a, b := replica(), replica()
	raw := e.getToken(a, "alice", "repository:raw/assets/hello:pull")
	wantStatus(t, "A's token on B", e.do(b, http.MethodGet, "/v2/raw/assets/hello/tags/list", nil, "Authorization", "Bearer "+raw), http.StatusOK)
	wantStatus(t, "A's token on A", e.do(a, http.MethodGet, "/v2/raw/assets/hello/tags/list", nil, "Authorization", "Bearer "+raw), http.StatusOK)
	lone := e.replicaWith(keys.NewEphemeralTokens())
	wantStatus(t, "A's token on a replica with an ephemeral key", e.do(lone, http.MethodGet, "/v2/raw/assets/hello/tags/list", nil,
		"Authorization", "Bearer "+raw), http.StatusUnauthorized)
}

// TestRequestScope is the table of the scope of the challenge.
func TestRequestScope(t *testing.T) {
	for _, tt := range []struct{ method, target, want string }{
		{http.MethodGet, "/v2/", ""},
		{http.MethodGet, "/v2", ""},
		{http.MethodGet, "/v2/token", ""},
		{http.MethodGet, "/v2/_catalog", "registry:catalog:*"},
		{http.MethodGet, "/v2/raw/assets/a/manifests/v1", "repository:raw/assets/a:pull"},
		{http.MethodHead, "/v2/raw/assets/a/blobs/" + string(zeroDigest), "repository:raw/assets/a:pull"},
		{http.MethodGet, "/v2/raw/assets/a/tags/list", "repository:raw/assets/a:pull"},
		{http.MethodPut, "/v2/raw/assets/a/manifests/v1", "repository:raw/assets/a:pull,push"},
		{http.MethodPost, "/v2/raw/assets/a/blobs/uploads/", "repository:raw/assets/a:pull,push"},
		{http.MethodPatch, "/v2/raw/assets/a/blobs/uploads/0f", "repository:raw/assets/a:pull,push"},
		{http.MethodDelete, "/v2/raw/assets/a/blobs/uploads/0f", "repository:raw/assets/a:pull,push"},
		{http.MethodDelete, "/v2/raw/assets/a/manifests/v1", "repository:raw/assets/a:delete"},
		{http.MethodPost, "/v2/raw/assets/a/blobs/uploads/?mount=" + string(zeroDigest) + "&from=oci/images/b",
			"repository:raw/assets/a:pull,push repository:oci/images/b:pull"},
		{http.MethodPost, "/v2/raw/assets/a/blobs/uploads/?mount=" + string(zeroDigest) + "&from=raw/assets/a", "repository:raw/assets/a:pull,push"},
		{http.MethodPost, "/v2/raw/assets/a/blobs/uploads/?mount=" + string(zeroDigest) + "&from=Bad", "repository:raw/assets/a:pull,push"},
		{http.MethodPost, "/v2/raw/assets/a/blobs/uploads/?from=oci/images/b", "repository:raw/assets/a:pull,push"},
		{http.MethodGet, "/v2/Raw/assets/a/tags/list", ""},
		{http.MethodGet, "/v2/raw/assets/a", ""},
	} {
		if got := requestScope(httptest.NewRequest(tt.method, tt.target, nil)); got != tt.want {
			t.Errorf("%s %s: %q, want %q", tt.method, tt.target, got, tt.want)
		}
	}
}

// TestNewPanics checks that the token keys are required.
func TestNewPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New without Tokens did not panic")
		}
	}()
	New(Options{Index: newEnv(t, envOptions{}).st, Providers: provider.Map{}})
}

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/ci"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/facade"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/server"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

const ciAudience = "https://art.example.com"

// ciServer is the server as serve builds it with auth.ci (edgeInputs), the real facade and RBAC
// over SQLite, and an API that answers what it sees of the request; two issuers in process sign
// GitHub-shaped and GitLab-shaped tokens.
type ciServer struct {
	h      http.Handler
	st     *sqlite.Store
	gh, gl *testutil.OIDCIssuer
	now    time.Time
	cfg    *config.Config
	dsn    string
	tokens *keys.EphemeralTokens
}

// The claims of the tokens of ciServer: a release job of acme/app (repository 123), and a
// pipeline of the protected main of acme/platform/app (namespace 42).
var (
	githubRelease = map[string]any{"repository": "acme/app", "repository_id": "123", "repository_owner_id": "9", "ref": "refs/tags/v1.0.0",
		"environment": "release", "event_name": "push", "run_id": "777", "sha": "abc"}
	gitlabMain = map[string]any{"project_path": "acme/platform/app", "project_id": "7", "namespace_id": "42", "ref": "main",
		"ref_protected": "true", "pipeline_source": "push", "pipeline_id": "1001", "user_email": "bob@example.com"}
)

func newCIServer(t *testing.T) *ciServer {
	t.Helper()
	cs := &ciServer{gh: testutil.NewOIDCIssuer(t), gl: testutil.NewOIDCIssuer(t), now: time.Now()}
	c := config.Default()
	c.Server.ExternalURL, c.Server.InsecureHTTP = ciAudience, true
	c.Auth.CI.Issuers = []config.CIIssuer{
		{Name: "github", Platform: config.CIPlatformGitHub, Issuer: cs.gh.URL},
		{Name: "gitlab", Platform: config.CIPlatformGitLab, Issuer: cs.gl.URL},
	}
	c.Auth.CI.Policies = []config.CIPolicy{
		{Name: "app-release", Issuer: "github", Claims: map[string]config.CIValues{"repository_id": {"123"}, "ref": {"refs/tags/v*"},
			"environment": {"release"}}, Grants: []config.CIGrant{{Role: "writer", Mask: "oci/acme"}}},
		{Name: "app-main", Issuer: "github", Claims: map[string]config.CIValues{"repository_id": {"123"}, "ref": {"refs/heads/main"}},
			Grants: []config.CIGrant{{Role: "writer", Mask: "oci/acme"}}},
		{Name: "platform", Issuer: "gitlab", Claims: map[string]config.CIValues{"namespace_id": {"42"}, "ref": {"main"},
			"ref_protected": {"true"}}, Grants: []config.CIGrant{{Role: "writer", Mask: "oci/acme"}, {Role: "reader", Mask: "oci/other"}}},
	}
	cs.cfg, cs.dsn, cs.tokens = c, "file:"+filepath.Join(t.TempDir(), "ci.db"), keys.NewEphemeralTokens()
	cs.st = cs.open(t)
	must(t, cs.st.Migrate(t.Context()))
	for _, name := range []string{"acme", "other"} {
		must(t, cs.st.CreateRepo(t.Context(), &index.Repo{Format: "oci", Name: name, Registry: "local", Prefix: "oci/" + name, Kind: index.KindHosted}))
	}
	cs.h = cs.replica(t, cs.st)
	return cs
}

// open opens the database of cs again: the connection of another replica.
func (cs *ciServer) open(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(t.Context(), cs.dsn)
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// replica returns the handler of a server as serve builds it over st, with the token keys of cs,
// as the replicas of one master key share them.
func (cs *ciServer) replica(t *testing.T, st *sqlite.Store) http.Handler {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	edge, err := edgeInputs(cs.cfg, st, cs.tokens, log)
	must(t, err)
	authz := auth.NewRBAC(auth.RBACOptions{Index: st, Log: log})
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, cred := auth.PrincipalFrom(r.Context()), auth.CredentialFrom(r.Context())
		write, _ := authz.Allowed(r.Context(), p, auth.ActionWrite, "oci", "acme")
		_ = json.NewEncoder(w).Encode(map[string]any{"principal": p.String(), "kind": cred.Kind, "write": write})
	})
	srv, err := server.New(server.Options{Listen: "127.0.0.1:0", ExternalURL: ciAudience, Log: log, API: api,
		Facade:        facade.New(facade.Options{Index: st, Auth: authz, Tokens: cs.tokens, ExternalURL: ciAudience, Log: log}),
		Authenticator: auth.NewAuthenticator(auth.Options{Index: st, Log: log}), CI: edge.ci, CIExchange: edge.exchange})
	must(t, err)
	return srv.Handler()
}

// token returns a token of the issuer i with the claims of base, the audience and the times of
// now, then over.
func (cs *ciServer) token(t *testing.T, i *testutil.OIDCIssuer, base, over map[string]any) string {
	t.Helper()
	m := map[string]any{"aud": ciAudience, "iat": cs.now.Unix(), "nbf": cs.now.Unix(), "exp": cs.now.Add(5 * time.Minute).Unix()}
	for _, src := range []map[string]any{base, over} {
		for k, v := range src {
			m[k] = v
		}
	}
	return i.Sign(t, m)
}

// exchange posts subject to /-/oidc/token and returns the status and the JSON answer.
func (cs *ciServer) exchange(subject string) (int, map[string]any) {
	return exchangeOn(cs.h, subject)
}

// exchangeOn posts subject to /-/oidc/token of h.
func exchangeOn(h http.Handler, subject string) (int, map[string]any) {
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "subject_token": {subject},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:jwt"}}
	r := httptest.NewRequest(http.MethodPost, "/-/oidc/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return rr.Code, body
}

// ciToken exchanges subject and returns the CI token, which must be principal's.
func (cs *ciServer) ciToken(t *testing.T, subject, principal string) string {
	t.Helper()
	status, body := cs.exchange(subject)
	secret, _ := body["access_token"].(string)
	if status != http.StatusOK || !strings.HasPrefix(secret, auth.CIPrefix) || body["principal"] != principal {
		t.Fatalf("the exchange for %s: %d %v", principal, status, body)
	}
	return secret
}

// get sends a GET of target with Basic credentials.
func (cs *ciServer) get(target, user, secret string) *httptest.ResponseRecorder {
	return getOn(cs.h, target, user, secret)
}

// getOn sends a GET of target with Basic credentials to h.
func getOn(h http.Handler, target, user, secret string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.SetBasicAuth(user, secret)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// TestCITokenExchangeEndToEnd runs token exchange for CI through the server as serve builds it
// (docs/spec/rbac.md §11): a release job and a pipeline of a protected branch get CI tokens,
// which /v2/token turns into bearers with the actions of their grants and nothing beyond, and
// which the API sees with their grants; a fork's pull_request_target on main, a merge request
// pipeline and an expired token are refused, and only the exchanges that minted a token are
// audited.
func TestCITokenExchangeEndToEnd(t *testing.T) {
	cs := newCIServer(t)
	release := cs.ciToken(t, cs.token(t, cs.gh, githubRelease, nil), "ci:github/acme/app")
	rr := cs.get("/v2/token?service=art.example.com&scope=repository:oci/acme/app:pull,push&scope=repository:oci/other/x:pull", "ci", release)
	if access, ci := bearerOf(t, rr.Body.Bytes()); access != "oci/acme/app:pull,push oci/other/x:" || ci == "" {
		t.Errorf("the bearer of the release job grants %q, ci %q", access, ci)
	}
	var seen struct {
		Principal, Kind string
		Write           bool
	}
	if err := json.Unmarshal(cs.get("/api/v1/whoami", "__token__", release).Body.Bytes(), &seen); err != nil ||
		seen != (struct {
			Principal, Kind string
			Write           bool
		}{"ci:github/acme/app", "ci", true}) {
		t.Errorf("the API sees %+v, %v", seen, err)
	}

	pipeline := cs.ciToken(t, cs.token(t, cs.gl, gitlabMain, nil), "ci:gitlab/acme/platform/app")
	rr = cs.get("/v2/token?scope=repository:oci/acme/app:pull,push&scope=repository:oci/other/x:pull,push", "gitlab-ci-token", pipeline)
	if access, _ := bearerOf(t, rr.Body.Bytes()); access != "oci/acme/app:pull,push oci/other/x:pull" {
		t.Errorf("the bearer of the pipeline grants %q", access)
	}

	for name, subject := range map[string]string{
		"a fork's pull_request_target on main":   cs.token(t, cs.gh, githubRelease, map[string]any{"event_name": "pull_request_target", "ref": "refs/heads/main"}),
		"an expired token":                       cs.token(t, cs.gh, githubRelease, map[string]any{"exp": cs.now.Add(-5 * time.Minute).Unix()}),
		"a merge request pipeline in the parent": cs.token(t, cs.gl, gitlabMain, map[string]any{"pipeline_source": "merge_request_event"}),
	} {
		if status, body := cs.exchange(subject); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
			t.Errorf("%s: %d %v", name, status, body)
		}
	}
	if rr := cs.get("/v2/token?scope=repository:oci/acme/app:pull", "ci", release+"x"); rr.Code != http.StatusUnauthorized {
		t.Errorf("a changed CI token: %d", rr.Code)
	}
	checkExchangeRows(t, cs.st)
}

// checkExchangeRows checks the token.exchange rows of TestCITokenExchangeEndToEnd: the two
// tokens minted, and a row for each source of the refusals (the GitHub project, the GitLab
// project, the client address), never GitLab's user_email.
func checkExchangeRows(t *testing.T, st index.Store) {
	t.Helper()
	rows, err := st.ListAudit(t.Context(), index.AuditFilter{Action: "token.exchange"})
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]int{}
	for _, row := range rows {
		var d struct{ Outcome, Source string }
		_ = json.Unmarshal(row.Details, &d)
		outcomes[d.Outcome]++
		if strings.Contains(string(row.Details), "bob@example.com") || d.Source != "ci" {
			t.Errorf("row %s", row.Details)
		}
	}
	if outcomes["minted"] != 2 || outcomes["refused"] != 3 {
		t.Errorf("token.exchange rows by outcome: %v, want 2 minted and 3 refused", outcomes)
	}
}

// TestCIPolicyThroughTheAPI: a policy made through the API (ci.Service, on a connection of its
// own) counts at the next exchange on another replica, without a restart; deleted, its CI token is
// refused at its next request on a third one (docs/spec/rbac.md §11).
func TestCIPolicyThroughTheAPI(t *testing.T) {
	cs := newCIServer(t)
	other := cs.replica(t, cs.open(t))
	lib := cs.token(t, cs.gh, map[string]any{"repository": "acme/lib", "repository_id": "555", "ref": "refs/heads/main", "event_name": "push"}, nil)
	if status, _ := cs.exchange(lib); status != http.StatusBadRequest {
		t.Fatalf("before the policy: %d", status)
	}
	admin := auth.WithPrincipal(t.Context(), auth.Principal{Kind: auth.KindUser, Name: "019a6c3e-7b1e-7c3d-8e2f-0000000000ad"})
	s := &ci.Service{Index: cs.open(t), Config: cs.cfg.Auth.CI}
	if _, err := s.Create(admin, ci.Policy{Name: "lib-read", Issuer: "github", Claims: map[string][]string{"repository_id": {"555"}},
		Grants: []index.CIGrant{{Role: "reader", Mask: "oci/acme"}}}, auth.Origin{Source: "api"}); err != nil {
		t.Fatal(err)
	}
	status, body := exchangeOn(other, lib)
	secret, _ := body["access_token"].(string)
	if status != http.StatusOK || body["principal"] != "ci:github/acme/lib" || fmt.Sprint(body["policies"]) != "[lib-read]" {
		t.Fatalf("on another replica after the create: %d %v", status, body)
	}
	if access, _ := bearerOf(t, cs.get("/v2/token?scope=repository:oci/acme/app:pull,push", "ci", secret).Body.Bytes()); access != "oci/acme/app:pull" {
		t.Errorf("the bearer of the policy of the API grants %q", access)
	}
	if err := s.Delete(admin, "lib-read", auth.Origin{Source: "api"}); err != nil {
		t.Fatal(err)
	}
	if rr := getOn(other, "/v2/token?scope=repository:oci/acme/app:pull", "ci", secret); rr.Code != http.StatusUnauthorized {
		t.Errorf("the CI token of a deleted policy: %d", rr.Code)
	}
}

// TestCIWritePolicyThroughTheAPI: a policy that writes, made through the API with a fresh sign-in
// with a second factor, matches no token while it waits for a second admin; approved, its CI token
// publishes; expired, marked once by a pass of the expiry, its CI token is refused at its next
// request on another replica (docs/spec/rbac.md §11).
func TestCIWritePolicyThroughTheAPI(t *testing.T) {
	cs := newCIServer(t)
	cs.cfg.Auth.CI.APIWriteRepos = []string{"oci/acme"}
	other := cs.replica(t, cs.open(t))
	lib := cs.token(t, cs.gh, map[string]any{"repository": "acme/lib", "repository_id": "555", "ref": "refs/heads/main", "event_name": "push"}, nil)
	signedIn := func(uid string) context.Context {
		ctx := auth.WithPrincipal(t.Context(), auth.Principal{Kind: auth.KindUser, Name: uid})
		return auth.WithCredential(ctx, auth.Credential{Kind: auth.CredPAT, TokenID: 1,
			SignIn: &auth.SignIn{At: time.Now().Add(-time.Minute), AMR: []string{"mfa"}}})
	}
	s := &ci.Service{Index: cs.open(t), Config: cs.cfg.Auth.CI}
	made, err := s.Create(signedIn("019a6c3e-7b1e-7c3d-8e2f-0000000000a1"), ci.Policy{Name: "lib-publish", Issuer: "github",
		Claims: map[string][]string{"repository_id": {"555"}, "ref": {"refs/heads/main"}},
		Grants: []index.CIGrant{{Role: "writer", Mask: "oci/acme"}}}, auth.Origin{Source: "api"})
	if err != nil || made.State != index.CIPolicyPending {
		t.Fatalf("Create = %+v, %v", made, err)
	}
	if status, _ := exchangeOn(other, lib); status != http.StatusBadRequest {
		t.Errorf("while the policy waits: %d", status)
	}
	approver := index.WithCondition(signedIn("019a6c3e-7b1e-7c3d-8e2f-0000000000b0"), func(_, revision int64) error {
		if revision != made.Revision {
			return errors.New("stale")
		}
		return nil
	})
	if _, err := s.Approve(approver, "lib-publish", auth.Origin{Source: "api"}); err != nil {
		t.Fatal(err)
	}
	status, body := exchangeOn(other, lib)
	secret, _ := body["access_token"].(string)
	if status != http.StatusOK || fmt.Sprint(body["policies"]) != "[lib-publish]" {
		t.Fatalf("after the approval: %d %v", status, body)
	}
	if access, _ := bearerOf(t, getOn(other, "/v2/token?scope=repository:oci/acme/app:pull,push", "ci", secret).Body.Bytes()); access != "oci/acme/app:pull,push" {
		t.Errorf("the bearer of the approved policy grants %q", access)
	}
	s.Now = func() time.Time { return time.Now().Add(ci.WriteLifetime + time.Hour) }
	if n, err := s.Expire(t.Context(), auth.Origin{Source: "expiry"}); n != 1 || err != nil {
		t.Fatalf("Expire = %d, %v", n, err)
	}
	if rr := getOn(other, "/v2/token?scope=repository:oci/acme/app:pull", "ci", secret); rr.Code != http.StatusUnauthorized {
		t.Errorf("the CI token of an expired policy: %d", rr.Code)
	}
}

// bearerOf returns what the bearer in the answer of /v2/token grants, <repository>:<actions>
// separated by spaces, and its claim ci.
func bearerOf(t *testing.T, answer []byte) (string, string) {
	t.Helper()
	var a struct{ Token string }
	if err := json.Unmarshal(answer, &a); err != nil || strings.Count(a.Token, ".") != 2 {
		t.Fatalf("not an answer of /v2/token: %s", answer)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(a.Token, ".")[1])
	must(t, err)
	var c struct {
		Access []struct {
			Name    string
			Actions []string
		}
		CI string
	}
	must(t, json.Unmarshal(payload, &c))
	var out []string
	for _, a := range c.Access {
		out = append(out, fmt.Sprintf("%s:%s", a.Name, strings.Join(a.Actions, ",")))
	}
	return strings.Join(out, " "), c.CI
}

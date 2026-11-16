package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/oidc"
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

const audience = "https://art.example.com"

var memoryDBs atomic.Int64

// exchangeFixture is an Exchange over two issuers in process, one that signs GitHub-shaped
// tokens and one GitLab-shaped, with policies that write only, a token key in memory, and an
// index for the audit rows.
type exchangeFixture struct {
	ex           *Exchange
	gh, gl       *testutil.OIDCIssuer
	tokens       *token.Verifier
	st           *sqlite.Store
	now          time.Time
	logs         *bytes.Buffer
	ghTok, glTok func(over map[string]any) string
}

func newExchangeFixture(t *testing.T) *exchangeFixture {
	t.Helper()
	f := &exchangeFixture{gh: testutil.NewOIDCIssuer(t), gl: testutil.NewOIDCIssuer(t), now: time.Now().Truncate(time.Second),
		logs: &bytes.Buffer{}}
	c := config.CI{
		Issuers: []config.CIIssuer{
			{Name: "github", Platform: config.CIPlatformGitHub, Issuer: f.gh.URL},
			{Name: "gitlab", Platform: config.CIPlatformGitLab, Issuer: f.gl.URL},
		},
		Policies: []config.CIPolicy{
			{Name: "app-main", Issuer: "github", Claims: map[string]config.CIValues{"repository_id": {"123"}, "ref": {"refs/heads/main"}},
				Grants: []config.CIGrant{{Role: "writer", Mask: "pypi/internal"}}},
			{Name: "app-release", Issuer: "github", Claims: map[string]config.CIValues{"repository_id": {"123"}, "ref": {"refs/tags/v*"},
				"environment": {"release"}}, Grants: []config.CIGrant{{Role: "writer", Mask: "oci/acme"}}},
			{Name: "platform", Issuer: "gitlab", Claims: map[string]config.CIValues{"namespace_id": {"42"}, "ref_protected": {"true"}},
				Grants: []config.CIGrant{{Role: "maintainer", Mask: "oci/*"}}},
		},
	}
	ps, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	verifiers := map[string]auth.TokenVerifier{}
	for _, i := range c.Issuers {
		v, err := oidc.NewVerifier(oidc.VerifierConfig{Issuer: i.Issuer, Audience: audience, AllowHTTP: true,
			Now: func() time.Time { return f.now }, Log: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		verifiers[i.Name] = v
	}
	f.st, err = sqlite.Open(context.Background(), fmt.Sprintf("file:ci-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.st.Close() })
	if err := f.st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	k := keys.NewEphemeralTokens()
	f.tokens = token.NewVerifier(token.Options{Keys: k})
	f.ex = &Exchange{Policies: ps, Verifiers: verifiers, Tokens: token.NewIssuer(token.Options{Keys: k, Verifier: f.tokens}),
		Lifetime: 15 * time.Minute, ExternalURL: audience, Index: f.st,
		Log: slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	times := func() map[string]any {
		return map[string]any{"aud": audience, "iat": f.now.Unix(), "nbf": f.now.Unix(), "exp": f.now.Add(5 * time.Minute).Unix()}
	}
	// The issuer's own iss, the times of now, then over.
	own := func(over map[string]any) map[string]any {
		m := times()
		m["iss"] = nil
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	f.ghTok = func(over map[string]any) string { return f.gh.Sign(t, github(own(over))) }
	f.glTok = func(over map[string]any) string { return f.gl.Sign(t, gitlab(own(over))) }
	return f
}

// post posts the form of an exchange of subject, with extra fields, and returns the answer and
// its JSON.
func (f *exchangeFixture) post(t *testing.T, subject string, extra url.Values) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	form := url.Values{"grant_type": {GrantTokenExchange}, "subject_token": {subject},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:jwt"}}
	for k, v := range extra {
		form[k] = v
	}
	r := httptest.NewRequest(http.MethodPost, "/-/oidc/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	f.ex.ServeHTTP(rr, r)
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return rr, body
}

// audit returns the token.exchange rows.
func (f *exchangeFixture) audit(t *testing.T) []index.AuditEvent {
	t.Helper()
	rows, err := f.st.ListAudit(context.Background(), index.AuditFilter{Action: ActionExchange})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestExchange: a GitHub Actions token of a tag in the release environment and a GitLab CI token
// of a protected ref are exchanged for CI tokens of their projects (docs/spec/rbac.md §11): the
// answer of RFC 8693 with the principal and the policies, a token that verifies, and its
// token.exchange row with the claims of the run and nothing else.
func TestExchange(t *testing.T) {
	f := newExchangeFixture(t)
	rr, body := f.post(t, f.ghTok(nil), url.Values{"requested_token_type": {typeAccessToken},
		"audience": {"anything"}})
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("exchange = %d %v", rr.Code, body)
	}
	fields(t, body, map[string]string{"issued_token_type": typeAccessToken, "token_type": "Bearer", "expires_in": "900",
		"principal": "ci:github/acme/app", "policies": "[app-release]"})
	secret, _ := body["access_token"].(string)
	g, err := f.tokens.VerifyCI(context.Background(), secret)
	if err != nil || g.Principal.String() != "ci:github/acme/app" || !slices.Equal(g.Policies, []string{"app-release"}) {
		t.Fatalf("VerifyCI = %+v, %v", g, err)
	}
	rows := f.audit(t)
	if len(rows) != 1 {
		t.Fatalf("audit = %+v", rows)
	}
	checkGitHubRow(t, rows[0], g)

	rr, body = f.post(t, f.glTok(nil), url.Values{"subject_token_type": {"urn:ietf:params:oauth:token-type:id_token"}})
	if rr.Code != http.StatusOK || body["principal"] != "ci:gitlab/acme/platform/app" || fmt.Sprint(body["policies"]) != "[platform]" {
		t.Fatalf("gitlab exchange = %d %v", rr.Code, body)
	}
	rows = f.audit(t)
	if len(rows) != 2 || strings.Contains(string(rows[0].Details)+string(rows[1].Details), "bob@example.com") {
		t.Errorf("the gitlab row has user_email, or is missing: %+v", rows)
	}
	for _, s := range []string{secret, strings.Split(f.ghTok(nil), ".")[2]} {
		if strings.Contains(f.logs.String(), s) {
			t.Errorf("the log has a token:\n%s", f.logs.String())
		}
	}
}

// fields checks the fields of the JSON body, each printed with %v.
func fields(t *testing.T, body map[string]any, want map[string]string) {
	t.Helper()
	for k, w := range want {
		if got := fmt.Sprint(body[k]); got != w {
			t.Errorf("%s = %s, want %s", k, got, w)
		}
	}
}

// checkGitHubRow checks the token.exchange row of the GitHub release token of TestExchange,
// whose CI token says g.
func checkGitHubRow(t *testing.T, row index.AuditEvent, g *token.CIGrant) {
	t.Helper()
	if row.Principal != "ci:github/acme/app" || row.Subject != "ci:github/acme/app" || row.Repo != "" {
		t.Errorf("row = %+v", row)
	}
	var d struct {
		Source, Issuer, Token, ExpiresAt string
		Policies                         []string
		Claims                           map[string]string
	}
	if err := json.Unmarshal(row.Details, &d); err != nil {
		t.Fatal(err)
	}
	if d.Source != "ci" || d.Issuer != "github" || d.Token != g.ID || !slices.Equal(d.Policies, []string{"app-release"}) ||
		d.ExpiresAt != g.ExpiresAt.UTC().Format(time.RFC3339) {
		t.Errorf("details = %+v", d)
	}
	for c, want := range map[string]string{"ref": "refs/tags/v1.2.3", "environment": "release", "run_id": "777", "repository_id": "123"} {
		if d.Claims[c] != want {
			t.Errorf("claims.%s = %q, want %q", c, d.Claims[c], want)
		}
	}
	if strings.Contains(string(row.Details), "\"sub\"") || strings.Contains(string(row.Details), "\"aud\"") {
		t.Errorf("the row has the subject token's sub or aud: %s", row.Details)
	}
}

// TestExchangeRefused: a pull request's token (pull_request_target on main, a GitLab merge
// request pipeline in the parent project), an expired token, another audience, a token of no
// configured issuer, one of an issuer whose key it is not, and one that no policy matches are
// 400 invalid_grant, and none writes an audit row; an issuer that does not answer is 503.
func TestExchangeRefused(t *testing.T) {
	f := newExchangeFixture(t)
	other := testutil.NewOIDCIssuer(t)
	for _, tt := range []struct {
		name, subject, msg string
	}{
		{"a fork's pull_request_target on main", f.ghTok(map[string]any{"event_name": "pull_request_target", "ref": "refs/heads/main"}),
			"event_name pull_request_target"},
		{"a pull request", f.ghTok(map[string]any{"event_name": "pull_request", "ref": "refs/pull/5/merge", "environment": nil}),
			"no trust policy matches the token: issuer github, repository acme/app, ref refs/pull/5/merge, environment none"},
		{"a merge request pipeline of the parent", f.glTok(map[string]any{"pipeline_source": "merge_request_event"}),
			"pipeline_source merge_request_event"},
		{"expired", f.ghTok(map[string]any{"exp": f.now.Add(-2 * time.Minute).Unix(), "iat": f.now.Add(-7 * time.Minute).Unix()}),
			"the subject token does not verify for github"},
		{"another audience", f.ghTok(map[string]any{"aud": "https://github.com/acme"}), "does not verify"},
		{"no configured issuer", other.Sign(t, github(map[string]any{"iss": nil, "aud": audience, "exp": f.now.Add(time.Minute).Unix()})),
			"not a token of a configured issuer"},
		{"another issuer's key", other.Sign(t, github(map[string]any{"iss": f.gh.URL, "aud": audience, "exp": f.now.Add(time.Minute).Unix()})),
			"does not verify"},
		{"not a JWT", "ljs_pat_x", "not a token of a configured issuer"},
		{"another repository", f.ghTok(map[string]any{"repository_id": "999", "repository": "evil/app"}), "repository evil/app"},
	} {
		rr, body := f.post(t, tt.subject, nil)
		if rr.Code != http.StatusBadRequest || body["error"] != "invalid_grant" || !strings.Contains(fmt.Sprint(body["error_description"]), tt.msg) {
			t.Errorf("%s: %d %v, want 400 invalid_grant with %q", tt.name, rr.Code, body, tt.msg)
		}
	}
	if rows := f.audit(t); len(rows) != 0 {
		t.Errorf("refusals wrote audit rows: %+v", rows)
	}
	// An issuer whose keys never loaded: those that did stay while it does not answer.
	f = newExchangeFixture(t)
	f.gl.Down.Store(true)
	rr, body := f.post(t, f.glTok(nil), nil)
	if rr.Code != http.StatusServiceUnavailable || body["error"] != "temporarily_unavailable" || rr.Header().Get("Retry-After") != "30" {
		t.Errorf("an issuer that does not answer: %d %v", rr.Code, body)
	}
}

// TestExchangeForm: the form of docs/spec/http-routing.md §7: a scope, an actor token, another
// subject or requested token type, a missing or repeated field, another media type, another
// grant and a body over 16 KiB are refused before the token is read.
func TestExchangeForm(t *testing.T) {
	f := newExchangeFixture(t)
	good := f.ghTok(nil)
	for _, tt := range []struct {
		name   string
		extra  url.Values
		status int
		code   string
	}{
		{"a scope", url.Values{"scope": {"read"}}, 400, "invalid_scope"},
		{"an empty scope", url.Values{"scope": {""}}, 200, ""},
		{"an actor token", url.Values{"actor_token": {good}}, 400, "invalid_request"},
		{"a SAML subject", url.Values{"subject_token_type": {"urn:ietf:params:oauth:token-type:saml2"}}, 400, "invalid_request"},
		{"a refresh token asked for", url.Values{"requested_token_type": {"urn:ietf:params:oauth:token-type:refresh_token"}}, 400, "invalid_request"},
		{"two subjects", url.Values{"subject_token": {good, good}}, 400, "invalid_request"},
		{"no subject type", url.Values{"subject_token_type": {}}, 400, "invalid_request"},
		{"another grant", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}, 400, "unsupported_grant_type"},
		{"too big", url.Values{"padding": {strings.Repeat("x", MaxBody)}}, 413, "invalid_request"},
	} {
		rr, body := f.post(t, good, tt.extra)
		if rr.Code != tt.status || (tt.code != "" && body["error"] != tt.code) {
			t.Errorf("%s: %d %v, want %d %s", tt.name, rr.Code, body, tt.status, tt.code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/-/oidc/token", strings.NewReader(`{"subject_token": "x"}`))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.ex.ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_request") {
		t.Errorf("JSON: %d %s", rr.Code, rr.Body)
	}
}

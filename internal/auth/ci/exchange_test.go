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
	cfg          config.CI
	gh, gl       *testutil.OIDCIssuer
	tokens       *token.Verifier
	st           *sqlite.Store
	now          time.Time
	exNow        time.Time // the clock of the Exchange: its audit rows and their rate
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
	f.cfg = c
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
		Log: slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Now: func() time.Time { return f.exNow }}
	f.exNow = f.now
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
		Source, Outcome, Issuer, Token, ExpiresAt string
		Policies                                  []string
		Claims                                    map[string]string
	}
	if err := json.Unmarshal(row.Details, &d); err != nil {
		t.Fatal(err)
	}
	if d.Source != "ci" || d.Outcome != "minted" || d.Issuer != "github" || d.Token != g.ID || !slices.Equal(d.Policies, []string{"app-release"}) ||
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
	// A minute later the next refusal of a key writes its row, with the count of those held back.
	f.exNow = f.exNow.Add(refusalEvery)
	f.post(t, "not.a.jwt", nil)
	checkRefusalRows(t, f.audit(t))
	// An issuer whose keys never loaded: those that did stay while it does not answer.
	f = newExchangeFixture(t)
	f.gl.Down.Store(true)
	rr, body := f.post(t, f.glTok(nil), nil)
	if rr.Code != http.StatusServiceUnavailable || body["error"] != "temporarily_unavailable" || rr.Header().Get("Retry-After") != "30" {
		t.Errorf("an issuer that does not answer: %d %v", rr.Code, body)
	}
}

// checkRefusalRows checks the token.exchange rows of the refusals of TestExchangeRefused: one per
// key (the project id of a token that verified, the client address otherwise), the refusals of a
// key held back counted in its next row; the claims of a token that verified, and none of one that
// did not.
func checkRefusalRows(t *testing.T, rows []index.AuditEvent) {
	t.Helper()
	type row struct {
		Principal, Issuer, Reason string
		Suppressed                int
		Claims                    map[string]string
	}
	var got []row
	for i := len(rows) - 1; i >= 0; i-- {
		var d struct {
			Outcome, Issuer, Reason string
			Suppressed              int
			Claims                  map[string]string
		}
		if err := json.Unmarshal(rows[i].Details, &d); err != nil || d.Outcome != "refused" || rows[i].IP == "" {
			t.Fatalf("row %+v: %v", rows[i], err)
		}
		got = append(got, row{rows[i].Principal, d.Issuer, d.Reason, d.Suppressed, d.Claims})
	}
	want := []struct {
		principal, issuer, reason string
		suppressed                int
		claims                    string
	}{
		{"ci:github/acme/app", "github", "event_name pull_request_target", 0,
			"map[environment:release event_name:pull_request_target ref:refs/heads/main ref_type:tag repository:acme/app repository_id:123 repository_owner_id:9]"},
		{"ci:gitlab/acme/platform/app", "gitlab", "pipeline_source merge_request_event", 0,
			"map[namespace_id:42 pipeline_source:merge_request_event project_id:7 project_path:acme/platform/app ref:main ref_type:branch]"},
		{"anonymous", "github", "the subject token does not verify for github: ", 0, "map[]"},
		{"ci:github/evil/app", "github", "repository evil/app", 0, ""},
		{"anonymous", "", "the subject token is not a token of a configured issuer", 4, "map[]"},
	}
	if len(got) != len(want) {
		t.Fatalf("%d refusal rows, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Principal != w.principal || g.Issuer != w.issuer || !strings.Contains(g.Reason, w.reason) || g.Suppressed != w.suppressed ||
			(w.claims != "" && fmt.Sprint(g.Claims) != w.claims) {
			t.Errorf("refusal row %d = %+v, want %+v", i, g, w)
		}
	}
}

// TestRefusalRate: a key gets a row a minute; beyond 10 000 keys a new one gets none until a
// window closes with nothing held back.
func TestRefusalRate(t *testing.T) {
	var r refusals
	t0 := time.Now()
	for i := range maxRefusalKeys {
		if ok, _ := r.take(fmt.Sprint(i), t0); !ok {
			t.Fatalf("key %d: no row", i)
		}
	}
	if ok, _ := r.take("one more", t0.Add(time.Second)); ok {
		t.Error("a key beyond the bound got a row")
	}
	if ok, _ := r.take("0", t0.Add(2*time.Second)); ok {
		t.Error("a second row of a key within its minute")
	}
	if ok, held := r.take("one more", t0.Add(refusalEvery)); !ok || held != 0 {
		t.Errorf("a new key once the windows closed: %v, %d", ok, held)
	}
	if ok, held := r.take("0", t0.Add(refusalEvery)); !ok || held != 1 {
		t.Errorf("key 0 after its minute: %v, held %d; want a row with 1 held back", ok, held)
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

// TestExchangeLivePolicies: a policy made through the API counts at the next exchange on another
// Exchange over the same index, as on another replica, and a policy deleted no more
// (docs/spec/rbac.md §11).
func TestExchangeLivePolicies(t *testing.T) {
	f := newExchangeFixture(t)
	replica := func() *Exchange {
		live, err := NewLive(f.cfg, f.st, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		return &Exchange{Policies: live, Verifiers: f.ex.Verifiers, Tokens: f.ex.Tokens, Lifetime: f.ex.Lifetime,
			ExternalURL: f.ex.ExternalURL, Index: f.st, Log: slog.New(slog.DiscardHandler)}
	}
	a, b := replica(), replica()
	newRepo := f.ghTok(map[string]any{"repository": "acme/new", "repository_id": "555", "ref": "refs/heads/feature", "environment": nil})
	f.ex = b
	if rr, body := f.post(t, newRepo, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("before the policy: %d %v", rr.Code, body)
	}
	s := &Service{Index: f.st, Config: f.cfg}
	ctx := auth.WithPrincipal(context.Background(), admin)
	if _, err := s.Create(ctx, Policy{Name: "new-read", Issuer: "github", Claims: map[string][]string{"repository_id": {"555"}},
		Grants: []index.CIGrant{{Role: "reader", Mask: "oci/*"}}}, auth.Origin{Source: "api"}); err != nil {
		t.Fatal(err)
	}
	if rr, body := f.post(t, newRepo, nil); rr.Code != http.StatusOK || body["principal"] != "ci:github/acme/new" ||
		fmt.Sprint(body["policies"]) != "[new-read]" {
		t.Errorf("on another replica after the create: %d %v", rr.Code, body)
	}
	f.ex = a
	if rr, _ := f.post(t, newRepo, nil); rr.Code != http.StatusOK {
		t.Errorf("on the first replica: %d", rr.Code)
	}
	if err := s.Delete(ctx, "new-read", auth.Origin{Source: "api"}); err != nil {
		t.Fatal(err)
	}
	if rr, _ := f.post(t, newRepo, nil); rr.Code != http.StatusBadRequest {
		t.Errorf("after the delete: %d", rr.Code)
	}
}

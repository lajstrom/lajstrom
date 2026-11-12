package oidc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// fakeProvider records the flow's parameters and answers the identity of the test; Exchange
// refuses a verifier whose challenge is not the one it was given, and another nonce.
type fakeProvider struct {
	mu                      sync.Mutex
	state, nonce, challenge string
	id                      Identity
	err, discoveryErr       error // of Exchange, of AuthCodeURL
	code                    string
}

func (f *fakeProvider) AuthCodeURL(_ context.Context, state, nonce, challenge string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.discoveryErr != nil {
		return "", f.discoveryErr
	}
	f.state, f.nonce, f.challenge = state, nonce, challenge
	return "https://idp.example/auth?state=" + url.QueryEscape(state), nil
}

func (f *fakeProvider) Exchange(_ context.Context, code, verifier, nonce string) (*Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.code = code
	switch {
	case f.err != nil:
		return nil, f.err
	case Challenge(verifier) != f.challenge || nonce != f.nonce:
		return nil, fmt.Errorf("%w: the verifier or the nonce", ErrRefused)
	}
	id := f.id
	return &id, nil
}

var memoryDBs atomic.Int64

type loginFixture struct {
	t    *testing.T
	st   *sqlite.Store
	p    *fakeProvider
	h    http.Handler
	now  atomic.Pointer[time.Time]
	logs *bytes.Buffer
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:oidc-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := &loginFixture{t: t, st: st, logs: &bytes.Buffer{},
		p: &fakeProvider{id: Identity{Issuer: "https://idp.example", Subject: "sub-1", Username: "alice", Groups: []string{"dev"}}}}
	now := time.Now().UTC().Truncate(time.Second)
	f.now.Store(&now)
	clock := func() time.Time { return *f.now.Load() }
	log := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.h = NewHandler(LoginOptions{Provider: f.p, Index: st, Users: &auth.Users{Index: st, Now: clock, Log: log},
		Tokens: &auth.Tokens{Index: st, Now: clock, Log: log}, ExternalURL: "https://lajstrom.example",
		TokenLifetime: 24 * time.Hour, Now: clock, Log: log})
	return f
}

func (f *loginFixture) advance(d time.Duration) {
	n := f.now.Load().Add(d)
	f.now.Store(&n)
}

func (f *loginFixture) do(method, target string, body url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	f.t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, r)
	if rr.Header().Get("Cache-Control") != "no-store" {
		f.t.Errorf("%s %s: Cache-Control %q, want no-store", method, target, rr.Header().Get("Cache-Control"))
	}
	return rr
}

// start runs GET /-/oidc/login (with query) and returns the state the IdP got and the cookie.
func (f *loginFixture) start(query string) (string, *http.Cookie) {
	f.t.Helper()
	rr := f.do(http.MethodGet, "/-/oidc/login"+query, nil)
	if rr.Code != http.StatusFound || !strings.HasPrefix(rr.Header().Get("Location"), "https://idp.example/auth?") {
		f.t.Fatalf("login: %d, Location %q: %s", rr.Code, rr.Header().Get("Location"), rr.Body)
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != cookiePath {
		f.t.Fatalf("login's cookie: %+v", cookie)
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	return f.p.state, cookie
}

func (f *loginFixture) callback(state string, cookie *http.Cookie) *httptest.ResponseRecorder {
	f.t.Helper()
	var cs []*http.Cookie
	if cookie != nil {
		cs = append(cs, cookie)
	}
	return f.do(http.MethodGet, "/-/oidc/callback?code=the-code&state="+url.QueryEscape(state), nil, cs...)
}

// tokenOf returns the token of the token page.
func tokenOf(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, auth.PATPrefix)
	if i < 0 {
		t.Fatalf("no token on the page: %s", body)
	}
	return body[i : i+len(auth.PATPrefix)+43]
}

// TestCodeFlow: a login shows a personal access token once, of a user made at the first login
// with its groups; the token authenticates; the state is redeemed once; the audit rows are there;
// no secret reaches the log.
func TestCodeFlow(t *testing.T) {
	f := newLoginFixture(t)
	ctx := context.Background()
	state, cookie := f.start("")
	rr := f.callback(state, cookie)
	if rr.Code != http.StatusOK || rr.Header().Get("Referrer-Policy") != "no-referrer" ||
		!strings.Contains(rr.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("callback: %d %v: %s", rr.Code, rr.Header(), rr.Body)
	}
	secret := tokenOf(t, rr.Body.String())
	u, p := f.checkLogin(secret)

	// The same state again, with its cookie: used.
	if rr := f.callback(state, cookie); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "used already") {
		t.Errorf("a second callback: %d %s", rr.Code, rr.Body)
	}
	f.checkAudit(p)
	f.checkNoSecrets(secret, state, cookie.Value, "the-code")

	// The next login of the identity: the same user, its groups replaced.
	f.p.id.Groups = []string{"ops"}
	f.p.id.Username = "renamed-at-the-idp"
	state, cookie = f.start("")
	if rr := f.callback(state, cookie); rr.Code != http.StatusOK {
		t.Fatalf("a second login: %d %s", rr.Code, rr.Body)
	}
	groups, _ := auth.UserGroups(ctx, f.st, u, *f.now.Load())
	if len(groups) != 1 || groups[0] != "ops" {
		t.Errorf("the groups after the second login: %q", groups)
	}
	if _, err := f.st.GetUser(ctx, "renamed-at-the-idp"); err == nil {
		t.Error("a later login renamed the user")
	}
}

// checkLogin checks the first login of the fixture: its token authenticates as a PAT without
// scopes, of the user alice of kind oidc without a password, whose groups are dev, and expires
// with the lifetime. It returns the user and its principal.
func (f *loginFixture) checkLogin(secret string) (*index.User, auth.Principal) {
	f.t.Helper()
	ctx := context.Background()
	p, cred, err := auth.NewAuthenticator(auth.Options{Index: f.st}).AuthenticateBearer(ctx, secret)
	if err != nil || cred.Kind != auth.CredPAT || len(cred.Scopes) != 0 {
		f.t.Fatalf("the token of the login: %v, %v, %+v", p, err, cred)
	}
	u, err := f.st.GetUser(ctx, "alice")
	if err != nil || u.Kind != auth.KindOIDC || u.PasswordHash != "" || auth.UserPrincipal(u) != p {
		f.t.Fatalf("the user of the login: %+v, %v", u, err)
	}
	if groups, err := auth.UserGroups(ctx, f.st, u, *f.now.Load()); err != nil || len(groups) != 1 || groups[0] != "dev" {
		f.t.Errorf("the groups of the login: %q, %v", groups, err)
	}
	toks, err := f.st.ListTokens(ctx, &u.ID, nil)
	if err != nil || len(toks) != 1 || toks[0].ExpiresAt == nil || !toks[0].ExpiresAt.Equal(f.now.Load().Add(24*time.Hour)) {
		f.t.Errorf("the token row: %+v, %v", toks, err)
	}
	return u, p
}

// checkAudit checks the audit rows of a first login by p.
func (f *loginFixture) checkAudit(p auth.Principal) {
	f.t.Helper()
	for _, tt := range []struct{ action, detail string }{
		{auth.ActionUserCreate, `"source":"oidc"`},
		{auth.ActionUserLogin, `"groups":["dev"]`},
		{auth.ActionTokenCreate, `"createdBy":"oidc"`},
	} {
		evs, err := f.st.ListAudit(context.Background(), index.AuditFilter{Action: tt.action})
		if err != nil || len(evs) != 1 || evs[0].Principal != p.String() || !strings.Contains(string(evs[0].Details), tt.detail) {
			f.t.Errorf("%s: %+v, %v; want one by %s with %s", tt.action, evs, err, p, tt.detail)
		}
	}
}

// checkNoSecrets fails when the log has one of the secrets.
func (f *loginFixture) checkNoSecrets(secrets ...string) {
	f.t.Helper()
	for _, s := range secrets {
		if strings.Contains(f.logs.String(), s) {
			f.t.Errorf("the log has a secret %q…", s[:min(8, len(s))])
		}
	}
}

// TestCodeFlowRefusals: each refusal of docs/spec/rbac.md §8 is 400 with a page (503 when the
// IdP does not answer), and none makes a user or a token.
func TestCodeFlowRefusals(t *testing.T) {
	for _, tt := range []struct {
		name   string
		setup  func(f *loginFixture)
		call   func(f *loginFixture, state string, cookie *http.Cookie) *httptest.ResponseRecorder
		status int
		want   string
	}{
		{"no cookie", nil, func(f *loginFixture, s string, _ *http.Cookie) *httptest.ResponseRecorder { return f.callback(s, nil) },
			400, "another browser"},
		{"another browser's cookie", nil, func(f *loginFixture, s string, _ *http.Cookie) *httptest.ResponseRecorder {
			return f.callback(s, &http.Cookie{Name: cookieName, Value: NewSecret(), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		}, 400, "another browser"},
		{"an unknown state", nil, func(f *loginFixture, _ string, c *http.Cookie) *httptest.ResponseRecorder {
			return f.callback(NewSecret(), c)
		},
			400, "unknown"},
		{"after ten minutes", nil, func(f *loginFixture, s string, c *http.Cookie) *httptest.ResponseRecorder {
			f.advance(LoginTTL)
			return f.callback(s, c)
		}, 400, "10 minutes"},
		{"the IdP refused", nil, func(f *loginFixture, s string, c *http.Cookie) *httptest.ResponseRecorder {
			return f.do(http.MethodGet, "/-/oidc/callback?error=access_denied&error_description=%3Cb%3Eno&state="+url.QueryEscape(s), nil, c)
		}, 400, "did not log you in"},
		{"the ID token did not verify", func(f *loginFixture) { f.p.err = fmt.Errorf("%w: aud", ErrRefused) },
			nil, 400, "did not verify"},
		{"the IdP does not answer the exchange", func(f *loginFixture) { f.p.err = fmt.Errorf("%w: timeout", ErrUnavailable) },
			nil, 503, "does not answer"},
		{"a name a local user has", func(f *loginFixture) {
			if _, err := (&auth.Users{Index: f.st}).Create(context.Background(), "alice", "a-long-password", false, auth.Origin{}); err != nil {
				f.t.Fatal(err)
			}
		}, nil, 400, "taken by another user"},
		{"no user name claim", func(f *loginFixture) { f.p.id.Username = "" }, nil, 400, "not a user name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newLoginFixture(t)
			if tt.setup != nil {
				tt.setup(f)
			}
			state, cookie := f.start("")
			call := tt.call
			if call == nil {
				call = func(f *loginFixture, s string, c *http.Cookie) *httptest.ResponseRecorder { return f.callback(s, c) }
			}
			rr := call(f, state, cookie)
			if rr.Code != tt.status || !strings.Contains(rr.Body.String(), tt.want) || strings.Contains(rr.Body.String(), "<b>") {
				t.Errorf("%d %s; want %d with %q", rr.Code, rr.Body, tt.status, tt.want)
			}
			if toks, _ := f.st.ListTokens(context.Background(), nil, nil); len(toks) != 0 {
				t.Errorf("a refused login made tokens: %+v", toks)
			}
		})
	}
	f := newLoginFixture(t)
	f.p.discoveryErr = fmt.Errorf("%w: discovery", ErrUnavailable)
	if rr := f.do(http.MethodGet, "/-/oidc/login", nil); rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") != "30" {
		t.Errorf("login with discovery down: %d %v", rr.Code, rr.Header())
	}
}

// poll posts the device code to the token endpoint and returns the status and the answer.
func (f *loginFixture) poll(deviceCode string) (int, map[string]any) {
	f.t.Helper()
	rr := f.do(http.MethodPost, "/-/oidc/token", url.Values{"grant_type": {deviceGrant}, "device_code": {deviceCode}})
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		f.t.Fatalf("token: %d %s", rr.Code, rr.Body)
	}
	return rr.Code, m
}

func (f *loginFixture) device() deviceResponse {
	f.t.Helper()
	rr := f.do(http.MethodPost, "/-/oidc/device/code", url.Values{"client_id": {"lajstrom-cli"}})
	var d deviceResponse
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &d) != nil {
		f.t.Fatalf("device/code: %d %s", rr.Code, rr.Body)
	}
	return d
}

// TestDeviceFlow: RFC 8628 as docs/spec/rbac.md §8 has it: pending, slow_down, the confirmation
// page, the approval by the code flow, the token once.
func TestDeviceFlow(t *testing.T) {
	f := newLoginFixture(t)
	d := f.device()
	f.checkWaiting(d)
	if rr := f.do(http.MethodGet, "/-/oidc/login?user_code=BCDF-GHJK", nil); rr.Code != 400 || !strings.Contains(rr.Body.String(), "matches no login") {
		t.Errorf("a code of no request: %d %s", rr.Code, rr.Body)
	}
	state, cookie := f.start("?user_code=" + url.QueryEscape(d.UserCode))
	if rr := f.callback(state, cookie); rr.Code != 200 || !strings.Contains(rr.Body.String(), "Return to your terminal") ||
		strings.Contains(rr.Body.String(), auth.PATPrefix) {
		t.Fatalf("the device's callback: %d %s", rr.Code, rr.Body)
	}
	f.advance(PollInterval)
	code, m := f.poll(d.DeviceCode)
	tok, _ := m["access_token"].(string)
	if code != 200 || m["user"] != "alice" || m["token_type"] != "Bearer" || !strings.HasPrefix(tok, auth.PATPrefix) {
		t.Fatalf("the poll after the approval: %d %v", code, m)
	}
	if _, _, err := auth.NewAuthenticator(auth.Options{Index: f.st}).AuthenticateBearer(context.Background(), tok); err != nil {
		t.Errorf("the device's token: %v", err)
	}
	if code, m := f.poll(d.DeviceCode); code != 400 || m["error"] != "invalid_grant" {
		t.Errorf("a second redemption: %d %v", code, m)
	}
	f.checkNoSecrets(tok, d.DeviceCode)
}

// checkWaiting checks a new device request d: its answer, the polls before the approval, and
// the page that confirms its code.
func (f *loginFixture) checkWaiting(d deviceResponse) {
	f.t.Helper()
	if len(d.UserCode) != 9 || d.UserCode[4] != '-' || d.VerificationURI != "https://lajstrom.example/-/oidc/device" ||
		d.VerificationURIComplete != d.VerificationURI+"?user_code="+d.UserCode || d.Interval != 5 || d.ExpiresIn != 600 || len(d.DeviceCode) != 43 {
		f.t.Fatalf("device/code = %+v", d)
	}
	if code, m := f.poll(d.DeviceCode); code != 400 || m["error"] != "authorization_pending" {
		f.t.Errorf("the first poll: %d %v", code, m)
	}
	if code, m := f.poll(d.DeviceCode); code != 400 || m["error"] != "slow_down" {
		f.t.Errorf("a poll too soon: %d %v", code, m)
	}
	page := f.do(http.MethodGet, "/-/oidc/device?user_code="+url.QueryEscape(strings.ToLower(d.UserCode)), nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), `value="`+strings.ReplaceAll(d.UserCode, "-", "")+`"`) ||
		page.Header().Get("X-Frame-Options") != "DENY" {
		f.t.Errorf("the device page: %d %s", page.Code, page.Body)
	}
}

// TestDeviceFlowEnds: a refused login denies the device, ten minutes expire it, an unknown
// code and another grant type are refused.
func TestDeviceFlowEnds(t *testing.T) {
	f := newLoginFixture(t)
	d := f.device()
	f.p.err = fmt.Errorf("%w: nonce", ErrRefused)
	state, cookie := f.start("?user_code=" + d.UserCode)
	if rr := f.callback(state, cookie); rr.Code != 400 {
		t.Fatalf("a refused device login: %d", rr.Code)
	}
	if code, m := f.poll(d.DeviceCode); code != 400 || m["error"] != "access_denied" {
		t.Errorf("the poll after a refusal: %d %v", code, m)
	}
	late := f.device()
	f.advance(LoginTTL)
	if code, m := f.poll(late.DeviceCode); code != 400 || m["error"] != "expired_token" {
		t.Errorf("a poll after ten minutes: %d %v", code, m)
	}
	if rr := f.do(http.MethodGet, "/-/oidc/login?user_code="+late.UserCode, nil); rr.Code != 400 {
		t.Errorf("the code of an expired request: %d", rr.Code)
	}
	if code, m := f.poll(NewSecret()); code != 400 || m["error"] != "invalid_grant" {
		t.Errorf("an unknown device code: %d %v", code, m)
	}
	rr := f.do(http.MethodPost, "/-/oidc/token", url.Values{"grant_type": {"authorization_code"}, "code": {"x"}})
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "unsupported_grant_type") {
		t.Errorf("another grant type: %d %s", rr.Code, rr.Body)
	}
	if rr := f.do(http.MethodGet, "/-/oidc/nothing", nil); rr.Code != 404 {
		t.Errorf("an unknown route: %d", rr.Code)
	}
}

func TestLimiter(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	l := newLimiter(2, time.Minute, func() time.Time { return now })
	for range 2 {
		if l.wait("a") != 0 {
			t.Fatal("limited before the bound")
		}
		l.add("a")
	}
	if w := l.wait("a"); w != time.Minute {
		t.Errorf("wait at the bound = %v, want a minute", w)
	}
	if l.wait("b") != 0 || l.wait("") != 0 {
		t.Error("another key, or no address, is limited")
	}
	now = now.Add(time.Minute)
	if l.wait("a") != 0 {
		t.Error("limited in the next window")
	}
	if normalizeUserCode("bcdf-ghjk") != "BCDFGHJK" || normalizeUserCode("BCDF-GHJA") != "" || normalizeUserCode("<x>") != "" {
		t.Error("normalizeUserCode")
	}
	if c := newUserCode(); len(c) != userCodeLen || strings.Trim(c, userCodeAlphabet) != "" {
		t.Errorf("newUserCode = %q", c)
	}
}

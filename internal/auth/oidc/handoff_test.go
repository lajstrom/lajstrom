package oidc

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

const uiOrigin = "https://ui.example.com"

// testJKT is the thumbprint of the web UI's DPoP key in the tests: the key of RFC 9449 Figure 9.
const testJKT = "0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I"

// uiQuery is the query of a web UI's login that returns to page with the challenge of verifier
// and the key testJKT.
func uiQuery(page, verifier string) string {
	return "?" + url.Values{"return_to": {page}, "code_challenge": {Challenge(verifier)}, "code_challenge_method": {"S256"},
		"dpop_jkt": {testJKT}}.Encode()
}

// handoffs returns the redeemer of the fixture's codes, whose token lives lifetime.
func (f *loginFixture) handoffs(lifetime time.Duration) *Handoffs {
	clock := func() time.Time { return *f.now.Load() }
	log := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &Handoffs{Index: f.st, Tokens: &auth.Tokens{Index: f.st, Now: clock, Log: log}, TokenLifetime: lifetime, Now: clock, Log: log}
}

// handOff runs a web UI's login to its callback and returns the code of the redirect.
func (f *loginFixture) handOff(page, verifier string) string {
	f.t.Helper()
	state, cookie := f.start(uiQuery(page, verifier))
	rr := f.callback(state, cookie)
	loc := rr.Header().Get("Location")
	code, ok := strings.CutPrefix(loc, page+"#code=")
	if rr.Code != http.StatusFound || !ok || len(code) != 43 || rr.Body.Len() != 0 {
		f.t.Fatalf("callback: %d, Location %q, body %q; want 302 to %s#code=<43 characters>, no body", rr.Code, loc, rr.Body, page)
	}
	return code
}

// TestUIHandoff checks a web UI's OIDC sign-in (docs/spec/rbac.md §10): the callback of a login
// with return_to mints no token and answers 302 to the page with a code in the fragment; the
// code, with the verifier, redeems once into a personal access token of the login's user, which
// expires after the handoff's lifetime, with the audit rows of a browser login; nothing secret
// reaches the log.
func TestUIHandoff(t *testing.T) {
	f := newLoginFixture(t)
	ctx := context.Background()
	verifier := NewSecret()
	page := uiOrigin + "/ui/sign-in?redirect=%2Fapt%2Fubuntu"
	code := f.handOff(page, verifier)
	if evs, _ := f.st.ListAudit(ctx, index.AuditFilter{Action: auth.ActionTokenCreate}); len(evs) != 0 {
		t.Errorf("the callback minted a token: %+v", evs)
	}
	row, err := f.st.GetOIDCLoginByHandoff(ctx, Hash(code))
	if err != nil || row.Status != index.OIDCApproved || row.UserID == nil || row.ReturnTo != page || row.CodeChallenge != Challenge(verifier) ||
		row.DPoPJKT != testJKT {
		t.Fatalf("the login row: %+v, %v", row, err)
	}

	secret := f.redeemed(code, verifier, 12*time.Hour)
	if _, _, err := f.handoffs(12*time.Hour).Redeem(ctx, code, verifier, testJKT, auth.Origin{}); !errors.Is(err, auth.ErrInvalid) ||
		err.Error() != errHandoff {
		t.Errorf("a second redemption: %v, want %q", err, errHandoff)
	}
	f.checkNoSecrets(code, verifier, Hash(code), secret)
}

// redeemed redeems code with verifier and the key testJKT and checks the token: alice's, oidc
// <time> <4 hex>, without scopes, bound to the key, expiring after lifetime, with the audit rows
// of a first login. It returns the secret.
func (f *loginFixture) redeemed(code, verifier string, lifetime time.Duration) string {
	f.t.Helper()
	ctx := context.Background()
	tok, secret, err := f.handoffs(lifetime).Redeem(ctx, code, verifier, testJKT, auth.Origin{Source: "api", IP: "192.0.2.1"})
	if err != nil {
		f.t.Fatalf("Redeem: %v", err)
	}
	if tok.User != "alice" || !strings.HasPrefix(tok.Name, "oidc ") || len(tok.Scopes) != 0 || tok.ExpiresAt == nil ||
		!tok.ExpiresAt.Equal(f.now.Load().Add(lifetime)) || tok.DPoPJKT != testJKT {
		f.t.Errorf("the token: %+v", tok)
	}
	authn := auth.NewAuthenticator(auth.Options{Index: f.st})
	p, cred, err := authn.AuthenticateDPoP(ctx, secret, testJKT)
	if err != nil || cred.Kind != auth.CredPAT {
		f.t.Fatalf("the token authenticates with its key: %v %+v, %v", p, cred, err)
	}
	if _, _, err := authn.AuthenticateBearer(ctx, secret); !errors.Is(err, auth.ErrBound) {
		f.t.Errorf("the token as a bearer: %v, want ErrBound", err)
	}
	f.checkAudit(p)
	return secret
}

// TestUIHandoffRefused checks the codes that do not redeem: a wrong verifier, which uses the
// code up; a verifier that is not one, which does not; an expired code; an unknown one; and the
// code of a user disabled since its login, ErrBadCredentials.
func TestUIHandoffRefused(t *testing.T) {
	ctx := context.Background()
	t.Run("a wrong verifier", func(t *testing.T) {
		f := newLoginFixture(t)
		verifier := NewSecret()
		code := f.handOff(uiOrigin+"/", verifier)
		h := f.handoffs(time.Hour)
		if _, _, err := h.Redeem(ctx, code, NewSecret(), testJKT, auth.Origin{}); !errors.Is(err, auth.ErrInvalid) {
			t.Fatalf("a wrong verifier: %v", err)
		}
		if _, _, err := h.Redeem(ctx, code, verifier, testJKT, auth.Origin{}); !errors.Is(err, auth.ErrInvalid) {
			t.Errorf("the right verifier after a wrong one: %v, want the code used up", err)
		}
	})
	t.Run("a verifier that is not one", func(t *testing.T) {
		f := newLoginFixture(t)
		verifier := NewSecret()
		code := f.handOff(uiOrigin+"/", verifier)
		h := f.handoffs(time.Hour)
		for _, v := range []string{"", "short", strings.Repeat("a", 129), verifier[:42] + "!"} {
			if _, _, err := h.Redeem(ctx, code, v, testJKT, auth.Origin{}); !errors.Is(err, auth.ErrInvalid) || !strings.HasPrefix(err.Error(), "codeVerifier: ") {
				t.Errorf("verifier %q: %v, want codeVerifier: …", v, err)
			}
		}
		if _, _, err := h.Redeem(ctx, code, verifier, testJKT, auth.Origin{}); err != nil {
			t.Errorf("the right verifier after malformed ones: %v", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		f := newLoginFixture(t)
		verifier := NewSecret()
		code := f.handOff(uiOrigin+"/", verifier)
		f.advance(LoginTTL)
		if _, _, err := f.handoffs(time.Hour).Redeem(ctx, code, verifier, testJKT, auth.Origin{}); !errors.Is(err, auth.ErrInvalid) {
			t.Errorf("an expired code: %v", err)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		f := newLoginFixture(t)
		for _, code := range []string{"", NewSecret()} {
			if _, _, err := f.handoffs(time.Hour).Redeem(ctx, code, NewSecret(), testJKT, auth.Origin{}); !errors.Is(err, auth.ErrInvalid) {
				t.Errorf("code %q: %v", code, err)
			}
		}
	})
	t.Run("a user disabled since", func(t *testing.T) {
		f := newLoginFixture(t)
		verifier := NewSecret()
		code := f.handOff(uiOrigin+"/", verifier)
		u, err := f.st.GetUser(ctx, "alice")
		if err != nil {
			t.Fatal(err)
		}
		u.Disabled = true
		if err := f.st.UpdateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.handoffs(time.Hour).Redeem(ctx, code, verifier, testJKT, auth.Origin{}); !errors.Is(err, auth.ErrBadCredentials) {
			t.Errorf("a disabled user: %v, want ErrBadCredentials", err)
		}
	})
}

// TestUIHandoffWrongKey checks a code redeemed with a proof of another key than the login's
// dpop_jkt (RFC 9449 §10): refused as any code that does not redeem, the login request denied,
// so that the right key does not redeem it after, and the reason logged.
func TestUIHandoffWrongKey(t *testing.T) {
	ctx := context.Background()
	f := newLoginFixture(t)
	verifier := NewSecret()
	code := f.handOff(uiOrigin+"/", verifier)
	h := f.handoffs(time.Hour)
	if _, _, err := h.Redeem(ctx, code, verifier, Challenge(NewSecret()), auth.Origin{}); !errors.Is(err, auth.ErrInvalid) ||
		err.Error() != errHandoff {
		t.Fatalf("a proof of another key: %v, want %q", err, errHandoff)
	}
	if row, err := f.st.GetOIDCLoginByHandoff(ctx, Hash(code)); err != nil || row.Status != index.OIDCDenied {
		t.Errorf("the login row %+v, %v; want denied", row, err)
	}
	if _, _, err := h.Redeem(ctx, code, verifier, testJKT, auth.Origin{}); !errors.Is(err, auth.ErrInvalid) {
		t.Errorf("the right key after a wrong one: %v, want the code used up", err)
	}
	if !strings.Contains(f.logs.String(), "a code of another DPoP key") {
		t.Errorf("the log does not say why: %s", f.logs)
	}
}

// TestUIStepUp checks a web UI's login with prompt=login (docs/spec/rbac.md §10): the IdP is asked
// to authenticate again, the callback keeps the ID token's auth_time, amr and acr on the login,
// and the token of its code keeps them, so that a request with it and its DPoP proof carries the
// sign-in, judged by auth.oidc.mfa; the token's view answers it. A login whose ID token has no
// auth_time keeps its amr and acr, a sign-in never fresh, and one with none of the three gives a
// token without a sign-in.
func TestUIStepUp(t *testing.T) {
	f := newLoginFixture(t)
	ctx := context.Background()
	at := f.now.Load().Add(-time.Minute)
	f.p.id.AuthTime, f.p.id.AMR, f.p.id.ACR = &at, []string{"pwd", "otp"}, "gold"
	verifier := NewSecret()
	state, cookie := f.start(uiQuery(uiOrigin+"/ui/", verifier) + "&prompt=login")
	if !f.p.stepUp {
		t.Error("a login with prompt=login did not ask the IdP for a step-up")
	}
	rr := f.callback(state, cookie)
	code, _ := strings.CutPrefix(rr.Header().Get("Location"), uiOrigin+"/ui/#code=")
	row, err := f.st.GetOIDCLoginByHandoff(ctx, Hash(code))
	if err != nil || row.AuthTime == nil || !row.AuthTime.Equal(at) || !slices.Equal(row.AMR, []string{"pwd", "otp"}) || row.ACR != "gold" {
		t.Fatalf("the login row: %+v, %v", row, err)
	}
	tok, secret, err := f.handoffs(time.Hour).Redeem(ctx, code, verifier, testJKT, auth.Origin{})
	if err != nil {
		t.Fatal(err)
	}
	checkStepUpToken(t, f, tok, at)
	gold := auth.MFA{ACR: []string{"gold"}}
	_, cred, err := auth.NewAuthenticator(auth.Options{Index: f.st, MFA: gold}).AuthenticateDPoP(ctx, secret, testJKT)
	if err != nil || cred.SignIn == nil || !cred.SignIn.At.Equal(at) || !slices.Equal(cred.SignIn.AMR, []string{"pwd", "otp"}) ||
		cred.SignIn.ACR != "gold" || !slices.Equal(cred.SignIn.MFA.ACR, gold.ACR) {
		t.Fatalf("the token's credential: %+v, %v", cred, err)
	}
	if err := auth.FreshSignIn(cred, *f.now.Load()); err != nil {
		t.Errorf("a sign-in a minute old with the acr of auth.oidc.mfa: %v", err)
	}
	checkNoSignIn(t, f, verifier)
}

// checkStepUpToken checks the token tok of TestUIStepUp, of a sign-in at at with pwd, otp and
// gold: its view, and its token.create row.
func checkStepUpToken(t *testing.T, f *loginFixture, tok *auth.Token, at time.Time) {
	t.Helper()
	if in := tok.SignIn; in == nil || in.AuthTime == nil || !in.AuthTime.Equal(at) || in.ACR != "gold" {
		t.Errorf("the token's view: %+v", tok)
	}
	evs, _ := f.st.ListAudit(context.Background(), index.AuditFilter{Action: auth.ActionTokenCreate})
	if len(evs) != 1 || !strings.Contains(string(evs[0].Details), `"amr":["pwd","otp"]`) ||
		!strings.Contains(string(evs[0].Details), `"authTime":"`+at.Format(time.RFC3339)+`"`) ||
		!strings.Contains(string(evs[0].Details), `"acr":"gold"`) {
		t.Errorf("the token.create row of %d: %+v", tok.ID, evs)
	}
}

// checkNoSignIn checks that a login of the fixture f without prompt asks no step-up; that, its
// ID token without auth_time, its token keeps amr and acr, a sign-in with a second factor that is
// never fresh; and that, its ID token with none of the three, its token carries no sign-in.
func checkNoSignIn(t *testing.T, f *loginFixture, verifier string) {
	t.Helper()
	ctx := context.Background()
	f.p.id.AuthTime, f.p.id.AMR, f.p.id.ACR = nil, []string{"mfa"}, "gold"
	tok, secret, err := f.handoffs(time.Hour).Redeem(ctx, f.handOff(uiOrigin+"/ui/", verifier), verifier, testJKT, auth.Origin{})
	if err != nil {
		t.Fatal(err)
	}
	if f.p.stepUp {
		t.Error("a login without prompt asked the IdP for a step-up")
	}
	if in := tok.SignIn; in == nil || in.AuthTime != nil || in.ACR != "gold" {
		t.Errorf("the view of a token without auth_time: %+v", tok.SignIn)
	}
	_, cred, err := auth.NewAuthenticator(auth.Options{Index: f.st}).AuthenticateDPoP(ctx, secret, testJKT)
	var stepUp *auth.StepUpError
	switch {
	case err != nil || cred.SignIn == nil || !cred.SignIn.At.IsZero() || cred.SignIn.ACR != "gold" || !cred.SignIn.SecondFactor():
		t.Errorf("a login without auth_time: %+v, %v; want its amr and acr", cred, err)
	case !errors.As(auth.FreshSignIn(cred, *f.now.Load()), &stepUp):
		t.Errorf("a sign-in without auth_time is fresh, or not asked for more")
	}
	f.p.id.AMR, f.p.id.ACR = nil, ""
	_, secret, err = f.handoffs(time.Hour).Redeem(ctx, f.handOff(uiOrigin+"/ui/", verifier), verifier, testJKT, auth.Origin{})
	if err != nil {
		t.Fatal(err)
	}
	_, cred, err = auth.NewAuthenticator(auth.Options{Index: f.st}).AuthenticateDPoP(ctx, secret, testJKT)
	if err != nil || cred.SignIn != nil {
		t.Errorf("a login without auth_time, amr and acr: %+v, %v; want no sign-in", cred, err)
	}
}

// TestUILoginRefused checks the logins with return_to that do not start: 400 with a page, no
// cookie, nothing for the IdP.
func TestUILoginRefused(t *testing.T) {
	verifier := NewSecret()
	ch := Challenge(verifier)
	q := func(kv ...string) string { // dpop_jkt testJKT unless kv names it, "" removing it
		v := url.Values{"dpop_jkt": {testJKT}}
		for pair := range slices.Chunk(kv, 2) {
			v.Set(pair[0], pair[len(pair)-1])
		}
		if v.Get("dpop_jkt") == "" {
			v.Del("dpop_jkt")
		}
		return "?" + v.Encode()
	}
	for _, tc := range []struct{ query, why string }{
		{q("return_to", "https://evil.test/", "code_challenge", ch, "code_challenge_method", "S256"), "not on the origin"},
		{q("return_to", "https://ui.example.com.evil.test/", "code_challenge", ch, "code_challenge_method", "S256"), "not on the origin"},
		{q("return_to", "http://ui.example.com/", "code_challenge", ch, "code_challenge_method", "S256"), "not on the origin"},
		{q("return_to", "https://ui.example.com/#x", "code_challenge", ch, "code_challenge_method", "S256"), "has a fragment"},
		{q("return_to", "/ui/", "code_challenge", ch, "code_challenge_method", "S256"), "not an absolute URL"},
		{q("return_to", "https://u@ui.example.com/", "code_challenge", ch, "code_challenge_method", "S256"), "not an absolute URL"},
		{q("return_to", uiOrigin+"/"+strings.Repeat("a", 2048), "code_challenge", ch, "code_challenge_method", "S256"), "longer than"},
		{q("return_to", uiOrigin+"/", "code_challenge_method", "S256"), "go together"},
		{q("code_challenge", ch, "code_challenge_method", "S256"), "go together"},
		{q("return_to", uiOrigin+"/", "code_challenge", ch), "must be S256"},
		{q("return_to", uiOrigin+"/", "code_challenge", ch, "code_challenge_method", "plain"), "must be S256"},
		{q("return_to", uiOrigin+"/", "code_challenge", ch[:42], "code_challenge_method", "S256"), "not an S256 challenge"},
		{q("return_to", uiOrigin+"/", "code_challenge", ch[:42]+"=", "code_challenge_method", "S256"), "not an S256 challenge"},
		{q("return_to", uiOrigin+"/", "code_challenge", ch, "code_challenge_method", "S256", "user_code", "BCDFGHJK"), "takes no return_to"},
		{q("return_to", uiOrigin+"/", "code_challenge", ch, "code_challenge_method", "S256", "dpop_jkt", ""), "go together"},
		{q("dpop_jkt", testJKT), "go together"},
		{q("return_to", uiOrigin+"/", "code_challenge", ch, "code_challenge_method", "S256", "dpop_jkt", testJKT[:42]), "not a JWK SHA-256 thumbprint"},
		{q("return_to", uiOrigin+"/", "code_challenge", ch, "code_challenge_method", "S256", "dpop_jkt", testJKT+"="), "not a JWK SHA-256 thumbprint"},
		{q("return_to", uiOrigin+"/", "code_challenge", ch, "code_challenge_method", "S256", "prompt", "consent"), "prompt must be login"},
		{q("dpop_jkt", "", "prompt", "login"), "prompt goes with the login of a web UI"},
	} {
		f := newLoginFixture(t)
		rr := f.do(http.MethodGet, "/-/oidc/login"+tc.query, nil)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), tc.why) || len(rr.Result().Cookies()) != 0 {
			t.Errorf("%s: %d, cookies %v, body %s; want 400 saying %q", tc.query, rr.Code, rr.Result().Cookies(), rr.Body, tc.why)
		}
		if f.p.state != "" {
			t.Errorf("%s: the IdP was asked", tc.query)
		}
	}
	// The server's own origin is a page to return to, with the default port or without.
	f := newLoginFixture(t)
	f.handOff("https://lajstrom.example:443/ui/", verifier)
}

// TestOriginOf checks the origin of a URL as a browser sends it.
func TestOriginOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://UI.Example.com/ui/?x=1": "https://ui.example.com",
		"https://ui.example.com:443/":    "https://ui.example.com",
		"https://ui.example.com:8443/":   "https://ui.example.com:8443",
		"http://localhost:80/":           "http://localhost",
		"http://localhost:5173/":         "http://localhost:5173",
		"http://[::1]:5173/":             "http://[::1]:5173",
	} {
		u, err := url.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := OriginOf(u); got != want {
			t.Errorf("OriginOf(%s) = %s, want %s", in, got, want)
		}
	}
}

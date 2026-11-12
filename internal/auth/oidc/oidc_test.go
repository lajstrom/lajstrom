package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testNow is the clock of the tests.
var testNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// testKey is a signing key of the fake IdP with the JWK it publishes.
type testKey struct {
	kid    string
	method jwt.SigningMethod
	priv   crypto.Signer
	jwk    map[string]string
}

var (
	keyOnce sync.Once
	rsaKey  *rsa.PrivateKey
	ecKey   *ecdsa.PrivateKey
	edKey   ed25519.PrivateKey
)

// keys returns an RS256, an ES256 and an EdDSA key, made once per test binary.
func keys(t *testing.T) (rs, es, ed testKey) {
	t.Helper()
	keyOnce.Do(func() {
		var err error
		if rsaKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if ecKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			panic(err)
		}
		if _, edKey, err = ed25519.GenerateKey(rand.Reader); err != nil {
			panic(err)
		}
	})
	enc := base64.RawURLEncoding.EncodeToString
	ecPub, _ := ecKey.PublicKey.Bytes() // 0x04 ‖ x ‖ y
	rs = testKey{"rs1", jwt.SigningMethodRS256, rsaKey, map[string]string{"kty": "RSA", "kid": "rs1", "use": "sig",
		"n": enc(rsaKey.N.Bytes()), "e": enc(big.NewInt(int64(rsaKey.E)).Bytes())}}
	es = testKey{"es1", jwt.SigningMethodES256, ecKey, map[string]string{"kty": "EC", "kid": "es1", "crv": "P-256",
		"x": enc(ecPub[1:33]), "y": enc(ecPub[33:])}}
	ed = testKey{"ed1", jwt.SigningMethodEdDSA, edKey, map[string]string{"kty": "OKP", "kid": "ed1", "crv": "Ed25519",
		"x": enc(edKey.Public().(ed25519.PublicKey))}}
	return rs, es, ed
}

// idp is a fake IdP: a discovery document, a JWKS of the keys it is given, and a token endpoint
// that answers the ID token of the test for the one code it knows.
type idp struct {
	srv      *httptest.Server
	mu       sync.Mutex
	jwks     []map[string]string
	idToken  string
	issuer   string // the issuer of the discovery document; the server's URL by default
	down     atomic.Bool
	jwksGets atomic.Int64
	discGets atomic.Int64
	lastForm url.Values
	lastAuth [2]string
}

const (
	testCode     = "the-code"
	testVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	testClient   = "lajstrom web"
	testSecret   = "s3cret&="
)

func newIDP(t *testing.T, published ...testKey) *idp {
	t.Helper()
	p := &idp{}
	for _, k := range published {
		p.jwks = append(p.jwks, k.jwk)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		p.discGets.Add(1)
		if p.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		p.mu.Lock()
		iss := p.issuer
		p.mu.Unlock()
		if iss == "" {
			iss = p.srv.URL
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": iss, "authorization_endpoint": p.srv.URL + "/auth?tenant=t1",
			"token_endpoint": p.srv.URL + "/token", "jwks_uri": p.srv.URL + "/jwks"})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		p.jwksGets.Add(1)
		if p.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": p.jwks})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		user, pass, _ := r.BasicAuth()
		p.mu.Lock()
		p.lastForm, p.lastAuth = r.PostForm, [2]string{user, pass}
		tok := p.idToken
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("code") != testCode {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"the code is <script>bad</script>"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at", "token_type": "Bearer", "id_token": tok})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// client returns a Client of the IdP whose clock is *now.
func (p *idp) client(t *testing.T, now *atomic.Pointer[time.Time]) *Client {
	t.Helper()
	c, err := New(Config{Issuer: p.srv.URL, ClientID: testClient, ClientSecret: testSecret,
		RedirectURL: "https://lajstrom.example/-/oidc/callback", Scopes: []string{"profile", "groups"},
		UsernameClaim: "preferred_username", GroupsClaim: "groups", AllowHTTP: true,
		KeepGroup: func(s string) error {
			if strings.TrimSpace(s) != s || s == "" {
				return errors.New("not a group")
			}
			return nil
		},
		Now: func() time.Time { return *now.Load() }, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func clockAt(t time.Time) *atomic.Pointer[time.Time] {
	var c atomic.Pointer[time.Time]
	c.Store(&t)
	return &c
}

// claims returns valid claims of the IdP for the nonce n.
func (p *idp) claims(n string) jwt.MapClaims {
	return jwt.MapClaims{"iss": p.srv.URL, "sub": "user-1", "aud": testClient, "nonce": n,
		"exp": testNow.Add(5 * time.Minute).Unix(), "iat": testNow.Unix(),
		"preferred_username": "alice", "groups": []any{"dev", "ops", "dev", " spaced", 7}}
}

// sign signs claims with k, its kid in the header unless kid is "-".
func sign(t *testing.T, k testKey, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(k.method, claims)
	tok.Header["kid"] = k.kid
	s, err := tok.SignedString(k.priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (p *idp) answer(tok string) {
	p.mu.Lock()
	p.idToken = tok
	p.mu.Unlock()
}

func TestExchange(t *testing.T) {
	rs, es, ed := keys(t)
	for _, k := range []testKey{rs, es, ed} {
		t.Run(k.method.Alg(), func(t *testing.T) {
			p := newIDP(t, rs, es, ed)
			c := p.client(t, clockAt(testNow))
			p.answer(sign(t, k, p.claims("n-1")))
			id, err := c.Exchange(context.Background(), testCode, testVerifier, "n-1")
			if err != nil {
				t.Fatal(err)
			}
			want := Identity{Issuer: p.srv.URL, Subject: "user-1", Username: "alice", Groups: []string{"dev", "ops"}, Dropped: 2}
			if fmt.Sprint(*id) != fmt.Sprint(want) {
				t.Errorf("Exchange = %+v, want %+v", *id, want)
			}
			p.mu.Lock()
			form, auth := p.lastForm, p.lastAuth
			p.mu.Unlock()
			if form.Get("grant_type") != "authorization_code" || form.Get("code_verifier") != testVerifier ||
				form.Get("redirect_uri") != "https://lajstrom.example/-/oidc/callback" || form.Has("client_secret") {
				t.Errorf("the token request's form: %v", form)
			}
			// RFC 6749 §2.3.1: the client id and secret are form-encoded inside Basic.
			if auth != [2]string{url.QueryEscape(testClient), url.QueryEscape(testSecret)} {
				t.Errorf("the token request's Basic credentials: %q", auth)
			}
		})
	}
}

// TestVerifyRefusals: every check of docs/spec/rbac.md §8 refuses with ErrRefused.
func TestVerifyRefusals(t *testing.T) {
	rs, es, _ := keys(t)
	p := newIDP(t, rs, es)
	c := p.client(t, clockAt(testNow))
	pinned := rs
	pinned.kid, pinned.jwk = "rs-pinned", map[string]string{"kty": "RSA", "kid": "rs-pinned", "alg": "RS256",
		"n": rs.jwk["n"], "e": rs.jwk["e"]}
	p.jwks = append(p.jwks, pinned.jwk)
	with := func(change func(jwt.MapClaims)) jwt.MapClaims {
		cl := p.claims("n-1")
		change(cl)
		return cl
	}
	hmac := func() string {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, p.claims("n-1"))
		tok.Header["kid"] = "rs1"
		s, _ := tok.SignedString([]byte(testSecret))
		return s
	}
	none := func() string {
		s, _ := jwt.NewWithClaims(jwt.SigningMethodNone, p.claims("n-1")).SignedString(jwt.UnsafeAllowNoneSignatureType)
		return s
	}
	confused := func() string { // an ES256 header over the RSA key's kid
		k := es
		k.kid = "rs1"
		return sign(t, k, p.claims("n-1"))
	}
	ps := rs
	ps.method, ps.kid = jwt.SigningMethodPS256, "rs-pinned"
	for _, tt := range []struct {
		name, tok string
	}{
		{"another issuer", sign(t, rs, with(func(c jwt.MapClaims) { c["iss"] = "https://evil.example" }))},
		{"another audience", sign(t, rs, with(func(c jwt.MapClaims) { c["aud"] = "other-client" }))},
		{"no audience", sign(t, rs, with(func(c jwt.MapClaims) { delete(c, "aud") }))},
		{"azp of another client", sign(t, rs, with(func(c jwt.MapClaims) { c["aud"] = []string{testClient, "x"}; c["azp"] = "x" }))},
		{"expired beyond the leeway", sign(t, rs, with(func(c jwt.MapClaims) { c["exp"] = testNow.Add(-Leeway - time.Second).Unix() }))},
		{"no exp", sign(t, rs, with(func(c jwt.MapClaims) { delete(c, "exp") }))},
		{"iat ahead beyond the leeway", sign(t, rs, with(func(c jwt.MapClaims) { c["iat"] = testNow.Add(Leeway + time.Minute).Unix() }))},
		{"nbf ahead beyond the leeway", sign(t, rs, with(func(c jwt.MapClaims) { c["nbf"] = testNow.Add(Leeway + time.Minute).Unix() }))},
		{"another nonce", sign(t, rs, with(func(c jwt.MapClaims) { c["nonce"] = "n-2" }))},
		{"no nonce", sign(t, rs, with(func(c jwt.MapClaims) { delete(c, "nonce") }))},
		{"no sub", sign(t, rs, with(func(c jwt.MapClaims) { delete(c, "sub") }))},
		{"HS256 with the client secret", hmac()},
		{"alg none", none()},
		{"an algorithm that is not its key's", confused()},
		{"an algorithm its JWK does not pin", sign(t, ps, p.claims("n-1"))},
		{"an unknown kid", sign(t, testKey{"nope", rs.method, rs.priv, nil}, p.claims("n-1"))},
		{"a changed payload", func() string {
			parts := strings.Split(sign(t, rs, p.claims("n-1")), ".")
			parts[1] = base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"x"}`))
			return strings.Join(parts, ".")
		}()},
		{"not a JWT", "abc"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p.answer(tt.tok)
			if id, err := c.Exchange(context.Background(), testCode, testVerifier, "n-1"); !errors.Is(err, ErrRefused) {
				t.Errorf("Exchange = %+v, %v; want ErrRefused", id, err)
			}
		})
	}
	// Within the leeway: accepted.
	p.answer(sign(t, rs, with(func(c jwt.MapClaims) { c["exp"] = testNow.Add(-Leeway + time.Second).Unix() })))
	if _, err := c.Exchange(context.Background(), testCode, testVerifier, "n-1"); err != nil {
		t.Errorf("an exp within the leeway: %v", err)
	}
}

// TestKeyRotation: an unknown kid reads the JWKS again, at most once per keysRefetchEvery; the
// set is read again when it is keysMaxAge old; an IdP that stops answering keeps the last set.
func TestKeyRotation(t *testing.T) {
	rs, es, _ := keys(t)
	p := newIDP(t, rs)
	now := clockAt(testNow)
	c := p.client(t, now)
	exchange := func(k testKey) error {
		cl := p.claims("n")
		cl["exp"] = now.Load().Add(time.Minute).Unix()
		cl["iat"] = now.Load().Unix()
		p.answer(sign(t, k, cl))
		_, err := c.Exchange(context.Background(), testCode, testVerifier, "n")
		return err
	}
	if err := exchange(rs); err != nil || p.jwksGets.Load() != 1 {
		t.Fatalf("first login: %v, %d JWKS reads", err, p.jwksGets.Load())
	}
	// The IdP rotates to es1; the first token of it reads the JWKS again.
	p.mu.Lock()
	p.jwks = append(p.jwks, es.jwk)
	p.mu.Unlock()
	if err := exchange(es); !errors.Is(err, ErrRefused) || p.jwksGets.Load() != 1 {
		t.Fatalf("a new kid within %v of the read: %v, %d reads; want ErrRefused, no read", keysRefetchEvery, err, p.jwksGets.Load())
	}
	later := testNow.Add(keysRefetchEvery)
	now.Store(&later)
	if err := exchange(es); err != nil || p.jwksGets.Load() != 2 {
		t.Fatalf("a new kid after %v: %v, %d reads; want 2", keysRefetchEvery, err, p.jwksGets.Load())
	}
	unknown := testKey{"rs-unknown", rs.method, rs.priv, nil}
	for range 5 {
		if err := exchange(unknown); !errors.Is(err, ErrRefused) {
			t.Fatalf("an unknown kid: %v", err)
		}
	}
	if p.jwksGets.Load() != 2 {
		t.Errorf("five made-up kids within %v read the JWKS %d times, want none", keysRefetchEvery, p.jwksGets.Load()-2)
	}
	// The IdP goes down: the keys stay, past their age, and the discovery document too.
	p.down.Store(true)
	muchLater := testNow.Add(2 * keysMaxAge)
	now.Store(&muchLater)
	if err := exchange(rs); err != nil {
		t.Errorf("a known kid while the IdP is down: %v", err)
	}
}

// TestDiscovery: the issuer must be the configured one, the endpoints of an allowed scheme; an
// IdP that never answered is ErrUnavailable.
func TestDiscovery(t *testing.T) {
	rs, _, _ := keys(t)
	ctx := context.Background()
	p := newIDP(t, rs)
	c := p.client(t, clockAt(testNow))
	u, err := c.AuthCodeURL(ctx, "st", "nn", "ch")
	if err != nil {
		t.Fatal(err)
	}
	q := mustParse(t, u).Query()
	for k, want := range map[string]string{"tenant": "t1", "response_type": "code", "client_id": testClient,
		"redirect_uri": "https://lajstrom.example/-/oidc/callback", "scope": "openid profile groups", "state": "st",
		"nonce": "nn", "code_challenge": "ch", "code_challenge_method": "S256"} {
		if q.Get(k) != want {
			t.Errorf("AuthCodeURL %s = %q, want %q", k, q.Get(k), want)
		}
	}

	other := newIDP(t, rs)
	other.issuer = "https://evil.example"
	if _, err := other.client(t, clockAt(testNow)).AuthCodeURL(ctx, "s", "n", "c"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a discovery document of another issuer: %v, want ErrUnavailable", err)
	}
	down := newIDP(t, rs)
	down.down.Store(true)
	if _, err := down.client(t, clockAt(testNow)).AuthCodeURL(ctx, "s", "n", "c"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("an IdP that does not answer: %v, want ErrUnavailable", err)
	}
	strict, err := New(Config{Issuer: "https://idp.example", ClientID: "c", RedirectURL: "https://l.example/-/oidc/callback"})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkEndpoint(p.srv.URL+"/token", strict.cfg.AllowHTTP); err == nil {
		t.Error("an http endpoint without AllowHTTP is accepted")
	}
}

// TestRedeemRefusals: a refused code is ErrRefused with the error code and never the IdP's
// description; a server error is ErrUnavailable.
func TestRedeemRefusals(t *testing.T) {
	rs, _, _ := keys(t)
	p := newIDP(t, rs)
	c := p.client(t, clockAt(testNow))
	_, err := c.Exchange(context.Background(), "another code", testVerifier, "n")
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), `"invalid_grant"`) || strings.Contains(err.Error(), "script") {
		t.Errorf("a refused code: %v; want ErrRefused naming invalid_grant only", err)
	}
	p.answer("")
	if _, err := c.Exchange(context.Background(), testCode, testVerifier, "n"); !errors.Is(err, ErrRefused) {
		t.Errorf("an answer without an ID token: %v, want ErrRefused", err)
	}
	if got := errorCode("bad\"code"); got != "?" {
		t.Errorf("errorCode of a quote = %q, want ?", got)
	}
}

// TestJWKs: the keys that are skipped.
func TestJWKs(t *testing.T) {
	rs, es, ed := keys(t)
	weak, err := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // a weak key, to be refused
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	for _, tt := range []struct {
		name string
		j    jwk
		ok   bool
	}{
		{"RSA", jwk{Kty: "RSA", N: rs.jwk["n"], E: rs.jwk["e"]}, true},
		{"EC P-256", jwk{Kty: "EC", Crv: "P-256", X: es.jwk["x"], Y: es.jwk["y"]}, true},
		{"Ed25519", jwk{Kty: "OKP", Crv: "Ed25519", X: ed.jwk["x"]}, true},
		{"RSA of 1024 bits", jwk{Kty: "RSA", N: enc(weak.N.Bytes()), E: rs.jwk["e"]}, false},
		{"RSA with an even exponent", jwk{Kty: "RSA", N: rs.jwk["n"], E: enc([]byte{2})}, false},
		{"EC off its curve", jwk{Kty: "EC", Crv: "P-256", X: es.jwk["x"], Y: es.jwk["x"]}, false},
		{"EC of a short x", jwk{Kty: "EC", Crv: "P-256", X: enc([]byte{1, 2}), Y: es.jwk["y"]}, false},
		{"EC on secp256k1", jwk{Kty: "EC", Crv: "secp256k1", X: es.jwk["x"], Y: es.jwk["y"]}, false},
		{"X25519", jwk{Kty: "OKP", Crv: "X25519", X: ed.jwk["x"]}, false},
		{"oct", jwk{Kty: "oct"}, false},
	} {
		_, err := tt.j.public()
		if (err == nil) != tt.ok {
			t.Errorf("%s: %v, want ok %v", tt.name, err, tt.ok)
		}
	}
}

func TestNew(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  Config
	}{
		{"no issuer", Config{ClientID: "c", RedirectURL: "https://l/cb"}},
		{"an http issuer", Config{Issuer: "http://idp", ClientID: "c", RedirectURL: "https://l/cb"}},
		{"an issuer with a query", Config{Issuer: "https://idp/?a=b", ClientID: "c", RedirectURL: "https://l/cb"}},
		{"no client", Config{Issuer: "https://idp", RedirectURL: "https://l/cb"}},
		{"no redirect URL", Config{Issuer: "https://idp", ClientID: "c"}},
	} {
		if _, err := New(tt.cfg); err == nil {
			t.Errorf("New with %s: no error", tt.name)
		}
	}
	c, err := New(Config{Issuer: "https://idp", ClientID: "c", RedirectURL: "https://l/cb", Scopes: []string{"email"}})
	if err != nil || strings.Join(c.cfg.Scopes, " ") != "openid email" {
		t.Errorf("New adds openid: %v, %v", c, err)
	}
}

// TestPKCE: the example of RFC 7636 Appendix B, and fresh secrets.
func TestPKCE(t *testing.T) {
	if got := Challenge(testVerifier); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Errorf("Challenge = %q", got)
	}
	a, b := NewSecret(), NewSecret()
	if len(a) != 43 || a == b {
		t.Errorf("NewSecret = %q, %q", a, b)
	}
	if h := Hash("x"); len(h) != 64 || h == Hash("y") {
		t.Errorf("Hash = %q", h)
	}
}

func mustParse(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

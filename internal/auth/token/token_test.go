package token

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
)

const service = "art.test"

// alice is a user principal, user:<uid>, as the authenticator makes it (docs/spec/rbac.md §1).
var alice = auth.Principal{Kind: auth.KindUser, Name: "019a6c3e-7b1e-7c3d-8e2f-00000000a11c"}

// newKey makes a token key of alg, as keys makes them: the fingerprint is the sha256 of the
// PKIX DER of the public key.
func newKey(t testing.TB, alg string) *keys.TokenKey {
	t.Helper()
	var priv crypto.Signer
	var err error
	switch alg {
	case keys.TokenEdDSA:
		_, priv, err = ed25519.GenerateKey(rand.Reader)
	default:
		priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	must(t, err)
	der, err := x509.MarshalPKIXPublicKey(priv.Public())
	must(t, err)
	sum := sha256.Sum256(der)
	return &keys.TokenKey{Key: index.Key{Kind: keys.KindJWT, Public: der, Fingerprint: hex.EncodeToString(sum[:])},
		Algorithm: alg, Public: priv.Public(), Private: priv}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fakeKeys signs with signer and verifies with verifiers, and counts the loads; err fails them.
// read, when set, runs as TokenSigner reads the signer, before it returns it.
type fakeKeys struct {
	mu        sync.Mutex
	signer    *keys.TokenKey
	verifiers []*keys.TokenKey
	loads     int
	err       error
	read      func()
}

func (f *fakeKeys) TokenSigner(context.Context) (*keys.TokenKey, error) {
	if f.read != nil {
		f.read()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signer, f.err
}

func (f *fakeKeys) TokenVerifiers(context.Context) ([]*keys.TokenKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.err != nil {
		return nil, f.err
	}
	return f.verifiers, nil
}

func (f *fakeKeys) set(signer *keys.TokenKey, verifiers ...*keys.TokenKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signer, f.verifiers = signer, verifiers
}

func (f *fakeKeys) loadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

// clock is a clock the test moves.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// t0 is the time the tests issue at.
var t0 = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

// pair returns an Issuer and a Verifier over k, each with a clock of its own at t0, and the
// verifier's debug log.
func pair(k Keys) (*Issuer, *Verifier, *clock, *bytes.Buffer) {
	var logged bytes.Buffer
	ic, vc := &clock{now: t0}, &clock{now: t0}
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewIssuer(Options{Keys: k, Now: ic.Now}), NewVerifier(Options{Keys: k, Now: vc.Now, Log: log}), vc, &logged
}

// segments decodes the header and the payload of a JWT.
func segments(t *testing.T, raw string) (header, payload map[string]any) {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("a JWT of %d segments", len(parts))
	}
	for i, into := range []*map[string]any{&header, &payload} {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		must(t, err)
		must(t, json.Unmarshal(b, into))
	}
	return header, payload
}

// TestIssueVerify issues and verifies a token with each algorithm: the header (alg of the key,
// kid its fingerprint, typ JWT), the claims of rbac.md §4, and the grant the verifier returns.
func TestIssueVerify(t *testing.T) {
	for _, alg := range []string{keys.TokenES256, keys.TokenEdDSA} {
		t.Run(alg, func(t *testing.T) {
			k := newKey(t, alg)
			issuer, verifier, _, _ := pair(&fakeKeys{signer: k, verifiers: []*keys.TokenKey{k}})
			access := []Access{
				{Type: TypeRepository, Name: "raw/assets/zeta", Actions: []string{ActionPull}},
				{Type: TypeRepository, Name: "raw/assets/alpha", Actions: []string{ActionPull, ActionPush}},
				{Type: TypeRegistry, Name: "catalog", Actions: []string{"*"}},
			}
			tok, err := issuer.Issue(t.Context(), alice, service, access)
			must(t, err)
			jti := checkIssued(t, tok, k)
			g, err := verifier.Verify(t.Context(), tok.Raw, service)
			must(t, err)
			if g.Principal != alice || len(g.Access) != 3 || !g.Allows("raw/assets/alpha", ActionPush) ||
				g.Allows("raw/assets/zeta", ActionPush) || !g.Catalog() {
				t.Errorf("grant %+v", g)
			}
			if _, ok := g.Repository("raw/assets/beta"); ok {
				t.Error("the grant names a repository the token does not")
			}
			second, err := issuer.Issue(t.Context(), auth.Anonymous, service, nil)
			must(t, err)
			_, payload := segments(t, second.Raw)
			if payload["jti"] == jti || payload["sub"] != "anonymous" || fmt.Sprint(payload["access"]) != "[]" {
				t.Errorf("an anonymous token without access: %v", payload)
			}
		})
	}
}

// checkIssued checks the token TestIssueVerify issued at t0 with key k for alice, and returns
// its jti: the header, iss, aud, sub, the times, a jti of 16 random bytes, the access sorted.
func checkIssued(t *testing.T, tok *Token, k *keys.TokenKey) string {
	t.Helper()
	if tok.KeyID != k.ID() || !tok.IssuedAt.Equal(t0) || !tok.ExpiresAt.Equal(t0.Add(20*time.Minute)) {
		t.Errorf("token %s, expires %v", tok, tok.ExpiresAt)
	}
	header, payload := segments(t, tok.Raw)
	if header["alg"] != k.Algorithm || header["kid"] != k.ID() || header["typ"] != "JWT" || len(header) != 3 {
		t.Errorf("header %v", header)
	}
	jti, _ := payload["jti"].(string)
	if b, err := base64.RawURLEncoding.DecodeString(jti); err != nil || len(b) != 16 {
		t.Errorf("jti %q is not 16 bytes of base64url", jti)
	}
	delete(payload, "jti")
	want := map[string]any{"iss": service, "aud": []any{service}, "sub": alice.String(),
		"iat": float64(t0.Unix()), "nbf": float64(t0.Unix()), "exp": float64(t0.Unix() + 1200),
		"access": []any{
			map[string]any{"type": "registry", "name": "catalog", "actions": []any{"*"}},
			map[string]any{"type": "repository", "name": "raw/assets/alpha", "actions": []any{"pull", "push"}},
			map[string]any{"type": "repository", "name": "raw/assets/zeta", "actions": []any{"pull"}},
		}}
	if fmt.Sprint(payload) != fmt.Sprint(want) {
		t.Errorf("claims\n%v\nwant\n%v", payload, want)
	}
	return jti
}

// TestTokenRedacted checks that a token prints and logs as its kid and time, never as the JWT.
func TestTokenRedacted(t *testing.T) {
	k := newKey(t, keys.TokenES256)
	issuer, _, _, _ := pair(&fakeKeys{signer: k})
	tok, err := issuer.Issue(t.Context(), alice, service, nil)
	must(t, err)
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("t", "token", tok)
	sig := tok.Raw[strings.LastIndexByte(tok.Raw, '.')+1:]
	for _, s := range []string{fmt.Sprint(tok), fmt.Sprintf("%+v", tok), fmt.Sprintf("%#v", tok), logged.String()} {
		if !strings.Contains(s, k.ID()) || strings.Contains(s, sig) {
			t.Errorf("a token prints as %q", s)
		}
	}
}

// TestIssueFails checks the errors of Issue: the keys fail, and a key of an algorithm that
// signs nothing.
func TestIssueFails(t *testing.T) {
	errDown := errors.New("index down")
	issuer, _, _, _ := pair(&fakeKeys{err: errDown})
	if _, err := issuer.Issue(t.Context(), alice, service, nil); !errors.Is(err, errDown) {
		t.Errorf("Issue with failing keys: %v", err)
	}
	k := newKey(t, keys.TokenES256)
	k.Algorithm = "HS256"
	issuer, _, _, _ = pair(&fakeKeys{signer: k})
	if _, err := issuer.Issue(t.Context(), alice, service, nil); err == nil || !strings.Contains(err.Error(), `"HS256", which signs no token`) {
		t.Errorf("Issue with an HS256 key: %v", err)
	}
	k = newKey(t, keys.TokenES256)
	k.Private = nil
	issuer, _, _, _ = pair(&fakeKeys{signer: k})
	if _, err := issuer.Issue(t.Context(), alice, service, nil); err == nil || !strings.Contains(err.Error(), "token: sign with") {
		t.Errorf("Issue with a key without its private key: %v", err)
	}
}

// TestVerifyTimes checks exp, nbf and iat with the leeway of 60 s: valid until 20 minutes after
// iat plus the leeway, and from iat minus the leeway.
func TestVerifyTimes(t *testing.T) {
	k := newKey(t, keys.TokenES256)
	issuer, verifier, vc, logged := pair(&fakeKeys{signer: k, verifiers: []*keys.TokenKey{k}})
	tok, err := issuer.Issue(t.Context(), alice, service, nil)
	must(t, err)
	for _, tt := range []struct {
		name  string
		at    time.Time
		valid bool
	}{
		{"at iat", t0, true},
		{"just before exp", t0.Add(20*time.Minute - time.Second), true},
		{"expired, within the leeway", t0.Add(20*time.Minute + 59*time.Second), true},
		{"expired, outside the leeway", t0.Add(20*time.Minute + 61*time.Second), false},
		{"not yet valid, within the leeway", t0.Add(-30 * time.Second), true},
		{"not yet valid, outside the leeway", t0.Add(-2 * time.Minute), false},
	} {
		vc.set(tt.at)
		_, err := verifier.Verify(t.Context(), tok.Raw, service)
		if tt.valid != (err == nil) || (err != nil && !errors.Is(err, ErrInvalidToken)) {
			t.Errorf("%s: %v, want valid: %t", tt.name, err, tt.valid)
		}
	}
	if !strings.Contains(logged.String(), "token is expired") || !strings.Contains(logged.String(), "token is not valid yet") {
		t.Errorf("the reasons are not logged:\n%s", logged)
	}
}

// sign returns a token of claims signed with method and key, with the header kid.
func sign(t *testing.T, method jwt.SigningMethod, key any, kid string, claims jwt.Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	raw, err := tok.SignedString(key)
	must(t, err)
	return raw
}

// claims returns valid claims for service at t0.
func claims(sub string) Claims {
	return Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: service, Subject: sub, Audience: jwt.ClaimStrings{service},
		IssuedAt: jwt.NewNumericDate(t0), NotBefore: jwt.NewNumericDate(t0), ExpiresAt: jwt.NewNumericDate(t0.Add(Lifetime))}}
}

// TestVerifyRefusals checks that every token Lajstrom did not issue, or not for this service, is
// ErrInvalidToken, with its reason logged and never the token: the alg confusions (HS256 with
// the public key as the secret, alg none, a token of key A that names key B, an ES256 token
// that names an EdDSA key), another audience or issuer, an unknown kid, no kid, a changed
// signature, a malformed token, no exp, and a subject that is not a principal.
func TestVerifyRefusals(t *testing.T) {
	a, b, ed := newKey(t, keys.TokenES256), newKey(t, keys.TokenES256), newKey(t, keys.TokenEdDSA)
	issuer, verifier, _, logged := pair(&fakeKeys{signer: a, verifiers: []*keys.TokenKey{a, b, ed}})
	good, err := issuer.Issue(t.Context(), alice, service, nil)
	must(t, err)
	if _, err := verifier.Verify(t.Context(), good.Raw, service); err != nil {
		t.Fatalf("the good token: %v", err)
	}
	other, err := issuer.Issue(t.Context(), alice, "other.test", nil)
	must(t, err)
	wrongIssuer := claims(alice.String())
	wrongIssuer.Issuer = "other.test"
	noExp := claims(alice.String())
	noExp.ExpiresAt = nil
	none := sign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, a.ID(), claims(alice.String()))
	changed := []byte(good.Raw)
	changed[strings.LastIndexByte(good.Raw, '.')+5] ^= 0x01
	tests := map[string]string{
		"HS256 with the public key as the secret": sign(t, jwt.SigningMethodHS256, a.Key.Public, a.ID(), claims(alice.String())),
		"alg none":                       none,
		"alg none without a signature":   strings.TrimSuffix(none, "."),
		"a token of key A that names B":  sign(t, jwt.SigningMethodES256, a.Private, b.ID(), claims(alice.String())),
		"ES256 that names an EdDSA key":  sign(t, jwt.SigningMethodES256, a.Private, ed.ID(), claims(alice.String())),
		"another audience":               other.Raw,
		"another issuer":                 sign(t, jwt.SigningMethodES256, a.Private, a.ID(), wrongIssuer),
		"an unknown kid":                 sign(t, jwt.SigningMethodES256, a.Private, strings.Repeat("0", 64), claims(alice.String())),
		"a user by name, of before #182": sign(t, jwt.SigningMethodES256, a.Private, a.ID(), claims("user:alice")),
		"no kid":                         sign(t, jwt.SigningMethodES256, a.Private, "", claims(alice.String())),
		"a changed signature":            string(changed),
		"malformed":                      "not.a.jwt",
		"empty":                          "",
		"no exp":                         sign(t, jwt.SigningMethodES256, a.Private, a.ID(), noExp),
		"a subject that is no principal": sign(t, jwt.SigningMethodES256, a.Private, a.ID(), claims("bogus")),
		"an empty subject":               sign(t, jwt.SigningMethodES256, a.Private, a.ID(), claims("")),
	}
	for name, raw := range tests {
		if _, err := verifier.Verify(t.Context(), raw, service); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: %v, want ErrInvalidToken", name, err)
		}
		if sig := raw[strings.LastIndexByte(raw, '.')+1:]; len(sig) > 8 && strings.Contains(logged.String(), sig) {
			t.Errorf("%s: the log shows the signature", name)
		}
	}
	for _, reason := range []string{"signing method HS256 is invalid", "signing method none is invalid", "no key has this kid",
		"the header has no kid", "token has invalid audience", "token has invalid issuer", "the subject is not a principal",
		"token is missing required claim: exp claim is required", "verification error"} {
		if !strings.Contains(logged.String(), reason) {
			t.Errorf("the reason %q is not logged:\n%s", reason, logged)
		}
	}
	if ErrInvalidToken.Error() != "token: invalid bearer token" {
		t.Errorf("ErrInvalidToken says %q: it must not say why", ErrInvalidToken)
	}
}

// TestVerifierReload checks the cache of verification keys: a kid it lacks (a rotation on
// another node) is refused within the second of the last load, and found by the reload a
// second later; a key the reload no longer returns (dropped on another node) stops verifying
// a second after the last load; a clock that went back reloads.
func TestVerifierReload(t *testing.T) {
	old, rotated := newKey(t, keys.TokenES256), newKey(t, keys.TokenES256)
	fk := &fakeKeys{signer: old, verifiers: []*keys.TokenKey{old}}
	issuer, verifier, vc, _ := pair(fk)
	oldToken, err := issuer.Issue(t.Context(), alice, service, nil)
	must(t, err)
	if _, err := verifier.Verify(t.Context(), oldToken.Raw, service); err != nil || fk.loadCount() != 1 {
		t.Fatalf("first verification: %v, %d loads", err, fk.loadCount())
	}
	fk.set(rotated, rotated, old)
	newToken, err := issuer.Issue(t.Context(), alice, service, nil)
	must(t, err)
	if _, err := verifier.Verify(t.Context(), newToken.Raw, service); !errors.Is(err, ErrInvalidToken) || fk.loadCount() != 1 {
		t.Errorf("a new kid within the second of the last load: %v, %d loads; want refused without a load", err, fk.loadCount())
	}
	vc.set(t0.Add(time.Second))
	if _, err := verifier.Verify(t.Context(), newToken.Raw, service); err != nil || fk.loadCount() != 2 {
		t.Errorf("the new kid a second later: %v, %d loads; want valid after one load", err, fk.loadCount())
	}
	fk.set(rotated, rotated)
	if _, err := verifier.Verify(t.Context(), oldToken.Raw, service); err != nil {
		t.Errorf("the old token within the second of the last load: %v", err)
	}
	vc.set(t0.Add(2 * time.Second))
	if _, err := verifier.Verify(t.Context(), oldToken.Raw, service); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("the old token a second after its key was dropped: %v", err)
	}
	vc.set(t0.Add(-time.Hour))
	before := fk.loadCount()
	_, _ = verifier.Verify(t.Context(), newToken.Raw, service)
	if fk.loadCount() != before+1 {
		t.Errorf("a clock that went back: %d loads, want one", fk.loadCount()-before)
	}
}

// TestVerifierFlood checks the bound of the reloads: a thousand made-up kids within a second
// cost no load beyond the first, and a thousand more in the next second one.
func TestVerifierFlood(t *testing.T) {
	k := newKey(t, keys.TokenES256)
	fk := &fakeKeys{signer: k, verifiers: []*keys.TokenKey{k}}
	_, verifier, vc, _ := pair(fk)
	flood := func() {
		for i := range 1000 {
			made := sign(t, jwt.SigningMethodES256, k.Private, fmt.Sprintf("%064x", i), claims(alice.String()))
			if _, err := verifier.Verify(t.Context(), made, service); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("a made-up kid: %v", err)
			}
		}
	}
	flood()
	if fk.loadCount() != 1 {
		t.Errorf("1000 made-up kids within a second: %d loads, want 1", fk.loadCount())
	}
	vc.set(t0.Add(time.Second))
	flood()
	if fk.loadCount() != 2 {
		t.Errorf("1000 made-up kids in the next second: %d loads in all, want 2", fk.loadCount())
	}
}

// TestVerifierLoadFails checks a reload that fails: the keys of the last load answer, with a
// warning; before any load succeeded, the verification fails with the error of the keys, not
// ErrInvalidToken.
func TestVerifierLoadFails(t *testing.T) {
	k := newKey(t, keys.TokenES256)
	fk := &fakeKeys{signer: k, verifiers: []*keys.TokenKey{k}}
	issuer, verifier, vc, logged := pair(fk)
	tok, err := issuer.Issue(t.Context(), alice, service, nil)
	must(t, err)
	_, err = verifier.Verify(t.Context(), tok.Raw, service)
	must(t, err)
	errDown := errors.New("index down")
	fk.mu.Lock()
	fk.err = errDown
	fk.mu.Unlock()
	vc.set(t0.Add(time.Second))
	if _, err := verifier.Verify(t.Context(), tok.Raw, service); err != nil {
		t.Errorf("a failed reload: %v, want the last keys to answer", err)
	}
	if !strings.Contains(logged.String(), "token verification keys not reloaded; the last ones answer") {
		t.Errorf("the failed reload is not logged:\n%s", logged)
	}
	_, fresh, _, _ := pair(fk)
	if _, err := fresh.Verify(t.Context(), tok.Raw, service); !errors.Is(err, errDown) || errors.Is(err, ErrInvalidToken) {
		t.Errorf("no keys loaded yet and the load fails: %v, want the error of the keys", err)
	}
}

// linked returns an Issuer that tells a Verifier its keys, as on one replica, both over k and
// one clock at t0.
func linked(k Keys) (*Issuer, *Verifier, *clock) {
	c := &clock{now: t0}
	v := NewVerifier(Options{Keys: k, Now: c.Now, Log: slog.New(slog.DiscardHandler)})
	return NewIssuer(Options{Keys: k, Verifier: v, Now: c.Now}), v, c
}

// TestVerifierLearnsIssuedKey checks #578: a token of a key rotated in within the second of the
// last load verifies at once on the replica that issued it, without a load when the load is
// older than the issue and with one when it is not, while a replica that did not issue it
// refuses it until its next reload, a second after its last.
func TestVerifierLearnsIssuedKey(t *testing.T) {
	old := newKey(t, keys.TokenES256)
	fk := &fakeKeys{signer: old, verifiers: []*keys.TokenKey{old}}
	issuer, verifier, c := linked(fk)
	other := NewVerifier(Options{Keys: fk, Now: c.Now, Log: slog.New(slog.DiscardHandler)})
	first, err := issuer.Issue(t.Context(), alice, service, nil)
	must(t, err)
	for _, v := range []*Verifier{verifier, other} {
		_, err := v.Verify(t.Context(), first.Raw, service)
		must(t, err)
	}
	if fk.loadCount() != 2 {
		t.Fatalf("%d loads, want one per verifier", fk.loadCount())
	}

	rotated := newKey(t, keys.TokenES256)
	fk.set(rotated, rotated, old)
	tok, err := issuer.Issue(t.Context(), alice, service, nil) // at the time of the last load
	must(t, err)
	if _, err := verifier.Verify(t.Context(), tok.Raw, service); err != nil || fk.loadCount() != 3 {
		t.Errorf("a key read at the time of the last load: %v, %d loads; want valid after one load", err, fk.loadCount())
	}

	c.set(t0.Add(100 * time.Millisecond))
	again := newKey(t, keys.TokenEdDSA)
	fk.set(again, again, rotated, old)
	tok, err = issuer.Issue(t.Context(), alice, service, nil)
	must(t, err)
	if _, err := verifier.Verify(t.Context(), tok.Raw, service); err != nil || fk.loadCount() != 3 {
		t.Errorf("a key read after the last load: %v, %d loads; want valid without a load", err, fk.loadCount())
	}
	if _, err := other.Verify(t.Context(), tok.Raw, service); !errors.Is(err, ErrInvalidToken) || fk.loadCount() != 3 {
		t.Errorf("the token on another replica within the second of its load: %v, want refused without a load", err)
	}

	c.set(t0.Add(time.Second))
	if _, err := other.Verify(t.Context(), tok.Raw, service); err != nil {
		t.Errorf("the token on another replica a second after its load: %v", err)
	}
}

// TestVerifierLearnsNoDroppedKey checks that a key the issuer read, then dropped before the
// verifier loaded the keys again, does not come back: the token is refused.
func TestVerifierLearnsNoDroppedKey(t *testing.T) {
	old, dropped := newKey(t, keys.TokenES256), newKey(t, keys.TokenES256)
	fk := &fakeKeys{signer: old, verifiers: []*keys.TokenKey{old}}
	issuer, verifier, c := linked(fk)
	first, err := issuer.Issue(t.Context(), alice, service, nil)
	must(t, err)
	_, err = verifier.Verify(t.Context(), first.Raw, service)
	must(t, err)

	c.set(t0.Add(2 * time.Second))
	fk.set(dropped, dropped, old)
	fk.read = func() { // the drop commits, and a verification loads, while the issuer reads the key
		fk.read = nil
		fk.mu.Lock()
		fk.verifiers = []*keys.TokenKey{old}
		fk.mu.Unlock()
		if _, err := verifier.Verify(t.Context(), first.Raw, service); err != nil {
			t.Errorf("the old token: %v", err)
		}
	}
	tok, err := issuer.Issue(t.Context(), alice, service, nil)
	must(t, err)
	if _, err := verifier.Verify(t.Context(), tok.Raw, service); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a token of a key dropped as it was read: %v, want refused", err)
	}
}

// TestIssueVerifyConcurrent issues and verifies tokens from several goroutines on one replica
// while the key rotates: each token verifies at once. Run with -race.
func TestIssueVerifyConcurrent(t *testing.T) {
	first := newKey(t, keys.TokenES256)
	fk := &fakeKeys{signer: first, verifiers: []*keys.TokenKey{first}}
	verifier := NewVerifier(Options{Keys: fk, Log: slog.New(slog.DiscardHandler)})
	issuer := NewIssuer(Options{Keys: fk, Verifier: verifier})
	var wg sync.WaitGroup
	wg.Go(func() {
		all := []*keys.TokenKey{first}
		for range 20 {
			k := newKey(t, keys.TokenES256)
			all = append([]*keys.TokenKey{k}, all...)
			fk.set(k, all...)
		}
	})
	for range 8 {
		wg.Go(func() {
			for range 50 {
				tok, err := issuer.Issue(t.Context(), alice, service, nil)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := verifier.Verify(t.Context(), tok.Raw, service); err != nil {
					t.Errorf("%s at once on its replica: %v", tok, err)
					return
				}
			}
		})
	}
	wg.Wait()
}

// TestRotationGrace checks two keys of keys.Tokens over a database: after a rotation the old key
// verifies for the grace, 21 minutes, and not after, even a token it signed with a later iat (a
// key that leaked); a rotation with drop ends it at once. The verifier reloads a second after
// its last load.
func TestRotationGrace(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:token-grace?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	m, err := keys.ParseMaster(strings.Repeat("0123456789abcdef", 4))
	must(t, err)
	c := &clock{now: t0}
	tokens, err := keys.NewTokens(keys.TokenOptions{Index: st, Master: m, Now: c.Now, Log: slog.New(slog.DiscardHandler)})
	must(t, err)
	verifier := NewVerifier(Options{Keys: tokens, Now: c.Now, Log: slog.New(slog.DiscardHandler)})
	old, err := tokens.TokenSigner(ctx)
	must(t, err)
	_, err = tokens.RotateToken(ctx, false)
	must(t, err)

	// leaked signs with the old key at iat, as someone who holds it would.
	leaked := func(iat time.Time) string {
		cl := claims("user:019a6c3e-7b1e-7c3d-8e2f-00000000beef") // mallory
		cl.IssuedAt, cl.NotBefore, cl.ExpiresAt = jwt.NewNumericDate(iat), jwt.NewNumericDate(iat), jwt.NewNumericDate(iat.Add(Lifetime))
		return sign(t, jwt.SigningMethodES256, old.Private, old.ID(), cl)
	}
	for _, tt := range []struct {
		after time.Duration
		valid bool
	}{{10 * time.Minute, true}, {21*time.Minute - time.Second, true}, {21*time.Minute + time.Second, false}} {
		c.set(t0.Add(tt.after))
		if _, err := verifier.Verify(ctx, leaked(t0.Add(tt.after)), service); tt.valid != (err == nil) {
			t.Errorf("the old key %v after the rotation: %v, want valid: %t", tt.after, err, tt.valid)
		}
	}

	c.set(t0.Add(time.Hour))
	before, err := tokens.TokenSigner(ctx)
	must(t, err)
	_, err = tokens.RotateToken(ctx, true)
	must(t, err)
	c.set(t0.Add(time.Hour + time.Second))
	cl := claims("user:019a6c3e-7b1e-7c3d-8e2f-00000000beef") // mallory
	cl.IssuedAt, cl.NotBefore, cl.ExpiresAt = jwt.NewNumericDate(c.Now()), jwt.NewNumericDate(c.Now()), jwt.NewNumericDate(c.Now().Add(Lifetime))
	if _, err := verifier.Verify(ctx, sign(t, jwt.SigningMethodES256, before.Private, before.ID(), cl), service); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a token of a dropped key: %v, want refused at once", err)
	}
}

// TestOptions checks the defaults and that the keys are required.
func TestOptions(t *testing.T) {
	o := Options{Keys: &fakeKeys{}}.defaults()
	if o.Now == nil || o.Log == nil {
		t.Errorf("defaults %+v", o)
	}
	defer func() {
		if recover() == nil {
			t.Error("NewIssuer without keys did not panic")
		}
	}()
	NewIssuer(Options{})
}

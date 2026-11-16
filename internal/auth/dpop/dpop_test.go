package dpop_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth/dpop"
	"github.com/lajstrom/lajstrom/internal/auth/dpop/dpoptest"
	"github.com/lajstrom/lajstrom/internal/index"
)

const (
	testURL   = "https://lajstrom.test"
	testToken = "ljs_pat_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
)

var testNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// TestVerify checks the checks of RFC 9449 §4.3 on a GET of /api/v1/repos with a token: every
// way a proof is refused, each with its reason, and the variants of htu that normalize to the
// request's URI.
func TestVerify(t *testing.T) {
	key, other := dpoptest.NewKey(t), dpoptest.NewKey(t)
	htu := testURL + "/api/v1/repos"
	valid := dpoptest.Claims{HTM: http.MethodGet, HTU: htu, Token: testToken, IAT: testNow}
	with := func(f func(c *dpoptest.Claims)) dpoptest.Claims {
		c := valid
		f(&c)
		return c
	}
	tests := []struct {
		name   string
		proof  string   // "" for key.Proof(claims)
		proofs []string // several headers
		claims dpoptest.Claims
		url    string // the Verifier's ExternalURL; "" for testURL, "-" for none
		token  string // "" for testToken, "-" for none
		why    string // "" for accepted
	}{
		{name: "a proof", claims: valid},
		{name: "htu in upper case with the default port", claims: with(func(c *dpoptest.Claims) {
			c.HTU = "HTTPS://Lajstrom.TEST:443/api/v1/repos"
		})},
		{name: "htu with an unreserved character encoded", claims: with(func(c *dpoptest.Claims) { c.HTU = testURL + "/api/v1/%72%65pos" })},
		{name: "htu with dot segments", claims: with(func(c *dpoptest.Claims) { c.HTU = testURL + "/api/./v2/../v1/repos" })},
		{name: "htu with a query and a fragment", claims: with(func(c *dpoptest.Claims) { c.HTU = htu + "?limit=1#top" })},
		{name: "iat 4 minutes ago", claims: with(func(c *dpoptest.Claims) { c.IAT = testNow.Add(-4 * time.Minute) })},
		{name: "iat 4 minutes ahead", claims: with(func(c *dpoptest.Claims) { c.IAT = testNow.Add(4 * time.Minute) })},
		{name: "a nonce, ignored", claims: with(func(c *dpoptest.Claims) { c.Extra = map[string]any{"nonce": "n"} })},
		{name: "a login: no token, no ath", claims: with(func(c *dpoptest.Claims) { c.Token = "" }), token: "-"},
		{name: "a login: an ath, ignored", claims: valid, token: "-"},
		{name: "no server.externalURL: the request's host", claims: with(func(c *dpoptest.Claims) {
			c.HTU = "http://example.com/api/v1/repos"
		}), url: "-"},

		{name: "no DPoP header", proofs: []string{}, why: "the request has no DPoP header"},
		{name: "two DPoP headers", proofs: []string{key.Proof(t, valid), key.Proof(t, valid)}, why: "more than one DPoP header"},
		{name: "a proof too long", proof: strings.Repeat("a", dpop.MaxProofLen+1), why: "longer than 4096 bytes"},
		{name: "not a JWT", proof: "not.a.jwt", why: "the proof is not a JWT"},
		{name: "a JWT of another typ", claims: with(func(c *dpoptest.Claims) { c.Header = map[string]any{"typ": "JWT"} }),
			why: "typ is not dpop+jwt"},
		{name: "no typ", claims: with(func(c *dpoptest.Claims) { c.Header = map[string]any{"typ": nil} }), why: "typ is not dpop+jwt"},
		{name: "alg HS256", claims: with(func(c *dpoptest.Claims) { c.Header = map[string]any{"alg": "HS256"} }), why: "alg is not ES256"},
		{name: "alg none", claims: with(func(c *dpoptest.Claims) { c.Header = map[string]any{"alg": "none"} }), why: "alg is not ES256"},
		{name: "alg ES384", claims: with(func(c *dpoptest.Claims) { c.Header = map[string]any{"alg": "ES384"} }), why: "alg is not ES256"},
		{name: "no jwk", claims: with(func(c *dpoptest.Claims) { c.Header = map[string]any{"jwk": nil} }), why: "has no jwk"},
		{name: "a private key in the jwk", claims: with(func(c *dpoptest.Claims) {
			jwk := key.JWK()
			d, err := key.Private.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			jwk["d"] = base64.RawURLEncoding.EncodeToString(d)
			c.Header = map[string]any{"jwk": jwk}
		}), why: "holds a private key"},
		{name: "an RSA jwk", claims: with(func(c *dpoptest.Claims) {
			c.Header = map[string]any{"jwk": map[string]any{"kty": "RSA", "n": "AQAB", "e": "AQAB"}}
		}), why: "is not a P-256 key"},
		{name: "a jwk of P-384", claims: with(func(c *dpoptest.Claims) {
			jwk := key.JWK()
			jwk["crv"] = "P-384"
			c.Header = map[string]any{"jwk": jwk}
		}), why: "is not a P-256 key"},
		{name: "a jwk with a short x", claims: with(func(c *dpoptest.Claims) {
			jwk := key.JWK()
			jwk["x"] = "AQAB"
			c.Header = map[string]any{"jwk": jwk}
		}), why: "no x and y of 32 bytes"},
		{name: "a jwk off the curve", claims: with(func(c *dpoptest.Claims) {
			jwk := key.JWK()
			jwk["y"] = jwk["x"]
			c.Header = map[string]any{"jwk": jwk}
		}), why: "not a point of P-256"},
		{name: "signed by another key than its jwk's", claims: with(func(c *dpoptest.Claims) {
			c.Header = map[string]any{"jwk": other.JWK()}
		}), why: "signature does not verify"},
		{name: "htm of another method", claims: with(func(c *dpoptest.Claims) { c.HTM = http.MethodPost }), why: "htm is not the method"},
		{name: "htm in lower case", claims: with(func(c *dpoptest.Claims) { c.HTM = "get" }), why: "htm is not the method"},
		{name: "htu of another host", claims: with(func(c *dpoptest.Claims) { c.HTU = "https://evil.test/api/v1/repos" }),
			why: "htu is not the URI"},
		{name: "htu of another path", claims: with(func(c *dpoptest.Claims) { c.HTU = testURL + "/api/v1/users" }),
			why: "htu is not the URI"},
		{name: "htu over http", claims: with(func(c *dpoptest.Claims) { c.HTU = "http://lajstrom.test/api/v1/repos" }),
			why: "htu is not the URI"},
		{name: "htu of another port", claims: with(func(c *dpoptest.Claims) { c.HTU = "https://lajstrom.test:8443/api/v1/repos" }),
			why: "htu is not the URI"},
		{name: "htu relative", claims: with(func(c *dpoptest.Claims) { c.HTU = "/api/v1/repos" }), why: "htu is not the URI"},
		{name: "htu with credentials", claims: with(func(c *dpoptest.Claims) { c.HTU = "https://a:b@lajstrom.test/api/v1/repos" }),
			why: "htu is not the URI"},
		{name: "htu of the internal address behind the edge", claims: with(func(c *dpoptest.Claims) {
			c.HTU = "http://example.com/api/v1/repos"
		}), why: "htu is not the URI"},
		{name: "iat 6 minutes ago", claims: with(func(c *dpoptest.Claims) { c.IAT = testNow.Add(-6 * time.Minute) }),
			why: "iat is not within 5 minutes"},
		{name: "iat 6 minutes ahead", claims: with(func(c *dpoptest.Claims) { c.IAT = testNow.Add(6 * time.Minute) }),
			why: "iat is not within 5 minutes"},
		{name: "no iat", claims: with(func(c *dpoptest.Claims) { c.Extra = map[string]any{"iat": nil} }), why: "lacks one of"},
		{name: "no jti", claims: with(func(c *dpoptest.Claims) { c.Extra = map[string]any{"jti": nil} }), why: "lacks one of"},
		{name: "no htu", claims: with(func(c *dpoptest.Claims) { c.Extra = map[string]any{"htu": nil} }), why: "lacks one of"},
		{name: "a jti too long", claims: with(func(c *dpoptest.Claims) { c.JTI = strings.Repeat("j", 257) }), why: "jti is longer"},
		{name: "the ath of another token", claims: with(func(c *dpoptest.Claims) { c.Token = testToken + "x" }),
			why: "ath is not the hash of the token"},
		{name: "no ath with a token", claims: with(func(c *dpoptest.Claims) { c.Token = "" }), why: "ath is not the hash of the token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/repos?limit=1", nil)
			switch {
			case tt.proofs != nil:
				r.Header["Dpop"] = tt.proofs
			case tt.proof != "":
				r.Header.Set("DPoP", tt.proof)
			default:
				r.Header.Set("DPoP", key.Proof(t, tt.claims))
			}
			v := &dpop.Verifier{ExternalURL: testURL, Replays: newReplays(), Now: func() time.Time { return testNow }}
			if tt.url == "-" {
				v.ExternalURL = ""
			}
			token := testToken
			if tt.token == "-" {
				token = ""
			}
			p, err := v.Verify(r, token)
			if tt.why == "" {
				if err != nil {
					t.Fatalf("Verify: %v, want the proof", err)
				}
				if p.JKT != key.JKT || p.JTI == "" || !p.IssuedAt.Equal(tt.claims.IAT.Truncate(time.Second)) {
					t.Errorf("proof %+v, want the key %s and iat %v", p, key.JKT, tt.claims.IAT)
				}
				return
			}
			if !errors.Is(err, dpop.ErrInvalidProof) || !strings.Contains(dpop.Why(err), tt.why) {
				t.Fatalf("Verify: %v (why %q), want ErrInvalidProof for %q", err, dpop.Why(err), tt.why)
			}
		})
	}
}

// TestVerifyRFCExample checks the proof of RFC 9449 Figure 13 at its own time: its signature,
// its claims and its key's thumbprint, which Figure 9 has as the token's jkt.
func TestVerifyRFCExample(t *testing.T) {
	const proof = "eyJ0eXAiOiJkcG9wK2p3dCIsImFsZyI6IkVTMjU2IiwiandrIjp7Imt0eSI6IkVDIiwieCI6Imw4dEZyaHgtMzR0VjNoUklDUkRZOXpDa0R" +
		"scEJoRjQyVVFVZldWQVdCRnMiLCJ5IjoiOVZFNGpmX09rX282NHpiVFRsY3VOSmFqSG10NnY5VERWclUwQ2R2R1JEQSIsImNydiI6IlAtMjU2In19.eyJ" +
		"qdGkiOiJlMWozVl9iS2ljOC1MQUVCIiwiaHRtIjoiR0VUIiwiaHR1IjoiaHR0cHM6Ly9yZXNvdXJjZS5leGFtcGxlLm9yZy9wcm90ZWN0ZWRyZXNvd" +
		"XJjZSIsImlhdCI6MTU2MjI2MjYxOCwiYXRoIjoiZlVIeU8ycjJaM0RaNTNFc05yV0JiMHhXWG9hTnk1OUlpS0NBcWtzbVFFbyJ9.2oW9RP35yRqzhrt" +
		"NP86L-Ey71EOptxRimPPToA1plemAgR6pxHF8y6-yqyVnmcw6Fy1dqd-jfxSYoMxhAJpLjA"
	r := httptest.NewRequest(http.MethodGet, "/protectedresource", nil)
	r.Header.Set("DPoP", proof)
	v := &dpop.Verifier{ExternalURL: "https://resource.example.org", Replays: newReplays(),
		Now: func() time.Time { return time.Unix(1562262618, 0) }}
	p, err := v.Verify(r, "Kz~8mXK1EalYznwH-LC-1fBAo.4Ljp~zsPE_NeO.gxU")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.JKT != "0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I" || p.JTI != "e1j3V_bKic8-LAEB" {
		t.Errorf("proof %+v, want the jkt of RFC 9449 Figure 9", p)
	}
	if got := dpop.TokenHash("Kz~8mXK1EalYznwH-LC-1fBAo.4Ljp~zsPE_NeO.gxU"); got != "fUHyO2r2Z3DZ53EsNrWBb0xWXoaNy59IiKCAqksmQEo" {
		t.Errorf("TokenHash = %s, want the ath of RFC 9449 Figure 14", got)
	}
}

// TestNormalize checks the normalization of htu: RFC 3986 §6.2.2 and §6.2.3.
func TestNormalize(t *testing.T) {
	for raw, want := range map[string]string{
		"https://Lajstrom.TEST:443/API/x":        "https://lajstrom.test/API/x",
		"HTTP://h:80":                            "http://h/",
		"http://h:8080/a/b":                      "http://h:8080/a/b",
		"https://h/a%2fb%7e%41":                  "https://h/a%2Fb~A",
		"https://h/a/./b/../c/":                  "https://h/a/c/",
		"https://h/a/b/..":                       "https://h/a/",
		"https://h/../../a":                      "https://h/a",
		"https://h/%2E%2E/a":                     "https://h/a",
		"https://[::1]:443/x?q=1#f":              "https://[::1]/x",
		"https://[::1]:8443/x":                   "https://[::1]:8443/x",
		"https://h/users/alice%40example.com/me": "https://h/users/alice%40example.com/me",
		"ftp://h/x":                              "",
		"/x":                                     "",
		"https://u@h/x":                          "",
		"mailto:a@h":                             "",
		"https://h/%zz":                          "",
	} {
		got, ok := dpop.Normalize(raw)
		if want == "" {
			if ok {
				t.Errorf("Normalize(%q) = %q, want refused", raw, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}

// TestThumbprint checks the thumbprint of the key of RFC 9449 Figure 4 against the jkt of
// Figure 9, and ValidThumbprint.
func TestThumbprint(t *testing.T) {
	x, _ := base64.RawURLEncoding.DecodeString("l8tFrhx-34tV3hRICRDY9zCkDlpBhF42UQUfWVAWBFs")
	y, _ := base64.RawURLEncoding.DecodeString("9VE4jf_Ok_o64zbTTlcuNJajHmt6v9TDVrU0CdvGRDA")
	if got := dpop.Thumbprint(x, y); got != "0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I" {
		t.Errorf("Thumbprint = %s", got)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := k.PublicKey.Bytes()
	for s, want := range map[string]bool{
		dpop.Thumbprint(b[1:33], b[33:]):               true,
		"0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I":  true,
		"0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4":   false,
		"0ZcOCORZNYy+DWpqq30jZyJGHTN0d2HglBV3uiguA4I":  false,
		"0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I=": false,
		"": false,
	} {
		if got := dpop.ValidThumbprint(s); got != want {
			t.Errorf("ValidThumbprint(%q) = %v", s, got)
		}
	}
}

// TestSpend checks that a proof is used once: a second Spend of the same key and jti is refused,
// of another jti or of another key with the same jti it is not; the expiry of the row is iat and
// the window; the expired rows are deleted at most once a minute; a failure of the index is not
// a refusal.
func TestSpend(t *testing.T) {
	now := testNow
	rp := newReplays()
	v := &dpop.Verifier{Replays: rp, Now: func() time.Time { return now }}
	ctx := context.Background()
	p := &dpop.Proof{JKT: "jkt", JTI: "a", IssuedAt: testNow.Add(-time.Minute)}
	if err := v.Spend(ctx, p); err != nil {
		t.Fatalf("Spend: %v", err)
	}
	if err := v.Spend(ctx, p); dpop.Why(err) != "the proof was used already" {
		t.Errorf("Spend again: %v, want used already", err)
	}
	for _, q := range []*dpop.Proof{{JKT: "jkt", JTI: "b", IssuedAt: testNow}, {JKT: "other", JTI: "a", IssuedAt: testNow}} {
		if err := v.Spend(ctx, q); err != nil {
			t.Errorf("Spend %+v: %v", q, err)
		}
	}
	if got, want := rp.rows[dpop.ReplayKey("jkt", "a")], testNow.Add(4*time.Minute); !got.Equal(want) {
		t.Errorf("the row expires at %v, want %v", got, want)
	}
	now = now.Add(30 * time.Second)
	_ = v.Spend(ctx, &dpop.Proof{JKT: "jkt", JTI: "c", IssuedAt: now})
	now = now.Add(31 * time.Second)
	_ = v.Spend(ctx, &dpop.Proof{JKT: "jkt", JTI: "d", IssuedAt: now})
	if rp.sweeps != 2 {
		t.Errorf("%d sweeps, want 2: at the first Spend and a minute after", rp.sweeps)
	}
	rp.err = errors.New("the database is down")
	if err := v.Spend(ctx, &dpop.Proof{JKT: "jkt", JTI: "e", IssuedAt: now}); err == nil || errors.Is(err, dpop.ErrInvalidProof) {
		t.Errorf("Spend with the index down: %v, want a failure that is no refusal", err)
	}
}

// replays is Replays in memory, as UseDPoPProof of the store behaves.
type replays struct {
	rows   map[string]time.Time
	sweeps int
	err    error
}

func newReplays() *replays { return &replays{rows: map[string]time.Time{}} }

func (r *replays) UseDPoPProof(_ context.Context, key string, expiresAt, now time.Time) error {
	if r.err != nil {
		return r.err
	}
	if exp, ok := r.rows[key]; ok && !exp.Before(now) {
		return fmt.Errorf("%w: a proof used", index.ErrConflict)
	}
	r.rows[key] = expiresAt
	return nil
}

func (r *replays) ExpireDPoPProofs(_ context.Context, before time.Time) (int, error) {
	r.sweeps++
	n := 0
	for k, exp := range r.rows {
		if exp.Before(before) {
			delete(r.rows, k)
			n++
		}
	}
	return n, nil
}

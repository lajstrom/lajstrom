// Package dpoptest makes DPoP proofs (RFC 9449) for the tests of other packages: a P-256 key
// made with crypto/ecdsa, and proofs signed with it as a web UI signs them.
package dpoptest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lajstrom/lajstrom/internal/auth/dpop"
)

// Key is a P-256 key of a client.
type Key struct {
	Private *ecdsa.PrivateKey
	// JKT is the JWK SHA-256 thumbprint of its public key.
	JKT string
}

// NewKey returns a new key.
func NewKey(t testing.TB) *Key {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	x, y := coordinates(&k.PublicKey)
	return &Key{Private: k, JKT: dpop.Thumbprint(x, y)}
}

// JWK returns the public key as the jwk of a proof's header.
func (k *Key) JWK() map[string]any {
	x, y := coordinates(&k.Private.PublicKey)
	return map[string]any{"kty": "EC", "crv": "P-256", "x": base64.RawURLEncoding.EncodeToString(x),
		"y": base64.RawURLEncoding.EncodeToString(y)}
}

// Claims are the claims of a proof. A zero IAT is now, an empty JTI a new random one; the ath is
// that of Token unless Token is "". Header adds to or replaces the members of the JOSE header,
// a nil value removing one; Extra adds claims, a nil value removing one.
type Claims struct {
	HTM, HTU, Token, JTI string
	IAT                  time.Time
	Header, Extra        map[string]any
}

// Proof returns a proof of c signed with k, ES256 unless c.Header says otherwise.
func (k *Key) Proof(t testing.TB, c Claims) string {
	t.Helper()
	if c.IAT.IsZero() {
		c.IAT = time.Now()
	}
	if c.JTI == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		c.JTI = base64.RawURLEncoding.EncodeToString(b)
	}
	m := jwt.MapClaims{"jti": c.JTI, "htm": c.HTM, "htu": c.HTU, "iat": c.IAT.Unix()}
	if c.Token != "" {
		m["ath"] = dpop.TokenHash(c.Token)
	}
	for name, v := range c.Extra {
		if v == nil {
			delete(m, name)
			continue
		}
		m[name] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, m)
	tok.Header["typ"] = "dpop+jwt"
	tok.Header["jwk"] = k.JWK()
	for name, v := range c.Header {
		if v == nil {
			delete(tok.Header, name)
			continue
		}
		tok.Header[name] = v
	}
	s, err := tok.SignedString(k.Private)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// coordinates returns x and y of pub, 32 bytes each.
func coordinates(pub *ecdsa.PublicKey) (x, y []byte) {
	b, err := pub.Bytes() // 0x04 ‖ x ‖ y
	if err != nil {
		panic(err) // a key of P-256 made by GenerateKey
	}
	return b[1:33], b[33:]
}

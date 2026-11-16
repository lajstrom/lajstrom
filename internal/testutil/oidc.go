package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// OIDCIssuer is an OpenID Connect issuer in the test's process, for the tests of the tokens a
// CI platform or a cluster issues (docs/spec/rbac.md §9, §11): a discovery document that names
// it, with a jwks_uri only, as GitHub Actions' and GitLab's do, the JWKS of its ES256 key, and
// the tokens it signs. The key lives only in the process. Start it with NewOIDCIssuer; it
// stops with the test.
type OIDCIssuer struct {
	// URL is the issuer, the iss of its tokens: http://127.0.0.1:<port>.
	URL string
	// Down makes the discovery document and the JWKS answer 503 while it is set.
	Down atomic.Bool

	key *ecdsa.PrivateKey
}

// oidcKid is the kid of an OIDCIssuer's key.
const oidcKid = "test-1"

// NewOIDCIssuer starts an issuer with a new key.
func NewOIDCIssuer(t testing.TB) *OIDCIssuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	o := &OIDCIssuer{key: key}
	pub, err := key.PublicKey.Bytes() // 0x04 ‖ x ‖ y
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	jwk := map[string]string{"kty": "EC", "kid": oidcKid, "crv": "P-256", "alg": "ES256", "use": "sig",
		"x": enc(pub[1:33]), "y": enc(pub[33:])}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if o.Down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": o.URL, "jwks_uri": o.URL + "/.well-known/jwks",
			"id_token_signing_alg_values_supported": []string{"ES256"}})
	})
	mux.HandleFunc("GET /.well-known/jwks", func(w http.ResponseWriter, _ *http.Request) {
		if o.Down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{jwk}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	o.URL = srv.URL
	return o
}

// Sign returns the JWT of claims signed with the issuer's key, its kid in the header; iss is the
// issuer's URL unless claims has one.
func (o *OIDCIssuer) Sign(t testing.TB, claims map[string]any) string {
	t.Helper()
	c := jwt.MapClaims{"iss": o.URL}
	for k, v := range claims {
		c[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, c)
	tok.Header["kid"] = oidcKid
	raw, err := tok.SignedString(o.key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

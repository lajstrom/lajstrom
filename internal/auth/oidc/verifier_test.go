package oidc

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// kubeIssuer is a fake service-account issuer of a Kubernetes cluster: a discovery document
// with an issuer and a jwks_uri only, as the API server serves it, and the JWKS of its keys.
type kubeIssuer struct {
	srv  *httptest.Server
	down atomic.Bool
	jwks []map[string]string
}

func newKubeIssuer(t *testing.T, published ...testKey) *kubeIssuer {
	t.Helper()
	k := &kubeIssuer{}
	for _, key := range published {
		k.jwks = append(k.jwks, key.jwk)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if k.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": k.srv.URL, "jwks_uri": k.srv.URL + "/openid/v1/jwks",
			"response_types_supported": []string{"id_token"}, "subject_types_supported": []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("GET /openid/v1/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": k.jwks})
	})
	k.srv = httptest.NewServer(mux)
	t.Cleanup(k.srv.Close)
	return k
}

// token is the claims of a service-account token of the issuer for the pod's service account
// ci/builder, with the audience aud.
func (k *kubeIssuer) token(aud string) jwt.MapClaims {
	return jwt.MapClaims{"iss": k.srv.URL, "aud": []any{aud}, "sub": "system:serviceaccount:ci:builder",
		"exp": testNow.Add(10 * time.Minute).Unix(), "iat": testNow.Unix(), "nbf": testNow.Unix(),
		"kubernetes.io": map[string]any{"namespace": "ci", "serviceaccount": map[string]any{"name": "builder", "uid": "u1"},
			"pod": map[string]any{"name": "builder-0", "uid": "p1"}}}
}

// TestVerifier: a token of the issuer for the audience verifies by the JWKS of a discovery
// document without login endpoints; each refusal of docs/spec/rbac.md §9 wraps ErrRefused, and an
// issuer that never answered is ErrUnavailable and auth.ErrIssuerUnavailable.
func TestVerifier(t *testing.T) {
	rs, es, ed := keys(t)
	k := newKubeIssuer(t, rs, es)
	v, err := NewVerifier(VerifierConfig{Issuer: k.srv.URL, Audience: "https://art.example.com", AllowHTTP: true,
		Now: func() time.Time { return testNow }, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	k.down.Store(true)
	if _, err := v.Verify(ctx, sign(t, rs, k.token("https://art.example.com"))); !errors.Is(err, ErrUnavailable) ||
		!errors.Is(err, auth.ErrIssuerUnavailable) {
		t.Errorf("an issuer that never answered: %v", err)
	}
	k.down.Store(false)
	claims, err := v.Verify(ctx, sign(t, rs, k.token("https://art.example.com")))
	if err != nil || claims["sub"] != "system:serviceaccount:ci:builder" {
		t.Fatalf("Verify = %v, %v", claims, err)
	}
	if _, err := v.Verify(ctx, sign(t, es, k.token("https://art.example.com"))); err != nil {
		t.Errorf("an ES256 token: %v", err)
	}
	with := func(key string, value any) jwt.MapClaims {
		c := k.token("https://art.example.com")
		if value == nil {
			delete(c, key)
		} else {
			c[key] = value
		}
		return c
	}
	hs := testKey{kid: "rs1", method: jwt.SigningMethodHS256}
	for _, tt := range []struct {
		name string
		raw  string
	}{
		{"another audience", sign(t, rs, k.token("https://kubernetes.default.svc"))},
		{"another issuer", sign(t, rs, with("iss", "https://other.example"))},
		{"no exp: a legacy token of a Secret", sign(t, rs, with("exp", nil))},
		{"expired", sign(t, rs, with("exp", testNow.Add(-2*time.Minute).Unix()))},
		{"not yet valid", sign(t, rs, with("nbf", testNow.Add(2*time.Minute).Unix()))},
		{"a key the JWKS does not have", sign(t, ed, k.token("https://art.example.com"))},
		{"an HMAC", func() string {
			tok := jwt.NewWithClaims(hs.method, k.token("https://art.example.com"))
			tok.Header["kid"] = hs.kid
			s, _ := tok.SignedString([]byte("secret"))
			return s
		}()},
		{"none", func() string {
			s, _ := jwt.NewWithClaims(jwt.SigningMethodNone, k.token("https://art.example.com")).SignedString(jwt.UnsafeAllowNoneSignatureType)
			return s
		}()},
		{"not a JWT", "a.b.c"},
	} {
		if _, err := v.Verify(ctx, tt.raw); !errors.Is(err, ErrRefused) || errors.Is(err, auth.ErrIssuerUnavailable) {
			t.Errorf("%s: Verify = %v, want ErrRefused", tt.name, err)
		}
	}
	for _, cfg := range []VerifierConfig{{Issuer: "", Audience: "a"}, {Issuer: "http://a.example", Audience: "a"}, {Issuer: "https://a.example"}} {
		if _, err := NewVerifier(cfg); err == nil {
			t.Errorf("NewVerifier(%+v) took it", cfg)
		}
	}
}

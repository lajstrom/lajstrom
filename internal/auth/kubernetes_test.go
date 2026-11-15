package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// fakeVerifier answers the claims of a token's payload as they are, or its error: the signature
// is the oidc package's to check.
type fakeVerifier struct{ err error }

func (f fakeVerifier) Verify(_ context.Context, raw string) (map[string]any, error) {
	if f.err != nil {
		return nil, f.err
	}
	parts := strings.Split(raw, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	return claims, json.Unmarshal(payload, &claims)
}

// jwtOf returns a JWT of claims with a header and a signature that only look like one.
func jwtOf(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","kid":"k1"}`)) + "." + enc(b) + ".c2lnbmF0dXJl"
}

// saClaims are the claims of a token of issuer for the service account ns/sa, with sub.
func saClaims(issuer, ns, sa, sub string) map[string]any {
	return map[string]any{"iss": issuer, "sub": sub,
		"kubernetes.io": map[string]any{"namespace": ns, "serviceaccount": map[string]any{"name": sa}}}
}

// TestKubernetes: a token is recognized by the issuer of its payload, and authenticates as
// k8s:<cluster>/<namespace>/<service account>; a token that does not verify, or whose sub
// disagrees with kubernetes.io, is ErrBadCredentials; an issuer that does not answer is
// ErrIssuerUnavailable.
func TestKubernetes(t *testing.T) {
	var logs bytes.Buffer
	k := &Kubernetes{Log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Clusters: []KubernetesCluster{
		{Name: "prod", Issuer: "https://prod.example", Verifier: fakeVerifier{}},
		{Name: "down", Issuer: "https://down.example", Verifier: fakeVerifier{err: ErrIssuerUnavailable}},
		{Name: "strict", Issuer: "https://strict.example", Verifier: fakeVerifier{err: errors.New("the audience")}},
	}}
	ctx := context.Background()
	good := jwtOf(t, saClaims("https://prod.example", "ci", "builder", "system:serviceaccount:ci:builder"))
	for _, s := range []string{"a password", "ljs_pat_x", "a.b.c", "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://other.example"}`)) + ".y",
		strings.TrimSuffix(good, "c2lnbmF0dXJl")} {
		if k.Recognizes(s) {
			t.Errorf("Recognizes(%q)", s)
		}
	}
	if !k.Recognizes(good) {
		t.Fatal("a token of prod is not recognized")
	}
	p, cred, err := k.Authenticate(ctx, good)
	if err != nil || p != (Principal{Kind: KindKubernetes, Name: "prod/ci/builder"}) || cred.Kind != CredKubernetes {
		t.Errorf("Authenticate = %v, %v, %v", p, cred, err)
	}
	for _, tt := range []struct {
		name  string
		token string
		want  error
	}{
		{"sub of another account", jwtOf(t, saClaims("https://prod.example", "ci", "builder", "system:serviceaccount:ci:other")), ErrBadCredentials},
		{"no kubernetes.io", jwtOf(t, map[string]any{"iss": "https://prod.example", "sub": "system:serviceaccount:ci:builder"}), ErrBadCredentials},
		{"a namespace that is no DNS label", jwtOf(t, saClaims("https://prod.example", "CI", "builder", "system:serviceaccount:CI:builder")), ErrBadCredentials},
		{"refused by the verifier", jwtOf(t, saClaims("https://strict.example", "ci", "builder", "system:serviceaccount:ci:builder")), ErrBadCredentials},
		{"an issuer that does not answer", jwtOf(t, saClaims("https://down.example", "ci", "builder", "system:serviceaccount:ci:builder")), ErrIssuerUnavailable},
		{"not a token", "a password", ErrBadCredentials},
	} {
		if p, _, err := k.Authenticate(ctx, tt.token); !errors.Is(err, tt.want) {
			t.Errorf("%s: Authenticate = %v, %v; want %v", tt.name, p, err, tt.want)
		}
	}
	if !strings.Contains(logs.String(), "cluster=strict") || strings.Contains(logs.String(), good) {
		t.Errorf("the log:\n%s", logs.String())
	}
}

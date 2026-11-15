package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// claimsVerifier answers the claims of a token's payload, or err: the server test needs no
// signature.
type claimsVerifier struct{ err error }

func (c claimsVerifier) Verify(_ context.Context, raw string) (map[string]any, error) {
	if c.err != nil {
		return nil, c.err
	}
	var claims map[string]any
	payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(raw, ".")[1])
	return claims, json.Unmarshal(payload, &claims)
}

// saToken is a token of issuer for ci/builder that only looks signed.
func saToken(issuer string) string {
	b, _ := json.Marshal(map[string]any{"iss": issuer, "sub": "system:serviceaccount:ci:builder",
		"kubernetes.io": map[string]any{"namespace": "ci", "serviceaccount": map[string]any{"name": "builder"}}})
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(b) + ".c2ln"
}

// TestKubernetesToken: on /v2/token a service-account token of a cluster is the password of
// Basic credentials or of the form, whatever the user name, and its principal is k8s:…
// (docs/spec/rbac.md §9); on any other route it is a password, refused; an issuer that does not
// answer is 503 with Retry-After.
func TestKubernetesToken(t *testing.T) {
	sw := &seen{}
	record := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*sw = seen{called: true, principal: auth.PrincipalFrom(r.Context()), credential: auth.CredentialFrom(r.Context())}
		w.WriteHeader(http.StatusNoContent)
	})
	s, logs := newTestServer(t, Options{Facade: record, API: record, Authenticator: auth.NewAuthenticator(auth.Options{Index: usersReader{}}),
		Kubernetes: &auth.Kubernetes{Clusters: []auth.KubernetesCluster{
			{Name: "prod", Issuer: "https://prod.example", Verifier: claimsVerifier{}},
			{Name: "down", Issuer: "https://down.example", Verifier: claimsVerifier{err: auth.ErrIssuerUnavailable}},
		}}})
	builder := auth.Principal{Kind: auth.KindKubernetes, Name: "prod/ci/builder"}
	token := saToken("https://prod.example")

	if rr := serveAs(s, "/v2/token?scope=repository:oci/k8s/pause:pull", basic("k8s", token)); rr.Code != http.StatusNoContent ||
		sw.principal != builder || sw.credential.Kind != auth.CredKubernetes {
		t.Errorf("GET with Basic: %d as %v with %q", rr.Code, sw.principal, sw.credential.Kind)
	}
	*sw = seen{}
	if rr := postForm(s, passwordForm("anything", token), ""); rr.Code != http.StatusNoContent || sw.principal != builder {
		t.Errorf("POST: %d as %v", rr.Code, sw.principal)
	}
	*sw = seen{}
	if rr := serveAs(s, "/api/v1/whoami", basic("k8s", token)); rr.Code != http.StatusUnauthorized || sw.called {
		t.Errorf("another route: %d, called %v", rr.Code, sw.called)
	}
	rr := serveAs(s, "/v2/token", basic("k8s", saToken("https://down.example")))
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") != "30" {
		t.Errorf("an issuer that does not answer: %d, Retry-After %q", rr.Code, rr.Header().Get("Retry-After"))
	}
	r := httptest.NewRequest(http.MethodGet, "/v2/token", nil)
	r.SetBasicAuth("k8s", saToken("https://other.example"))
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("a token of no cluster, checked as a password: %d", rr.Code)
	}
	if strings.Contains(logs.String(), token) {
		t.Errorf("the log has a token:\n%s", logs)
	}
}

package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/dpop/dpoptest"
	"github.com/lajstrom/lajstrom/internal/server"
)

// dpopReplica returns the handler of a server with the management API over the index of f, an
// authenticator and a DPoP verifier of its own, at testNow: one replica of several, which share
// the index and nothing else.
func dpopReplica(t *testing.T, f *fixture) http.Handler {
	t.Helper()
	authn := auth.NewAuthenticator(auth.Options{Index: f.st, Now: func() time.Time { return testNow }})
	v := loginVerifier(f.st)
	srv, err := server.New(server.Options{Listen: "127.0.0.1:0", ExternalURL: loginURL, Log: f.repos.Log, Authenticator: authn, DPoP: v,
		API: recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log, Passwords: authn, DPoP: v}))})
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler()
}

// send sends method target with the headers to h; a header with several values is sent so.
func send(h http.Handler, method, target, body string, header map[string][]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, vs := range header {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// TestDPoPThroughServer checks the token of a sign-in behind the server's middleware
// (docs/spec/http-routing.md §4, RFC 9449), on two replicas over one index: the token works on
// /api/ as Authorization: DPoP with a proof of its key, each proof once on either replica; every
// refused proof is 401 with the challenge invalid_dpop_proof and why, a refused token 401 with
// invalid_token; the bound token is refused as a bearer and in Basic credentials, on /api/ with
// the DPoP challenge, on /v2, /v2/token and a frontend with theirs; the DPoP scheme is refused
// outside /api/; sign-out revokes the token.
func TestDPoPThroughServer(t *testing.T) {
	f := loginFixture(t, Options{})
	one, two := dpopReplica(t, f), dpopReplica(t, f)
	key, other := dpoptest.NewKey(t), dpoptest.NewKey(t)
	rr := send(one, http.MethodPost, "/api/v1/login", fmt.Sprintf(`{"username": "alice", "password": %q}`, loginPassword),
		map[string][]string{"DPoP": {loginProof(t, key)}})
	var c signedIn
	decodeJSON(t, rr, http.StatusCreated, &c)
	secret := c.Secret
	whoami := loginURL + "/api/v1/whoami"
	proof := func(k *dpoptest.Key, method, htu, token string, iat time.Time) string {
		return k.Proof(t, dpoptest.Claims{HTM: method, HTU: htu, Token: token, IAT: iat})
	}
	dpopAuth := map[string][]string{"Authorization": {"DPoP " + secret}}
	with := func(proofs ...string) map[string][]string {
		return map[string][]string{"Authorization": dpopAuth["Authorization"], "DPoP": proofs}
	}

	// A proof works once, on either replica.
	good := proof(key, http.MethodGet, whoami, secret, testNow)
	rr = send(one, http.MethodGet, "/api/v1/whoami", "", with(good))
	wantStatus(t, rr, http.StatusOK)
	if !strings.Contains(rr.Body.String(), `"name":"alice"`) {
		t.Errorf("whoami %s, want alice", rr.Body)
	}
	want401(t, "the proof again on the other replica", send(two, http.MethodGet, "/api/v1/whoami", "", with(good)),
		`DPoP error="invalid_dpop_proof", error_description="the proof was used already", algs="ES256"`)
	wantStatus(t, send(two, http.MethodGet, "/api/v1/whoami", "", with(proof(key, http.MethodGet, whoami, secret, testNow))), http.StatusOK)

	// A personal access token bound to no key.
	ctx := auth.WithCredential(context.Background(), auth.Credential{Kind: auth.CredPassword})
	_, plain, err := (&auth.Tokens{Index: f.st, Now: func() time.Time { return testNow }}).
		CreatePAT(ctx, "alice", "laptop", nil, auth.Expiry{In: time.Hour}, auth.Origin{Source: SourceCLI})
	if err != nil {
		t.Fatal(err)
	}
	bound := `DPoP error="invalid_token", error_description="the token is bound to a DPoP key: send it as Authorization: DPoP with a proof", algs="ES256"`
	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:"+secret))
	for _, tc := range []struct {
		name, method, path string
		header             map[string][]string
		challenge          string // the WWW-Authenticate of the 401
	}{
		{"no proof", http.MethodGet, "/api/v1/whoami", dpopAuth,
			`DPoP error="invalid_dpop_proof", error_description="the request has no DPoP header", algs="ES256"`},
		{"two proofs", http.MethodGet, "/api/v1/whoami", with(proof(key, http.MethodGet, whoami, secret, testNow),
			proof(key, http.MethodGet, whoami, secret, testNow)), `error_description="the request has more than one DPoP header"`},
		{"htm of another method", http.MethodGet, "/api/v1/whoami", with(proof(key, http.MethodPost, whoami, secret, testNow)),
			`error_description="the proof's htm is not the method of the request"`},
		{"htu of another path", http.MethodGet, "/api/v1/whoami", with(proof(key, http.MethodGet, loginURL+"/api/v1/info", secret, testNow)),
			`error_description="the proof's htu is not the URI of the request"`},
		{"htu of another host", http.MethodGet, "/api/v1/whoami", with(proof(key, http.MethodGet, "https://evil.test/api/v1/whoami", secret, testNow)),
			`error_description="the proof's htu is not the URI of the request"`},
		{"iat stale", http.MethodGet, "/api/v1/whoami", with(proof(key, http.MethodGet, whoami, secret, testNow.Add(-6*time.Minute))),
			`error_description="the proof's iat is not within 5 minutes of the server's clock"`},
		{"the ath of another token", http.MethodGet, "/api/v1/whoami", with(proof(key, http.MethodGet, whoami, plain, testNow)),
			`error_description="the proof's ath is not the hash of the token"`},
		{"a proof of another key", http.MethodGet, "/api/v1/whoami", with(proof(other, http.MethodGet, whoami, secret, testNow)),
			`DPoP error="invalid_token", algs="ES256"`},
		{"a token bound to no key", http.MethodGet, "/api/v1/whoami", map[string][]string{"Authorization": {"DPoP " + plain},
			"DPoP": {proof(key, http.MethodGet, whoami, plain, testNow)}}, `DPoP error="invalid_token", algs="ES256"`},
		{"the bound token as a bearer", http.MethodGet, "/api/v1/whoami", map[string][]string{"Authorization": {"Bearer " + secret}}, bound},
		{"the bound token as a bearer with a proof", http.MethodGet, "/api/v1/whoami", map[string][]string{"Authorization": {"Bearer " + secret},
			"DPoP": {proof(key, http.MethodGet, whoami, secret, testNow)}}, bound},
		{"the bound token in Basic credentials", http.MethodDelete, "/api/v1/users/alice/tokens/1", map[string][]string{"Authorization": {basicAuth}}, bound},
		{"the bound token on /v2/token", http.MethodGet, "/v2/token?service=lajstrom.test", map[string][]string{"Authorization": {basicAuth}},
			`Basic realm="lajstrom"`},
		{"the bound token on /v2", http.MethodGet, "/v2/", map[string][]string{"Authorization": {basicAuth}}, `Bearer realm=`},
		{"the bound token on a frontend", http.MethodGet, "/raw/assets/x", map[string][]string{"Authorization": {"Bearer " + secret}},
			`Basic realm="lajstrom"`},
		{"npm's _authToken", http.MethodGet, "/npm/internal/left-pad", map[string][]string{"Authorization": {"Bearer " + secret}},
			`Basic realm="lajstrom"`},
		{"DPoP on /v2", http.MethodGet, "/v2/", map[string][]string{"Authorization": {"DPoP " + secret},
			"DPoP": {proof(key, http.MethodGet, loginURL+"/v2/", secret, testNow)}}, `Bearer realm=`},
		{"DPoP on a frontend", http.MethodGet, "/raw/assets/x", map[string][]string{"Authorization": {"DPoP " + secret},
			"DPoP": {proof(key, http.MethodGet, loginURL+"/raw/assets/x", secret, testNow)}}, `Basic realm="lajstrom"`},
	} {
		want401(t, tc.name, send(one, tc.method, tc.path, "", tc.header), tc.challenge)
	}

	// Sign-out revokes the token, on every replica.
	target := fmt.Sprintf("/api/v1/users/alice/tokens/%d", c.ID)
	wantStatus(t, send(two, http.MethodDelete, target, "", with(proof(key, http.MethodDelete, loginURL+target, secret, testNow))),
		http.StatusNoContent)
	want401(t, "the revoked token", send(one, http.MethodGet, "/api/v1/whoami", "", with(proof(key, http.MethodGet, whoami, secret, testNow))),
		`DPoP error="invalid_token", algs="ES256"`)
	if strings.Contains(f.logs.String(), secret) {
		t.Error("the log holds the secret")
	}
}

// want401 checks a 401 with a WWW-Authenticate that contains challenge.
func want401(t *testing.T, name string, rr *httptest.ResponseRecorder, challenge string) {
	t.Helper()
	if got := rr.Header().Get("WWW-Authenticate"); rr.Code != http.StatusUnauthorized || !strings.Contains(got, challenge) {
		t.Errorf("%s: %d with WWW-Authenticate %q; want 401 with %s", name, rr.Code, got, challenge)
	}
}

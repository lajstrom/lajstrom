package token

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/keys"
)

var ciJob = auth.Principal{Kind: auth.KindCI, Name: "github/acme/app"}

// TestIssueVerifyCI: a CI token is the prefix and a JWT of the token key, typ ljs-ci+jwt, aud
// lajstrom:ci, the lifetime asked for, its policies; it verifies as a CI token and never as a
// bearer of /v2, and a bearer never verifies as a CI token (docs/spec/rbac.md §11).
func TestIssueVerifyCI(t *testing.T) {
	for _, alg := range []string{keys.TokenES256, keys.TokenEdDSA} {
		t.Run(alg, func(t *testing.T) {
			k := newKey(t, alg)
			issuer, verifier, vc, _ := pair(&fakeKeys{signer: k, verifiers: []*keys.TokenKey{k}})
			tok, err := issuer.IssueCI(t.Context(), ciJob, service, []string{"app-release", "acme-read"}, 15*time.Minute)
			must(t, err)
			raw, ok := strings.CutPrefix(tok.Raw, auth.CIPrefix)
			if !ok || !tok.ExpiresAt.Equal(t0.Add(15*time.Minute)) || tok.ID == "" || tok.KeyID != k.ID() {
				t.Fatalf("token %s, expires %v, id %q", tok, tok.ExpiresAt, tok.ID)
			}
			checkCIToken(t, raw, tok, k)
			g, err := verifier.VerifyCI(t.Context(), tok.Raw)
			must(t, err)
			if g.Principal != ciJob || g.ID != tok.ID || !slices.Equal(g.Policies, []string{"app-release", "acme-read"}) ||
				!g.ExpiresAt.Equal(tok.ExpiresAt) {
				t.Errorf("grant %+v", g)
			}
			if _, err := verifier.Verify(t.Context(), raw, service); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("a CI token as a bearer: %v", err)
			}
			if _, err := verifier.Verify(t.Context(), raw, CIAudience); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("a CI token as a bearer of the service %s: %v", CIAudience, err)
			}
			bearer, err := issuer.Issue(t.Context(), ciJob, service, nil)
			must(t, err)
			if _, err := verifier.VerifyCI(t.Context(), auth.CIPrefix+bearer.Raw); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("a bearer as a CI token: %v", err)
			}
			vc.set(t0.Add(15*time.Minute + Leeway + time.Second))
			if _, err := verifier.VerifyCI(t.Context(), tok.Raw); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("an expired CI token: %v", err)
			}
		})
	}
}

// checkCIToken checks the JWT raw of the CI token tok of TestIssueVerifyCI, signed with k.
func checkCIToken(t *testing.T, raw string, tok *Token, k *keys.TokenKey) {
	t.Helper()
	header, payload := segments(t, raw)
	if header["typ"] != CIType || header["kid"] != k.ID() || header["alg"] != k.Algorithm {
		t.Errorf("header %v", header)
	}
	if !slices.Equal(toStrings(payload["aud"]), []string{CIAudience}) || payload["iss"] != service ||
		payload["sub"] != "ci:github/acme/app" || payload["jti"] != tok.ID ||
		!slices.Equal(toStrings(payload["policies"]), []string{"app-release", "acme-read"}) {
		t.Errorf("payload %v", payload)
	}
}

// TestVerifyCIRefusals: no prefix, a JWT without its typ or of another key, another audience, no
// policy, a subject that is no ci: principal; and IssueCI only for a ci: principal with policies.
func TestVerifyCIRefusals(t *testing.T) {
	a, b := newKey(t, keys.TokenES256), newKey(t, keys.TokenES256)
	issuer, verifier, _, logged := pair(&fakeKeys{signer: a, verifiers: []*keys.TokenKey{a}})
	good, err := issuer.IssueCI(t.Context(), ciJob, service, []string{"p"}, time.Minute)
	must(t, err)
	ciSign := func(key *keys.TokenKey, typ string, c ciClaims) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodES256, c)
		tok.Header["kid"] = key.ID()
		if typ != "" {
			tok.Header["typ"] = typ
		}
		raw, err := tok.SignedString(key.Private)
		must(t, err)
		return auth.CIPrefix + raw
	}
	valid := func(sub string, aud string, policies ...string) ciClaims {
		return ciClaims{RegisteredClaims: jwt.RegisteredClaims{Subject: sub, Audience: jwt.ClaimStrings{aud}, ID: "x",
			IssuedAt: jwt.NewNumericDate(t0), ExpiresAt: jwt.NewNumericDate(t0.Add(time.Minute))}, Policies: policies}
	}
	for name, secret := range map[string]string{
		"no prefix":        strings.TrimPrefix(good.Raw, auth.CIPrefix),
		"the JWT's typ":    ciSign(a, "JWT", valid("ci:github/acme/app", CIAudience, "p")),
		"no typ":           ciSign(a, "", valid("ci:github/acme/app", CIAudience, "p")),
		"another key":      ciSign(b, CIType, valid("ci:github/acme/app", CIAudience, "p")),
		"another audience": ciSign(a, CIType, valid("ci:github/acme/app", service, "p")),
		"no policy":        ciSign(a, CIType, valid("ci:github/acme/app", CIAudience)),
		"a user":           ciSign(a, CIType, valid(alice.String(), CIAudience, "p")),
		"a signature":      good.Raw[:len(good.Raw)-4] + "AAAA",
	} {
		if _, err := verifier.VerifyCI(t.Context(), secret); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: VerifyCI = %v", name, err)
		}
	}
	if strings.Contains(logged.String(), strings.TrimPrefix(good.Raw, auth.CIPrefix)[:40]) {
		t.Errorf("the log has the token:\n%s", logged.String())
	}
	if _, err := issuer.IssueCI(t.Context(), alice, service, []string{"p"}, time.Minute); err == nil {
		t.Error("IssueCI for a user")
	}
	if _, err := issuer.IssueCI(t.Context(), ciJob, service, nil, time.Minute); err == nil {
		t.Error("IssueCI without a policy")
	}
}

func toStrings(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

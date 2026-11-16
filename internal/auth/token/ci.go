package token

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// The CI token of docs/spec/rbac.md §11: auth.CIPrefix and a JWT signed with the token key, for a
// ci: principal and the trust policies that matched its exchange. It is a credential, never a
// bearer of /v2: its audience is CIAudience, which is no service, and its typ is CIType.
const (
	// CIAudience is the aud of a CI token: a host never has a colon without a port.
	CIAudience = "lajstrom:ci"
	// CIType is the typ of a CI token's header.
	CIType = "ljs-ci+jwt"
)

// ciClaims are the claims of a CI token.
type ciClaims struct {
	jwt.RegisteredClaims
	// Policies are the names of the trust policies that matched the exchange.
	Policies []string `json:"policies"`
}

// CIGrant is what a valid CI token says.
type CIGrant struct {
	// Principal is the ci: principal of sub.
	Principal auth.Principal
	// Policies are the names of the trust policies.
	Policies []string
	// ID is the jti, which names the token.exchange row of its exchange.
	ID string
	// ExpiresAt is exp.
	ExpiresAt time.Time
}

// IssueCI returns the CI token of p, a ci: principal, for the policies, which lives lifetime:
// header typ CIType; claims iss service, aud CIAudience, sub p, iat, nbf, exp, jti and policies.
// Its Raw is the secret, auth.CIPrefix and the JWT.
func (i *Issuer) IssueCI(ctx context.Context, p auth.Principal, service string, policies []string, lifetime time.Duration) (*Token, error) {
	if p.Kind != auth.KindCI || len(policies) == 0 {
		return nil, errors.New("token: a CI token is for a ci: principal and one or more policies")
	}
	t, err := i.sign(ctx, CIType, lifetime, func(rc jwt.RegisteredClaims) jwt.Claims {
		rc.Issuer, rc.Subject, rc.Audience = service, p.String(), jwt.ClaimStrings{CIAudience}
		return ciClaims{RegisteredClaims: rc, Policies: policies}
	})
	if err != nil {
		return nil, err
	}
	t.Raw = auth.CIPrefix + t.Raw
	return t, nil
}

// VerifyCI checks secret, a CI token, and returns what it says: the prefix, the key of the kid of
// the header and that key's algorithm only, typ CIType, aud CIAudience, exp required, iat not in
// the future, with Leeway on the times; sub a ci: principal, a jti and one or more policies. Its
// iss is not read: every replica's service may differ, and the key is the check. Every refusal is
// an error wrapping ErrInvalidToken, its reason logged at debug level; any other error is a
// failure to load the keys.
func (v *Verifier) VerifyCI(ctx context.Context, secret string) (*CIGrant, error) {
	raw, ok := strings.CutPrefix(secret, auth.CIPrefix)
	if !ok {
		return nil, v.refuse(ctx, string(auth.CredCI), "", "not a CI token", false)
	}
	kid, err := kidOf(raw)
	if err != nil {
		return nil, v.refuse(ctx, string(auth.CredCI), "", err.Error(), false)
	}
	k, err := v.key(ctx, kid)
	if err != nil {
		return nil, err
	}
	if k == nil {
		return nil, v.refuse(ctx, string(auth.CredCI), kid, "no key has this kid", false)
	}
	var c ciClaims
	_, err = jwt.ParseWithClaims(raw, &c, func(t *jwt.Token) (any, error) {
		if typ, _ := t.Header["typ"].(string); typ != CIType {
			return nil, errors.New("the header's typ is not a CI token's")
		}
		return k.Public, nil
	},
		jwt.WithValidMethods([]string{k.Algorithm}), jwt.WithAudience(CIAudience),
		jwt.WithLeeway(Leeway), jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
		jwt.WithTimeFunc(v.now))
	if err != nil {
		return nil, v.refuse(ctx, string(auth.CredCI), kid, err.Error(), errors.Is(err, jwt.ErrTokenExpired))
	}
	p, err := auth.ParsePrincipal(c.Subject)
	switch {
	case err != nil || p.Kind != auth.KindCI:
		return nil, v.refuse(ctx, string(auth.CredCI), kid, "the subject is not a ci: principal", false)
	case c.ID == "" || len(c.Policies) == 0:
		return nil, v.refuse(ctx, string(auth.CredCI), kid, "no jti or no policy", false)
	}
	return &CIGrant{Principal: p, Policies: c.Policies, ID: c.ID, ExpiresAt: c.ExpiresAt.Time}, nil
}

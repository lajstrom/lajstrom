package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// algorithms are the JWS algorithms an ID token may be signed with: asymmetric only, never
// none nor an HMAC, whose key the client secret would be (docs/spec/rbac.md §8).
var algorithms = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}

// maxSubject bounds sub (OpenID Connect Core §2: at most 255 ASCII characters).
const maxSubject = 255

// verify checks the ID token raw (docs/spec/rbac.md §8) and returns its identity: the signature
// by the key of its kid with an algorithm that is the key's, iss, aud, azp, exp, iat, nbf with
// Leeway, and nonce. Every failure wraps ErrRefused, but a JWKS that never loaded, which wraps
// ErrUnavailable.
func (c *Client) verify(ctx context.Context, raw, nonce string) (*Identity, error) {
	m, err := c.disc.get(ctx)
	if err != nil {
		return nil, err
	}
	var keyErr error
	claims := jwt.MapClaims{}
	_, err = jwt.NewParser(
		jwt.WithValidMethods(algorithms),
		jwt.WithIssuer(c.cfg.Issuer),
		jwt.WithAudience(c.cfg.ClientID),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(Leeway),
		jwt.WithTimeFunc(c.cfg.Now),
	).ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		k, err := c.keys.key(ctx, m.JWKSURI, kid)
		if err != nil {
			keyErr = err
			return nil, err
		}
		alg := t.Method.Alg()
		if k.alg != "" && k.alg != alg {
			return nil, fmt.Errorf("the key %q is for %s, the token is %s", kid, k.alg, alg)
		}
		if !fits(alg, k.pub) {
			return nil, fmt.Errorf("the key %q is not a key of %s", kid, alg)
		}
		return k.pub, nil
	})
	switch {
	case errors.Is(keyErr, ErrUnavailable):
		return nil, keyErr // the JWKS never loaded
	case err != nil:
		return nil, fmt.Errorf("%w: the ID token: %w", ErrRefused, err)
	}
	return c.identity(ctx, claims, nonce)
}

// identity checks the claims that the parser does not (azp, nonce, sub) and reads the identity.
func (c *Client) identity(ctx context.Context, claims jwt.MapClaims, nonce string) (*Identity, error) {
	if azp, ok := claims["azp"]; ok && azp != c.cfg.ClientID {
		return nil, fmt.Errorf("%w: the ID token's azp is not the client", ErrRefused)
	}
	got, _ := claims["nonce"].(string)
	if nonce == "" || subtle.ConstantTimeCompare([]byte(got), []byte(nonce)) != 1 {
		return nil, fmt.Errorf("%w: the ID token's nonce is not the login's", ErrRefused)
	}
	sub, _ := claims["sub"].(string)
	if sub == "" || len(sub) > maxSubject {
		return nil, fmt.Errorf("%w: the ID token's sub is empty or longer than %d", ErrRefused, maxSubject)
	}
	id := &Identity{Issuer: c.cfg.Issuer, Subject: sub}
	if c.cfg.UsernameClaim != "" {
		id.Username, _ = claims[c.cfg.UsernameClaim].(string)
	}
	if c.cfg.GroupsClaim != "" {
		var dropped int
		if id.Groups, dropped = c.groups(claims[c.cfg.GroupsClaim]); dropped > 0 {
			c.cfg.Log.DebugContext(ctx, "oidc: values of the groups claim that are no group's name are dropped",
				"subject", sub, "claim", c.cfg.GroupsClaim, "dropped", dropped)
		}
	} else {
		id.Groups = []string{}
	}
	return id, nil
}

// groups returns the group names of the claim v, a list of strings or one string, in order and
// without repeats, and how many values it dropped: not strings, or names KeepGroup refuses.
func (c *Client) groups(v any) ([]string, int) {
	var values []any
	switch g := v.(type) {
	case nil:
		return []string{}, 0
	case string:
		values = []any{g}
	case []any:
		values = g
	default:
		return []string{}, 1
	}
	out, dropped := []string{}, 0
	for _, v := range values {
		s, ok := v.(string)
		switch {
		case !ok, c.cfg.KeepGroup != nil && c.cfg.KeepGroup(s) != nil:
			dropped++
		case !slices.Contains(out, s):
			out = append(out, s)
		}
	}
	return out, dropped
}

// fits reports whether pub is a key of the algorithm alg: RS* and PS* an RSA key, ES* an EC key
// on the curve of its hash size, EdDSA an Ed25519 key.
func fits(alg string, pub any) bool {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return strings.HasPrefix(alg, "RS") || strings.HasPrefix(alg, "PS")
	case *ecdsa.PublicKey:
		curve := map[string]elliptic.Curve{"ES256": elliptic.P256(), "ES384": elliptic.P384(), "ES512": elliptic.P521()}[alg]
		return curve != nil && k.Curve == curve
	case ed25519.PublicKey:
		return alg == "EdDSA"
	}
	return false
}

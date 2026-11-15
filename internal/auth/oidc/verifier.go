package oidc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// VerifierConfig configures a Verifier: an issuer whose JWTs are credentials, and the audience
// they must have.
type VerifierConfig struct {
	// Issuer is the exact iss of the issuer's tokens; https, or http with AllowHTTP.
	Issuer string
	// Audience is what a token's aud must contain.
	Audience string
	// AllowHTTP accepts an issuer and a jwks_uri on http: server.insecureHTTP, the dev stack.
	AllowHTTP bool
	// HTTPClient reads the discovery document and the JWKS; nil is a client with DefaultTimeout
	// that follows no redirect.
	HTTPClient *http.Client
	// Now is the clock of the verification and of the caches; nil is time.Now.
	Now func() time.Time
	// Log gets the reloads of the keys and of the discovery document at debug level; nil is
	// slog.Default().
	Log *slog.Logger
}

// Verifier verifies the JWTs of an issuer for an audience, by its discovery document and its
// JWKS, cached as the login's: the service-account tokens of a Kubernetes cluster
// (docs/spec/rbac.md §9). The discovery document needs a jwks_uri only. Create it with
// NewVerifier; its methods may be called concurrently.
type Verifier struct {
	c        *Client
	audience string
}

// NewVerifier checks cfg and returns a Verifier. It reads nothing from the issuer: the
// discovery document is read at the first token.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if err := checkURL("issuer", cfg.Issuer, cfg.AllowHTTP); err != nil {
		return nil, err
	}
	if cfg.Audience == "" {
		return nil, errors.New("oidc: the audience must not be empty")
	}
	c := &Client{cfg: Config{Issuer: cfg.Issuer, AllowHTTP: cfg.AllowHTTP, HTTPClient: cfg.HTTPClient, Now: cfg.Now, Log: cfg.Log},
		keysOnly: true}
	if c.cfg.HTTPClient == nil {
		c.cfg.HTTPClient = &http.Client{
			Timeout:       DefaultTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if c.cfg.Now == nil {
		c.cfg.Now = time.Now
	}
	if c.cfg.Log == nil {
		c.cfg.Log = slog.Default()
	}
	c.disc = &discovery{c: c}
	c.keys = &keySet{c: c}
	return &Verifier{c: c, audience: cfg.Audience}, nil
}

// Verify checks the JWT raw and returns its claims: the signature by the key of its kid in the
// issuer's JWKS, with an asymmetric algorithm that is the key's, iss, aud containing the
// audience, exp required, iat and nbf with Leeway. Every failure wraps ErrRefused, but an issuer
// whose discovery document or keys never loaded, which wraps ErrUnavailable and
// auth.ErrIssuerUnavailable.
func (v *Verifier) Verify(ctx context.Context, raw string) (map[string]any, error) {
	claims, err := v.c.parse(ctx, raw, v.audience)
	if errors.Is(err, ErrUnavailable) {
		return nil, fmt.Errorf("%w: %w", auth.ErrIssuerUnavailable, err)
	}
	return claims, err
}

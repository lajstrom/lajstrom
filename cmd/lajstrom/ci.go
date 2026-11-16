package main

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/ci"
	"github.com/lajstrom/lajstrom/internal/auth/oidc"
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// ciExchange returns the exchange of POST /-/oidc/token and the authenticator of the CI tokens
// of auth.ci (docs/spec/rbac.md §11), both nil when it has no issuer. Each issuer is read through
// a client that trusts its caFile, or the system's roots, and nothing is read at start; the CI
// tokens are signed and verified with keys, the token keys of the bearers. It logs each issuer
// and each policy with its claims and grants: the configuration is the record of the policies.
func ciExchange(c *config.Config, st index.Store, keys token.Keys, log *slog.Logger) (http.Handler, server.CIAuthenticator, error) {
	cc := c.Auth.CI
	if len(cc.Issuers) == 0 {
		return nil, nil, nil
	}
	policies, err := ci.New(cc)
	if err != nil {
		return nil, nil, fmt.Errorf("auth.ci: %w", err)
	}
	verifiers := map[string]auth.TokenVerifier{}
	for i, is := range cc.Issuers {
		key := fmt.Sprintf("auth.ci.issuers[%d]", i)
		client, err := issuerClient(is.CAFile)
		if err != nil {
			return nil, nil, fmt.Errorf("%s.caFile: %w", key, err)
		}
		audience := is.AudienceOr(c.Server.ExternalURL)
		v, err := oidc.NewVerifier(oidc.VerifierConfig{Issuer: is.Issuer, Audience: audience, AllowHTTP: c.Server.InsecureHTTP,
			HTTPClient: client, Log: log})
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", key, err)
		}
		verifiers[is.Name] = v
		log.Info("CI tokens of an issuer are exchanged at POST /-/oidc/token", "issuer", is.Name, "platform", is.Platform,
			"url", is.Issuer, "audience", audience)
	}
	for _, p := range cc.Policies {
		log.Info("CI trust policy", "policy", p.Name, "issuer", p.Issuer, "claims", p.Claims, "grants", p.Grants)
	}
	verifier := token.NewVerifier(token.Options{Keys: keys, Log: log})
	exchange := &ci.Exchange{Policies: policies, Verifiers: verifiers, Index: st, Lifetime: cc.TokenLifetime,
		Tokens: token.NewIssuer(token.Options{Keys: keys, Verifier: verifier, Log: log}), ExternalURL: c.Server.ExternalURL, Log: log}
	return exchange, &ci.Tokens{Policies: policies, Verifier: verifier, Log: log}, nil
}

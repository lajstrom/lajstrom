package main

import (
	"fmt"
	"log/slog"

	"github.com/lajstrom/lajstrom/internal/auth/clientcert"
	"github.com/lajstrom/lajstrom/internal/config"
)

// clientCertificates returns the verifier of auth.clientCertificates (docs/spec/rbac.md §9), its
// bundles read from their CA files, and nil when there is no bundle. It says at start where the
// certificates come from, and warns when they can come from nowhere: no server.tls and no
// forwarded header.
func clientCertificates(c *config.Config, log *slog.Logger) (*clientcert.Verifier, error) {
	cc := c.Auth.ClientCertificates
	if len(cc.Bundles) == 0 {
		return nil, nil //nolint:nilnil // no bundle: no certificate is read
	}
	bundles := make([]*clientcert.Bundle, len(cc.Bundles))
	names := make([]string, len(cc.Bundles))
	for i, b := range cc.Bundles {
		bundle, err := clientcert.LoadBundle(b)
		if err != nil {
			return nil, fmt.Errorf("auth.clientCertificates.bundles[%d]: %w", i, err)
		}
		bundles[i], names[i] = bundle, b.Name
	}
	tls := c.Server.TLS.CertFile != ""
	if !tls && cc.ForwardedHeader == "" {
		log.Warn("auth.clientCertificates has bundles, but no certificate can arrive: set server.tls, " +
			"or auth.clientCertificates.forwardedHeader behind an edge")
	}
	log.Info("client certificates are a credential of GET /v2/token", "bundles", names, "tls", tls,
		"forwardedHeader", cc.ForwardedHeader)
	return clientcert.NewVerifier(bundles, nil), nil
}

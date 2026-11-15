package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/clientcert"
	"github.com/lajstrom/lajstrom/internal/auth/oidc"
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

// kubernetesClusters returns the authenticator of the service-account tokens of auth.kubernetes
// (docs/spec/rbac.md §9), and nil when there is no cluster. Each cluster's issuer is read through
// a client that trusts its caFile, or the system's roots; nothing is read at start.
func kubernetesClusters(c *config.Config, log *slog.Logger) (*auth.Kubernetes, error) {
	if len(c.Auth.Kubernetes.Clusters) == 0 {
		return nil, nil //nolint:nilnil // no cluster: no token is taken
	}
	k := &auth.Kubernetes{Log: log}
	for i, cl := range c.Auth.Kubernetes.Clusters {
		key := fmt.Sprintf("auth.kubernetes.clusters[%d]", i)
		client, err := issuerClient(cl.CAFile)
		if err != nil {
			return nil, fmt.Errorf("%s.caFile: %w", key, err)
		}
		audience := cl.AudienceOr(c.Server.ExternalURL)
		v, err := oidc.NewVerifier(oidc.VerifierConfig{Issuer: cl.Issuer, Audience: audience, AllowHTTP: c.Server.InsecureHTTP,
			HTTPClient: client, Log: log})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		k.Clusters = append(k.Clusters, auth.KubernetesCluster{Name: cl.Name, Issuer: cl.Issuer, Verifier: v})
		log.Info("service-account tokens of a Kubernetes cluster are a credential of /v2/token", "cluster", cl.Name,
			"issuer", cl.Issuer, "audience", audience)
	}
	return k, nil
}

// issuerClient returns the client that reads an issuer's discovery document and JWKS: the
// timeout of the OIDC login, no redirect, and the CAs of caFile when it is set.
func issuerClient(caFile string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		data, err := os.ReadFile(caFile) //nolint:gosec // the operator's configuration names the file
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return nil, errors.New("no PEM certificate in the file")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Timeout: oidc.DefaultTimeout, Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

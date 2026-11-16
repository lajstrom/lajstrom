package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/clientcert"
	"github.com/lajstrom/lajstrom/internal/auth/oidc"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// maxClientSecret bounds the file of auth.oidc.clientSecretFile.
const maxClientSecret = 4 << 10

// edge is what the server and the management API need of the configuration beside the
// services.
type edge struct {
	// login is the handler of /-/oidc/, handoffs the redeemer of the codes it hands a web UI;
	// both nil while auth.oidc is off.
	login    http.Handler
	handoffs *oidc.Handoffs
	// proxies are server.trustedProxies.
	proxies []netip.Prefix
	// certs is the verifier of auth.clientCertificates (clientCertificates), nil without bundles.
	certs *clientcert.Verifier
	// kubernetes authenticates the tokens of auth.kubernetes (kubernetesClusters), nil without
	// clusters.
	kubernetes *auth.Kubernetes
}

// edgeInputs returns the edge of the configuration c.
func edgeInputs(c *config.Config, st index.Store, log *slog.Logger) (edge, error) {
	login, handoffs, err := oidcLogin(c, st, log)
	if err != nil {
		return edge{}, err
	}
	proxies, err := c.Server.TrustedPrefixes()
	if err != nil {
		return edge{}, err
	}
	certs, err := clientCertificates(c, log)
	if err != nil {
		return edge{}, err
	}
	kubernetes, err := kubernetesClusters(c, log)
	return edge{login: login, handoffs: handoffs, proxies: proxies, certs: certs, kubernetes: kubernetes}, err
}

// oidcLogin returns the handler of /-/oidc/ when auth.oidc is on (docs/spec/rbac.md §8), and the
// redeemer of the codes its callback hands a web UI (§10), whose token lives the shorter of
// auth.login.tokenLifetime and auth.oidc.tokenLifetime; both nil when its issuer is empty. It
// reads the client's secret from auth.oidc.clientSecretFile, never logs it, and reads nothing
// from the IdP: the discovery document loads at the first login.
func oidcLogin(c *config.Config, st index.Store, log *slog.Logger) (http.Handler, *oidc.Handoffs, error) {
	o := c.Auth.OIDC
	if o.Issuer == "" {
		return nil, nil, nil
	}
	secret, err := readClientSecret(o.ClientSecretFile)
	if err != nil {
		return nil, nil, err
	}
	base := strings.TrimSuffix(c.Server.ExternalURL, "/")
	p, err := oidc.New(oidc.Config{Issuer: o.Issuer, ClientID: o.ClientID, ClientSecret: secret,
		RedirectURL: base + "/-/oidc/callback", Scopes: o.Scopes, UsernameClaim: o.UsernameClaim, GroupsClaim: o.GroupsClaim,
		KeepGroup: auth.CheckGroupName, AllowHTTP: c.Server.InsecureHTTP, Log: log})
	if err != nil {
		return nil, nil, fmt.Errorf("auth.oidc: %w", err)
	}
	origins, err := returnOrigins(c.Server)
	if err != nil {
		return nil, nil, err
	}
	log.Info("OIDC login on", "issuer", o.Issuer, "clientID", o.ClientID, "redirectURI", base+"/-/oidc/callback",
		"tokenLifetime", o.TokenLifetime.String(), "webUIOrigins", origins)
	tokens := &auth.Tokens{Index: st, Log: log}
	login := oidc.NewHandler(oidc.LoginOptions{Provider: p, Index: st, Users: &auth.Users{Index: st, Log: log},
		Tokens: tokens, ExternalURL: base, InsecureCookie: c.Server.InsecureHTTP, TokenLifetime: o.TokenLifetime,
		ReturnOrigins: origins, Log: log})
	handoffs := &oidc.Handoffs{Index: st, Tokens: tokens, TokenLifetime: min(c.Auth.Login.TokenLifetime, o.TokenLifetime), Log: log}
	return login, handoffs, nil
}

// returnOrigins returns the origins a web UI's OIDC login may return to (docs/spec/rbac.md §10):
// the origin of server.externalURL, then those of server.corsOrigins.
func returnOrigins(s config.Server) ([]string, error) {
	u, err := url.Parse(s.ExternalURL)
	if err != nil {
		return nil, fmt.Errorf("server.externalURL: %w", err)
	}
	return append([]string{oidc.OriginOf(u)}, s.CORSOrigins...), nil
}

// readClientSecret reads the client's secret from path: one line, surrounding white space
// trimmed, not empty, at most maxClientSecret bytes. Its errors never quote the file.
func readClientSecret(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // the operator's configuration names the file
	if err != nil {
		return "", fmt.Errorf("auth.oidc.clientSecretFile: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxClientSecret+1))
	if err != nil {
		return "", fmt.Errorf("auth.oidc.clientSecretFile: %w", err)
	}
	s := strings.TrimSpace(string(b))
	switch {
	case len(b) > maxClientSecret:
		return "", fmt.Errorf("auth.oidc.clientSecretFile: %s is larger than %d bytes", path, maxClientSecret)
	case s == "":
		return "", fmt.Errorf("auth.oidc.clientSecretFile: %s is empty", path)
	case strings.ContainsAny(s, "\r\n"):
		return "", errors.New("auth.oidc.clientSecretFile: the secret is more than one line")
	}
	return s, nil
}

// handoffsOf returns the redeemer of e as the management API takes it: nil, not a nil
// *oidc.Handoffs, while auth.oidc is off.
func handoffsOf(e edge) api.Handoffs {
	if e.handoffs == nil {
		return nil
	}
	return e.handoffs
}

package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/oidc"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// maxClientSecret bounds the file of auth.oidc.clientSecretFile.
const maxClientSecret = 4 << 10

// edgeInputs returns what the server needs of the configuration beside the services: the
// handler of the OIDC login (oidcLogin) and the trusted proxies.
func edgeInputs(c *config.Config, st index.Store, log *slog.Logger) (http.Handler, []netip.Prefix, error) {
	login, err := oidcLogin(c, st, log)
	if err != nil {
		return nil, nil, err
	}
	proxies, err := c.Server.TrustedPrefixes()
	return login, proxies, err
}

// oidcLogin returns the handler of /-/oidc/ when auth.oidc is on (docs/spec/rbac.md §8), and nil
// when its issuer is empty. It reads the client's secret from auth.oidc.clientSecretFile, never
// logs it, and reads nothing from the IdP: the discovery document loads at the first login.
func oidcLogin(c *config.Config, st index.Store, log *slog.Logger) (http.Handler, error) {
	o := c.Auth.OIDC
	if o.Issuer == "" {
		return nil, nil
	}
	secret, err := readClientSecret(o.ClientSecretFile)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(c.Server.ExternalURL, "/")
	p, err := oidc.New(oidc.Config{Issuer: o.Issuer, ClientID: o.ClientID, ClientSecret: secret,
		RedirectURL: base + "/-/oidc/callback", Scopes: o.Scopes, UsernameClaim: o.UsernameClaim, GroupsClaim: o.GroupsClaim,
		KeepGroup: auth.CheckGroupName, AllowHTTP: c.Server.InsecureHTTP, Log: log})
	if err != nil {
		return nil, fmt.Errorf("auth.oidc: %w", err)
	}
	log.Info("OIDC login on", "issuer", o.Issuer, "clientID", o.ClientID, "redirectURI", base+"/-/oidc/callback",
		"tokenLifetime", o.TokenLifetime.String())
	return oidc.NewHandler(oidc.LoginOptions{Provider: p, Index: st, Users: &auth.Users{Index: st, Log: log},
		Tokens: &auth.Tokens{Index: st, Log: log}, ExternalURL: base, InsecureCookie: c.Server.InsecureHTTP,
		TokenLifetime: o.TokenLifetime, Log: log}), nil
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

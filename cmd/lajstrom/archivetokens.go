package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/terraform"
)

// archiveTokensPlugin is a plugin that signs URLs with bearer tokens, terraform's archive URLs:
// it takes the issuer and the verifier of them.
type archiveTokensPlugin interface {
	WithArchiveTokens(t terraform.ArchiveTokens) format.Plugin
}

// withArchiveTokens returns plugins with t given to each plugin that signs URLs.
func withArchiveTokens(plugins []format.Plugin, t terraform.ArchiveTokens) []format.Plugin {
	for i, p := range plugins {
		if u, ok := p.(archiveTokensPlugin); ok {
			plugins[i] = u.WithArchiveTokens(t)
		}
	}
	return plugins
}

// archiveTokens are terraform.ArchiveTokens over the token keys of the facade
// (docs/spec/rbac.md §4): an issuer and a verifier of their own, which the issuer tells of each
// key it signs with, for the service of server.externalURL, or of the request's Host without it
// (token.Realm), as the facade's.
type archiveTokens struct {
	issuer      *token.Issuer
	verifier    *token.Verifier
	externalURL string
}

// newArchiveTokens returns the archive tokens over keys.
func newArchiveTokens(keys token.Keys, externalURL string, log *slog.Logger) archiveTokens {
	v := token.NewVerifier(token.Options{Keys: keys, Log: log})
	return archiveTokens{issuer: token.NewIssuer(token.Options{Keys: keys, Verifier: v, Log: log}), verifier: v, externalURL: externalURL}
}

// Issue implements terraform.ArchiveTokens: a token whose one access entry is ref with pull.
func (a archiveTokens) Issue(ctx context.Context, r *http.Request, p auth.Principal, ref string) (string, error) {
	_, service := token.Realm(a.externalURL, r)
	t, err := a.issuer.Issue(ctx, p, service, []token.Access{{Type: terraform.AccessType, Name: ref, Actions: []string{token.ActionPull}}})
	if err != nil {
		return "", err
	}
	return t.Raw, nil
}

// Allows implements terraform.ArchiveTokens.
func (a archiveTokens) Allows(ctx context.Context, r *http.Request, raw, ref string) (bool, error) {
	_, service := token.Realm(a.externalURL, r)
	g, err := a.verifier.Verify(ctx, raw, service)
	switch {
	case errors.Is(err, token.ErrInvalidToken):
		return false, nil
	case err != nil:
		return false, err
	}
	return slices.ContainsFunc(g.Access, func(e token.Access) bool {
		return e.Type == terraform.AccessType && e.Name == ref && slices.Contains(e.Actions, token.ActionPull)
	}), nil
}

package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/lajstrom/lajstrom/internal/auth/oidc"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// Asking the IdP again about each OIDC login (docs/spec/rbac.md §8, #1282; ADR 0037):
// auth.oidc.refresh.interval, its master key, and its warning at start.

// errRefreshWithoutKey is serve's refusal of auth.oidc.refresh.interval without a master key.
var errRefreshWithoutKey = errors.New("auth.oidc.refresh.interval is set and no master key is: asking the IdP again keeps each " +
	"login's refresh token sealed with the master key. Configure one (keys.masterKeyEnv, keys.masterKeyFile, keys.masterKeyDir " +
	"or keys.masterKeyKMS), or set auth.oidc.refresh.interval to 0")

// checkRefreshKey is serve's check of auth.oidc.refresh.interval at start: with auth.oidc on, an
// interval takes a master key, m; without one the configuration is refused (exit status 2).
func checkRefreshKey(o config.OIDC, m *keys.Master) error {
	if o.Issuer == "" || o.Refresh.Interval == 0 || m != nil {
		return nil
	}
	return configError{errRefreshWithoutKey}
}

// warnRefreshGrant logs at warn, at start with auth.oidc.refresh.interval, when the IdP's
// discovery document lists grant_types_supported without refresh_token: its logins may keep no
// refresh token, and last auth.oidc.tokenLifetime each. A document that lists none, or that does
// not load, says nothing: the callback warns of each login without a refresh token. Nothing
// without the setting or without auth.oidc (p nil).
func warnRefreshGrant(ctx context.Context, o config.OIDC, p *oidc.Client, log *slog.Logger) {
	if p == nil || o.Refresh.Interval == 0 {
		return
	}
	grants, err := p.GrantTypes(ctx)
	if err != nil {
		log.DebugContext(ctx, "the IdP's grant types not read at start", "error", err.Error())
		return
	}
	if grants != nil && !slices.Contains(grants, "refresh_token") {
		log.WarnContext(ctx, "the IdP's discovery document lists no refresh_token grant: its logins may keep no refresh token, "+
			"and are then not asked about again (auth.oidc.refresh.interval)", "grantTypesSupported", grants)
	}
}

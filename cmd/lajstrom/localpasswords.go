package main

import (
	"errors"
	"log/slog"

	"github.com/lajstrom/lajstrom/internal/config"
)

// errPasswordsOffWithoutOIDC is serve's refusal of auth.localPasswords false without an IdP.
var errPasswordsOffWithoutOIDC = errors.New("auth.localPasswords is false and auth.oidc.issuer is empty: no person could log in. " +
	"Configure auth.oidc, or set auth.localPasswords to true")

// checkLocalPasswords is serve's check of auth.localPasswords at start (docs/spec/rbac.md §1,
// Local passwords): false needs auth.oidc.issuer, since people then log in through the IdP only;
// and it says so at info when they are off.
func checkLocalPasswords(a config.Auth, log *slog.Logger) error {
	if a.LocalPasswords {
		return nil
	}
	if a.OIDC.Issuer == "" {
		return errPasswordsOffWithoutOIDC
	}
	log.Info("local passwords are off: every password is refused, and none is set; people log in through the IdP",
		"auth.localPasswords", false)
	return nil
}

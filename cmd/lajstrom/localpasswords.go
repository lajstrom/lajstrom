package main

import (
	"errors"
	"log/slog"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
)

// errPasswordsOffWithoutOIDC is serve's refusal of auth.localPasswords false without an IdP.
var errPasswordsOffWithoutOIDC = errors.New("auth.localPasswords is false and auth.oidc.issuer is empty: no person could log in. " +
	"Configure auth.oidc, or set auth.localPasswords to true")

// checkLocalPasswords is serve's check of auth.localPasswords at start (docs/spec/rbac.md §1,
// Local passwords): false needs auth.oidc.issuer, since people then log in through the IdP only.
// It says at info when passwords are off, and when they are locked out after wrong ones
// (auth.lockout.duration), which does nothing while they are off.
func checkLocalPasswords(a config.Auth, log *slog.Logger) error {
	switch {
	case a.LocalPasswords && a.Lockout.Duration > 0:
		log.Info("the lockout is on: wrong passwords in a row lock a password", "failures", auth.LockoutFailures,
			"auth.lockout.duration", a.Lockout.Duration.String())
	case a.LocalPasswords:
	case a.OIDC.Issuer == "":
		return errPasswordsOffWithoutOIDC
	default:
		log.Info("local passwords are off: every password is refused, and none is set; people log in through the IdP",
			"auth.localPasswords", false, "auth.lockout.duration", a.Lockout.Duration.String())
	}
	return nil
}

package main

import (
	"errors"
	"log/slog"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
)

// errPasswordsOffWithoutOIDC is serve's refusal of auth.localPasswords false without an IdP.
var errPasswordsOffWithoutOIDC = errors.New("auth.localPasswords is false and auth.oidc.issuer is empty: no person could log in. " +
	"Configure auth.oidc, or set auth.localPasswords to true")

// lockoutOf is auth.lockout.duration, 0 while auth.localPasswords is false, when it does nothing.
func lockoutOf(a config.Auth) time.Duration {
	if !a.LocalPasswords {
		return 0
	}
	return a.Lockout.Duration
}

// passwordPolicyOf is auth.passwords, the rules of a password and its change at first use
// (docs/spec/rbac.md §1, Local passwords), for auth.Users: of serve, of its bootstrap and of
// lajstrom admin user.
func passwordPolicyOf(a config.Auth) auth.PasswordPolicy {
	return auth.PasswordPolicy{MinLength: a.Passwords.MinLength, RequireLettersAndDigits: a.Passwords.RequireLettersAndDigits,
		ChangeAtFirstUse: a.Passwords.ChangeAtFirstUse}
}

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

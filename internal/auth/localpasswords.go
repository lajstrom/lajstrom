package auth

import (
	"context"
	"errors"
	"fmt"
)

// Local passwords turned off (docs/spec/rbac.md §1, Local passwords; #1280, ADR 0028): with
// auth.localPasswords false, no password is a credential, an admin's included, on any route that
// takes one, and none is set. People log in through the IdP, and the command line against the
// database is the break-glass path.

// ErrPasswordsOff means that a password was refused because local passwords are off on this
// server: before a user is read, without argon2id, counted by no throttle. It wraps
// ErrBadCredentials; the message of its 401 is MsgPasswordsOff (RefusalMessage).
var ErrPasswordsOff = fmt.Errorf("%w: local passwords are off", ErrBadCredentials)

// MsgPasswordsOff is the message of the 401 of every password while passwords are off.
const MsgPasswordsOff = "passwords are off on this server: use a personal access token, or log in through the IdP with lajstrom login"

// RefusalMessage returns the message of the 401 of err, a refused credential, when the
// authentication chose one, and fallback, the caller's own, otherwise. The server's Basic
// credentials and /v2/token, npm login and POST /api/v1/login answer it alike, the same whatever
// the name and the reason (docs/spec/rbac.md §1, Local passwords): while passwords are off, every
// password's says so (ErrPasswordsOff), and while the lockout is set, what locks a password
// (ErrPasswordRefused, LockoutMessage). The message is the authenticator's text, never a value of
// the request.
func (a *Authenticator) RefusalMessage(err error, fallback string) string {
	switch {
	case errors.Is(err, ErrPasswordsOff):
		return MsgPasswordsOff
	case errors.Is(err, ErrPasswordRefused) && a.lockoutMsg != "":
		return a.lockoutMsg
	}
	return fallback
}

// passwordsRefused refuses a password while passwords are off (Options.PasswordsOff), before a
// user is read: the line of the refusal, with the account unknown and the reason passwords off,
// and ErrPasswordsOff. It returns nil while they are on.
func (a *Authenticator) passwordsRefused(ctx context.Context) error {
	if !a.passwordsOff {
		return nil
	}
	LogRefusal(ctx, a.log, string(CredPassword), AccountUnknown, 0, "passwords off")
	return ErrPasswordsOff
}

// CheckPasswordSet returns nil when a password may be set, and while local passwords are off
// (PasswordsOff) an error wrapping ErrInvalid that names the setting: Create and SetPassword
// refuse with it, and the command line asks it before its prompt (docs/spec/rbac.md §1, Local
// passwords).
func (s *Users) CheckPasswordSet() error {
	if !s.PasswordsOff {
		return nil
	}
	return invalidf("password: local passwords are off on this server (auth.localPasswords: false), and none is set: " +
		"people log in through the IdP, and a service account authenticates with its tokens")
}

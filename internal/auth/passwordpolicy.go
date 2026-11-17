package auth

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The rules of a local password and its change at first use (docs/spec/rbac.md §1, Local
// passwords; #1281): auth.passwords. The rules are checked wherever a password is set, by an
// admin, at the bootstrap and by the user's own change; a password set before keeps working,
// since a hash cannot be checked. Every password set keeps the hash it replaces in the user's
// history, against which the user's own change is compared.

// Bounds of auth.passwords.minLength, in Unicode code points: DefaultMinPasswordLength by default,
// and from it to MaxMinPasswordLength.
const (
	DefaultMinPasswordLength = 8
	MaxMinPasswordLength     = 128
)

// PasswordHistoryLen is how many passwords before the current one a user's history keeps
// (index.User.PasswordHistory): with the current one, the last four of PCI DSS 8.3.7.
const PasswordHistoryLen = 3

// PasswordPolicy is auth.passwords (docs/spec/rbac.md §1, Local passwords): the rules of a
// password (Check) and the change at first use. The zero PasswordPolicy is the default one.
type PasswordPolicy struct {
	// MinLength is auth.passwords.minLength: the fewest characters of a password, counted as
	// Unicode code points of the password as it was sent, which is not normalized. 0 means
	// DefaultMinPasswordLength.
	MinLength int
	// RequireLettersAndDigits is auth.passwords.requireLettersAndDigits: a password holds at
	// least one letter, a code point of Unicode's general category L, and one digit, of Nd, of
	// any script.
	RequireLettersAndDigits bool
	// ChangeAtFirstUse is auth.passwords.changeAtFirstUse: every password that is not the
	// user's own change sets its mustChange (index.User.MustChange); without it, an admin's new
	// password clears it.
	ChangeAtFirstUse bool
}

// Check checks password against the rules of p: UTF-8 of at most MaxPasswordLen bytes and of at
// least MinLength characters, that does not start with the prefix of a token, PATPrefix or
// DeployPrefix (the Authenticator takes a secret with either for a token, never for a password),
// and with RequireLettersAndDigits holds a letter and a digit. Errors wrap ErrInvalid and name the
// rule, with its setting where it has one; they never contain the password.
func (p PasswordPolicy) Check(password string) error {
	minimum := p.MinLength
	if minimum <= 0 {
		minimum = DefaultMinPasswordLength
	}
	switch {
	case !utf8.ValidString(password):
		return invalidf("password: must be UTF-8")
	case len(password) > MaxPasswordLen:
		return invalidf("password: must be at most %d bytes", MaxPasswordLen)
	case utf8.RuneCountInString(password) < minimum:
		return invalidf("password: must be at least %d characters (auth.passwords.minLength)", minimum)
	case strings.HasPrefix(password, PATPrefix), strings.HasPrefix(password, DeployPrefix):
		return invalidf("password: must not start with %s or %s: a secret with either is a token's", PATPrefix, DeployPrefix)
	case p.RequireLettersAndDigits && !lettersAndDigits(password):
		return invalidf("password: must hold at least one letter and one digit, of any script (auth.passwords.requireLettersAndDigits)")
	}
	return nil
}

// lettersAndDigits reports whether s holds a letter, of Unicode's general category L, and a
// digit, of Nd.
func lettersAndDigits(s string) bool {
	var letter, digit bool
	for _, r := range s {
		letter = letter || unicode.IsLetter(r)
		digit = digit || unicode.IsDigit(r)
		if letter && digit {
			return true
		}
	}
	return false
}

// withHistory returns history with replaced, the hash that a new password replaces, at its head,
// cut to PasswordHistoryLen: every password set, an admin's too, keeps the one before it, so
// that the user's next change returns to neither (docs/spec/index-schema.md, A password's
// history). A user without a password (replaced "") adds nothing.
func withHistory(history []string, replaced string) []string {
	if replaced != "" {
		history = append([]string{replaced}, history...)
	}
	if len(history) > PasswordHistoryLen {
		history = history[:PasswordHistoryLen]
	}
	return history
}

// hashNew checks password, a password an admin or the bootstrap sets, and returns its argon2id
// hash: while local passwords are off none is set (CheckPasswordSet), and the password follows
// the rules of PasswordPolicy.
func (s *Users) hashNew(password string) (string, error) {
	if err := s.CheckPasswordSet(); err != nil {
		return "", err
	}
	if err := s.PasswordPolicy.Check(password); err != nil {
		return "", err
	}
	return hashWith(hashParams, password), nil
}

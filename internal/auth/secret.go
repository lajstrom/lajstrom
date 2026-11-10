package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"regexp"
	"strings"
)

// Prefixes of the token secrets: the kind is readable in the secret, so that the authenticator
// tells a token from a password by its prefix alone (CheckPassword refuses passwords that start
// with either), and so that a secret scanner recognises a leaked one.
const (
	// PATPrefix starts the secret of a personal access token.
	PATPrefix = "ljs_pat_"
	// DeployPrefix starts the secret of a deploy token.
	DeployPrefix = "ljs_dep_"
)

// secretDigits is the length of the random part of a secret: 32 bytes in base 62 need 43
// digits (62^43 > 2^256), and a shorter number is padded with zeros, so that every secret has
// the same length.
const secretDigits = 43

// SecretLen is the length of a token secret: the prefix and 43 base-62 digits.
const SecretLen = len(PATPrefix) + secretDigits

// secretInText matches what may be a token secret in a text, a path that carries one (npm
// logout's DELETE /-/user/token/<token>) or a message: either prefix and what follows it.
var secretInText = regexp.MustCompile(`(` + PATPrefix + `|` + DeployPrefix + `)[0-9A-Za-z]+`)

// RedactSecrets returns s with every token secret in it replaced by its prefix and "…", for the
// logs, which never have a secret.
func RedactSecrets(s string) string {
	if !strings.Contains(s, "ljs_") {
		return s
	}
	return secretInText.ReplaceAllString(s, "${1}…")
}

// HashPrefixLen is the number of hexadecimal characters of the hash that every view of a token
// shows after its creation (docs/acceptance.md X-03): enough to tell tokens apart, far too few
// to find the secret.
const HashPrefixLen = 8

// newSecret returns a new secret with prefix: 32 bytes of crypto/rand written as a number in
// base 62 (0-9, a-z, A-Z), padded to 43 digits. It has no character that a URL, a shell or a
// netrc file treats specially, and a double-click selects all of it.
func newSecret(prefix string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read does not return errors; it crashes the program instead
	return encodeSecret(prefix, b)
}

// encodeSecret returns prefix and the 32 bytes of b as a number in base 62, padded to 43 digits.
func encodeSecret(prefix string, b []byte) string {
	digits := new(big.Int).SetBytes(b).Text(62)
	return prefix + strings.Repeat("0", secretDigits-len(digits)) + digits
}

// HashSecret returns what the index stores of a secret, tokens.hash: the lowercase hexadecimal
// sha256 of the whole secret, prefix included.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// tokenShaped reports whether secret is a token secret of prefix: the prefix, then 43 digits of
// base 62.
func tokenShaped(secret, prefix string) bool {
	digits, ok := strings.CutPrefix(secret, prefix)
	if !ok || len(digits) != secretDigits {
		return false
	}
	for i := range len(digits) {
		if c := digits[i]; !isBase62(c) {
			return false
		}
	}
	return true
}

func isBase62(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

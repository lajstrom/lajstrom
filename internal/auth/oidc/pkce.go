package oidc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// NewSecret returns 256 random bits in base64url without padding, 43 characters: a state, a
// nonce, a PKCE verifier (RFC 7636 §4.1: 43 to 128 characters of its alphabet), a browser
// cookie or a device code.
func NewSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read does not return errors; it crashes the program instead
	return base64.RawURLEncoding.EncodeToString(b)
}

// Challenge returns the S256 code challenge of the PKCE verifier (RFC 7636 §4.2):
// base64url(sha256(verifier)).
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Hash returns the hex sha256 of a secret: how the index keeps a state, a cookie and a device
// code (index-schema.md, oidc_logins).
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

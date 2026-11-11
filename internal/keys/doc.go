// Package keys manages signing keys: the per-repo OpenPGP keys that sign apt indexes, the server
// key that signs the bearer tokens of /v2, and their encryption at rest with the master key.
// See docs/spec/index-schema.md (keys), docs/operations.md (keys and apt signing) and ADR 0010.
//
// The master key is 32 bytes, 64 hexadecimal characters in the environment variable that
// keys.masterKeyEnv names, loaded once at start (LoadMaster). Service implements
// format.Signer over the index: a repo's key is made on its first use (KeyFor), its private
// key stored as nonce || AES-256-GCM with the fingerprint as additional data; ClearSign and
// DetachSign make apt's InRelease and Release.gpg, PublicKeys its key.asc, Current tells
// whether a cached InRelease or Release.gpg is still signed by the keys that sign; Rotate
// retires the active key.
//
// Tokens keeps the bearer-token key (issue #26): a row of kind jwt without a repo, an ECDSA
// P-256 (ES256) or Ed25519 (EdDSA) key after keys.tokenAlgorithm, made on first use, sealed the
// same way; TokenSigner and TokenVerifiers are what internal/auth/token signs and verifies
// with, RotateToken and DropToken what lajstrom admin key rotate --token and drop --token do. A
// retired token key verifies for TokenGrace, 21 minutes. Without a master key the server
// signs with EphemeralTokens, a key in memory.
package keys

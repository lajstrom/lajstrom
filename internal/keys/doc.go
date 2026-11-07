// Package keys manages signing keys: the per-repo OpenPGP keys that sign apt indexes, and
// their encryption at rest with the master key. See docs/spec/index-schema.md (keys) and
// docs/operations.md (keys and apt signing).
//
// The master key is 32 bytes, 64 hexadecimal characters in the environment variable that
// keys.masterKeyEnv names, loaded once at start (LoadMaster). Service implements
// format.Signer over the index: a repo's key is made on its first use (KeyFor), its private
// key stored as nonce || AES-256-GCM with the fingerprint as additional data; ClearSign and
// DetachSign make apt's InRelease and Release.gpg, PublicKeys its key.asc; Rotate retires the
// active key. The server signing key (kind jwt) comes with issue #26.
package keys

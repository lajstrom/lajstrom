// Package blobcrypt is the blob cipher of encrypted repos (docs/adr/0009, Amendment;
// docs/spec/manifest-profile.md §11): OCI encrypted layers as containers/ocicrypt reads them, so
// that skopeo and ctr-enc decrypt what Lajstrom stored.
//
// A blob is encrypted with AES_256_CTR_HMAC_SHA256: a data key of 32 random bytes and a nonce of
// 16, AES-256 in counter mode from the nonce as the initial counter block, and HMAC-SHA256 under
// the data key over the whole ciphertext. The ciphertext is as long as the plaintext, a reader
// can start at any offset (At), and the HMAC proves a whole blob only. Private holds the data key,
// the nonce and the plaintext digest; Wrap writes it as a JWE (RSA-OAEP-256, A256GCM) for the
// repo's key and the recipients of the policy, and Unwrap opens one with a KeyDecrypter, a private
// key here or a key in a KMS. Public holds the HMAC. The package does no I/O and keeps no state:
// the primitives are the standard library's, the JWE go-jose's.
package blobcrypt

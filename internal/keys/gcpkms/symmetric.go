package gcpkms

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"strconv"

	"github.com/lajstrom/lajstrom/internal/keys"
)

// The symmetric keys of Cloud KMS that wrap the data keys of the master key (#1388, ADR 0036):
// cryptoKeys.get, encrypt and decrypt of a key named without a version, AES-256 in the KMS, each
// encrypt and decrypt bound to additional authenticated data, the CRC32C of both sides checked.

const (
	// purposeEncrypt and algorithmSymmetric are the purpose of a key that wraps and the
	// algorithm of its primary version.
	purposeEncrypt     = "ENCRYPT_DECRYPT"
	algorithmSymmetric = "GOOGLE_SYMMETRIC_ENCRYPTION"
)

var _ keys.MasterKMS = (*Client)(nil)

// SymmetricKey describes the key of name, projects/…/cryptoKeys/<k> without a version, with
// cryptoKeys.get, and returns name: the KMS encrypts with the primary version and decrypts with
// the version that the ciphertext names. A key whose purpose is not ENCRYPT_DECRYPT, or whose
// primary version is not GOOGLE_SYMMETRIC_ENCRYPTION and ENABLED, is keys.ErrKMSKeyCannotWrap,
// naming them.
func (c *Client) SymmetricKey(ctx context.Context, name string) (string, error) {
	var k struct {
		Purpose string
		Primary *struct{ Name, State, Algorithm string }
	}
	if err := c.call(ctx, http.MethodGet, name, "", nil, &k); err != nil {
		return "", err
	}
	switch {
	case k.Purpose != purposeEncrypt:
		return "", fmt.Errorf("%w: %s is a key of purpose %s, not %s", keys.ErrKMSKeyCannotWrap, name, cmp.Or(k.Purpose, "none"),
			purposeEncrypt)
	case k.Primary == nil:
		return "", fmt.Errorf("%w: %s has no primary version", keys.ErrKMSKeyCannotWrap, name)
	case k.Primary.Algorithm != algorithmSymmetric:
		return "", fmt.Errorf("%w: the primary version of %s is of %s, not %s", keys.ErrKMSKeyCannotWrap, name, k.Primary.Algorithm,
			algorithmSymmetric)
	case k.Primary.State != "ENABLED":
		return "", fmt.Errorf("%w: the primary version of %s is %s, not ENABLED", keys.ErrKMSKeyCannotWrap, name, k.Primary.State)
	}
	return name, nil
}

// crcOf is the CRC32C of b as Cloud KMS writes an int64: decimal, in a string.
func crcOf(b []byte) string { return strconv.FormatUint(uint64(crc32.Checksum(b, castagnoli)), 10) }

// Encrypt encrypts plain with the key name (cryptoKeys.encrypt, its primary version), ad.String()
// as the additional authenticated data, and returns the KMS's ciphertext.
func (c *Client) Encrypt(ctx context.Context, name string, plain []byte, ad keys.AdditionalData) ([]byte, error) {
	aad := []byte(ad.String())
	in := map[string]any{"plaintext": plain, "plaintextCrc32c": crcOf(plain), "additionalAuthenticatedData": aad,
		"additionalAuthenticatedDataCrc32c": crcOf(aad)}
	var out struct {
		Ciphertext                                []byte
		CiphertextCrc32c                          string
		VerifiedPlaintextCrc32c                   bool
		VerifiedAdditionalAuthenticatedDataCrc32c bool
	}
	if err := c.call(ctx, http.MethodPost, name, ":encrypt", in, &out); err != nil {
		return nil, err
	}
	switch {
	case !out.VerifiedPlaintextCrc32c || !out.VerifiedAdditionalAuthenticatedDataCrc32c:
		return nil, fmt.Errorf("%w: Cloud KMS did not verify the CRC32C of the plaintext and the additional data", keys.ErrKMSUnavailable)
	case len(out.Ciphertext) == 0:
		return nil, errors.New("gcpkms: encrypt answered no ciphertext")
	}
	if err := checkCRC(out.Ciphertext, out.CiphertextCrc32c); err != nil {
		return nil, fmt.Errorf("%w: the ciphertext: %w", keys.ErrKMSUnavailable, err)
	}
	return out.Ciphertext, nil
}

// DecryptSymmetric decrypts ciphertext with the key name (cryptoKeys.decrypt, the version the
// ciphertext names), ad.String() as the additional authenticated data: a ciphertext of another
// key or other data is refused by the KMS.
func (c *Client) DecryptSymmetric(ctx context.Context, name string, ciphertext []byte, ad keys.AdditionalData) ([]byte, error) {
	aad := []byte(ad.String())
	in := map[string]any{"ciphertext": ciphertext, "ciphertextCrc32c": crcOf(ciphertext), "additionalAuthenticatedData": aad,
		"additionalAuthenticatedDataCrc32c": crcOf(aad)}
	var out struct {
		Plaintext       []byte
		PlaintextCrc32c string
	}
	if err := c.call(ctx, http.MethodPost, name, ":decrypt", in, &out); err != nil {
		return nil, err
	}
	if len(out.Plaintext) == 0 {
		return nil, errors.New("gcpkms: decrypt answered no plaintext")
	}
	if err := checkCRC(out.Plaintext, out.PlaintextCrc32c); err != nil {
		clear(out.Plaintext)
		return nil, fmt.Errorf("%w: the plaintext: %w", keys.ErrKMSUnavailable, err)
	}
	return out.Plaintext, nil
}

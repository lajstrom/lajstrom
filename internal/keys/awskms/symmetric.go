package awskms

import (
	"context"
	"errors"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/keys"
)

// The symmetric keys of AWS KMS that wrap the data keys of the master key (#1388, ADR 0036):
// DescribeKey, Encrypt and Decrypt, AES-256 in the KMS, each Encrypt and Decrypt bound to an
// encryption context.

const (
	// specSymmetric and usageEncrypt are the key spec and the key usage of a key that wraps.
	specSymmetric = "SYMMETRIC_DEFAULT"
	usageEncrypt  = "ENCRYPT_DECRYPT"
)

var _ keys.MasterKMS = (*Client)(nil)

// SymmetricKey describes the key of name, a key id, key ARN, alias/<name> or alias ARN, with
// DescribeKey, and returns its ARN, which Encrypt and DecryptSymmetric name from then on. A key
// whose spec is not SYMMETRIC_DEFAULT, whose usage is not ENCRYPT_DECRYPT, or that is not Enabled
// is keys.ErrKMSKeyCannotWrap, naming its spec, usage and state.
func (c *Client) SymmetricKey(ctx context.Context, name string) (string, error) {
	var desc struct {
		KeyMetadata struct{ Arn, KeySpec, KeyUsage, KeyState string }
	}
	if err := c.call(ctx, name, "DescribeKey", map[string]string{"KeyId": name}, &desc); err != nil {
		return "", err
	}
	m := desc.KeyMetadata
	switch {
	case m.Arn == "":
		return "", errors.New("awskms: DescribeKey answered no key ARN")
	case m.KeySpec != specSymmetric || m.KeyUsage != usageEncrypt:
		return "", fmt.Errorf("%w: %s is a key of %s for %s, not %s for %s", keys.ErrKMSKeyCannotWrap, m.Arn, m.KeySpec, m.KeyUsage,
			specSymmetric, usageEncrypt)
	case m.KeyState != "Enabled":
		return "", fmt.Errorf("%w: %s is %s, not Enabled", keys.ErrKMSKeyCannotWrap, m.Arn, m.KeyState)
	}
	return m.Arn, nil
}

// Encrypt encrypts plain with the symmetric key of the ARN name, under the encryption context
// {ad.Key: ad.Value}, and returns the KMS's ciphertext.
func (c *Client) Encrypt(ctx context.Context, name string, plain []byte, ad keys.AdditionalData) ([]byte, error) {
	in := map[string]any{"KeyId": name, "Plaintext": plain, "EncryptionContext": map[string]string{ad.Key: ad.Value}}
	var out struct {
		CiphertextBlob []byte
		KeyID          string `json:"KeyId"`
	}
	if err := c.call(ctx, name, "Encrypt", in, &out); err != nil {
		return nil, err
	}
	switch {
	case len(out.CiphertextBlob) == 0:
		return nil, errors.New("awskms: Encrypt answered no ciphertext")
	case out.KeyID != name:
		return nil, fmt.Errorf("awskms: Encrypt answered with the key %q, not %s", out.KeyID, name)
	}
	return out.CiphertextBlob, nil
}

// DecryptSymmetric decrypts ciphertext with the symmetric key of the ARN name, under the
// encryption context {ad.Key: ad.Value}: a ciphertext of another key or another context is
// refused by the KMS, and an answer that names another key is an error.
func (c *Client) DecryptSymmetric(ctx context.Context, name string, ciphertext []byte, ad keys.AdditionalData) ([]byte, error) {
	in := map[string]any{"KeyId": name, "CiphertextBlob": ciphertext, "EncryptionContext": map[string]string{ad.Key: ad.Value}}
	var out struct {
		Plaintext []byte
		KeyID     string `json:"KeyId"`
	}
	if err := c.call(ctx, name, "Decrypt", in, &out); err != nil {
		return nil, err
	}
	switch {
	case len(out.Plaintext) == 0:
		return nil, errors.New("awskms: Decrypt answered no plaintext")
	case out.KeyID != name:
		clear(out.Plaintext)
		return nil, fmt.Errorf("awskms: Decrypt answered with the key %q, not %s", out.KeyID, name)
	}
	return out.Plaintext, nil
}

package format

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/blobcrypt"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The modes of policy.encryption (docs/api.md, the policy keys).
const (
	EncryptionNone   = "none"
	EncryptionAtRest = "at-rest"
)

// Encryption wraps and opens the data keys of encrypted repos (docs/spec/manifest-profile.md
// §11, ADR 0009); internal/keys implements it over the index, the master key and the KMS, and
// format does not import keys. A KMS that does not answer is an error wrapping ErrUnavailable
// (503 with Retry-After).
type Encryption interface {
	// Wrap wraps opts for the repo's active key, made on first use from policy.encryption.key,
	// and for the recipients of the policy: the value of the annotation
	// org.opencontainers.image.enc.keys.jwe, and the fingerprint of the active key.
	Wrap(ctx context.Context, repo index.Repo, opts blobcrypt.Private) (jwe, fingerprint string, err error)
	// Unwrap opens jwe with the repo's key of fingerprint, active or retired. What it opened is
	// cached for a while in memory.
	Unwrap(ctx context.Context, repo index.Repo, fingerprint, jwe string) (blobcrypt.Private, error)
	// Seal encrypts the data key of a facade upload for its row with the master key the index
	// names, aad the row's key; Open decrypts it with any master key the process holds, that one
	// first. Seal fails with keys.ErrNoSealingKey while the process does not hold it
	// (index-schema.md, The master key and its replacement, #1288), a 503 with Retry-After.
	Seal(aad string, key []byte) ([]byte, error)
	Open(aad string, sealed []byte) ([]byte, error)
}

// EncryptionPolicy is policy.encryption of a repo.
type EncryptionPolicy struct {
	// Mode is EncryptionNone or EncryptionAtRest.
	Mode string `json:"mode"`
	// Key is the key URI of a KMS key that the repo's next key is, or "" for one made here.
	Key string `json:"key,omitempty"`
	// Recipients are the PEM public keys that every new data key is wrapped for as well.
	Recipients []string `json:"recipients,omitempty"`
}

// AtRest reports whether the repo's new files are encrypted.
func (p EncryptionPolicy) AtRest() bool { return p.Mode == EncryptionAtRest }

// EncryptionOf reads policy.encryption of the repo; no key is mode none. A policy that does not
// parse is an error, which StatusOf answers with 500: a write is not let through a policy that
// cannot be read, since it could store in the clear what should be encrypted. The API checks the
// policy (docs/api.md); an oci repo and a group never encrypt, whatever their row says.
func EncryptionOf(repo index.Repo) (EncryptionPolicy, error) {
	none := EncryptionPolicy{Mode: EncryptionNone}
	if len(repo.Policy) == 0 || repo.Format == "oci" || repo.Kind == index.KindGroup {
		return none, nil
	}
	var p struct {
		Encryption *EncryptionPolicy `json:"encryption"`
	}
	if err := json.Unmarshal(repo.Policy, &p); err != nil {
		return none, fmt.Errorf("format: repo %s/%s: policy encryption: %w", repo.Format, repo.Name, err)
	}
	if p.Encryption == nil {
		return none, nil
	}
	switch p.Encryption.Mode {
	case EncryptionNone, EncryptionAtRest:
		return *p.Encryption, nil
	}
	return none, fmt.Errorf("format: repo %s/%s: policy encryption: the mode %q is not %s or %s",
		repo.Format, repo.Name, p.Encryption.Mode, EncryptionNone, EncryptionAtRest)
}

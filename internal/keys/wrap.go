package keys

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The making of a data key of the master key, and its wrap again (#1388; docs/spec/index-schema.md,
// A master key in a KMS): the 32 bytes are made here, have the KMS wrap them, and are compared with
// what the KMS unwraps again before any row is written. They are held for the call only, and
// cleared; no error holds them or a ciphertext.

// errNotRoundTrip is the error of a KMS that does not unwrap what it wrapped.
var errNotRoundTrip = errors.New("the KMS does not unwrap what it wrapped")

// WrapMaster makes a data key of the master key: 32 bytes of crypto/rand and their id, wrapped by
// the KMS key key, its canonical name, under the additional data of the id (MasterAD), then
// unwrapped again and compared. It returns the id and the ciphertext, the row's Wrapped.
func WrapMaster(ctx context.Context, kms MasterKMS, key config.KMSKey) (string, []byte, error) {
	b := make([]byte, 32)
	defer clear(b)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error; it crashes the program instead
	id := masterID(b)
	wrapped, err := wrapChecked(ctx, kms, key, id, b)
	if err != nil {
		return "", nil, err
	}
	return id, wrapped, nil
}

// RewrapMaster wraps the data key of row again under its KMS key key, which a GCP key does with its
// primary version: it unwraps the row, checks that the 32 bytes are the key of the row's id, wraps
// them again and checks the new ciphertext as WrapMaster does. It returns the new ciphertext.
func RewrapMaster(ctx context.Context, kms MasterKMS, key config.KMSKey, row index.WrappedMasterKey) ([]byte, error) {
	if row.KMS != key.String() {
		return nil, fmt.Errorf("keys: master key %s is wrapped by %s, not %s", row.KeyID, row.KMS, key)
	}
	b, err := kms.DecryptSymmetric(ctx, key.Name, row.Wrapped, MasterAD(row.KeyID))
	if err != nil {
		return nil, fmt.Errorf("keys: master key %s: unwrap with %s: %w", row.KeyID, key, err)
	}
	defer clear(b)
	if len(b) != 32 || masterID(b) != row.KeyID {
		return nil, fmt.Errorf("%w: the KMS key %s unwrapped, for master key %s, what is not that key", ErrOtherDataKey, key, row.KeyID)
	}
	return wrapChecked(ctx, kms, key, row.KeyID, b)
}

// wrapChecked has the KMS key key wrap b, the data key id, and unwrap the ciphertext again, and
// returns it when the KMS answered b.
func wrapChecked(ctx context.Context, kms MasterKMS, key config.KMSKey, id string, b []byte) ([]byte, error) {
	wrapped, err := kms.Encrypt(ctx, key.Name, b, MasterAD(id))
	if err != nil {
		return nil, fmt.Errorf("keys: master key %s: wrap with %s: %w", id, key, err)
	}
	back, err := kms.DecryptSymmetric(ctx, key.Name, wrapped, MasterAD(id))
	defer clear(back)
	switch {
	case err != nil:
		return nil, fmt.Errorf("keys: master key %s: %w: unwrap with %s: %w", id, errNotRoundTrip, key, err)
	case subtle.ConstantTimeCompare(back, b) != 1:
		return nil, fmt.Errorf("keys: master key %s: %w: %s answered other bytes", id, errNotRoundTrip, key)
	}
	return wrapped, nil
}

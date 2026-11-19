package sqlstore

import (
	"context"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The data keys of the master key in a KMS (#1388; docs/spec/index-schema.md, A master key in a
// KMS): a row per data key, its KMS key and the KMS's ciphertext, never a key in clear.

const wrappedCols = "key_id, kms, wrapped, created_at, wrapped_at, audit_id"

func wrappedDests(k *index.WrappedMasterKey) []any {
	return []any{&k.KeyID, &k.KMS, blobDest{&k.Wrapped}, timeDest{&k.CreatedAt}, timeDest{&k.WrappedAt}, nullIDDest{&k.AuditID}}
}

// ListWrappedMasterKeys returns every row of wrapped_master_keys, by key_id.
func (c *conn) ListWrappedMasterKeys(ctx context.Context) ([]index.WrappedMasterKey, error) {
	return getAll(ctx, c, "wrapped_master_keys", wrappedDests, "SELECT "+wrappedCols+" FROM wrapped_master_keys ORDER BY key_id")
}

// PutWrappedMasterKey inserts k where old is nil and there is no row of its id, or replaces the
// ciphertext of the row that still holds old, in one statement, at the database's time: a row
// that is not the one expected changes nothing and is the ErrConflict.
func (c *conn) PutWrappedMasterKey(ctx context.Context, k *index.WrappedMasterKey, old []byte) error {
	at, err := c.DatabaseTime(ctx)
	if err != nil {
		return err
	}
	what := "wrapped_master_keys " + k.KeyID
	var n int
	if old == nil {
		k.CreatedAt, k.WrappedAt = Normalize(at), Normalize(at)
		n, err = c.exec(ctx, what, "INSERT INTO wrapped_master_keys ("+wrappedCols+") VALUES (?, ?, ?, ?, ?, ?) "+
			"ON CONFLICT (key_id) DO NOTHING", k.KeyID, k.KMS, blobArg(k.Wrapped), c.d.EncodeTime(k.CreatedAt),
			c.d.EncodeTime(k.WrappedAt), idArg(k.AuditID))
	} else {
		k.WrappedAt = Normalize(at)
		n, err = c.exec(ctx, what, "UPDATE wrapped_master_keys SET wrapped = ?, wrapped_at = ?, audit_id = ? "+
			"WHERE key_id = ? AND wrapped = ?", blobArg(k.Wrapped), c.d.EncodeTime(k.WrappedAt), idArg(k.AuditID), k.KeyID,
			blobArg(old))
	}
	if err == nil && n == 0 {
		err = fmt.Errorf("%w: %s: the row changed in between", index.ErrConflict, what)
	}
	return err
}

// DeleteWrappedMasterKey deletes the row of keyID.
func (c *conn) DeleteWrappedMasterKey(ctx context.Context, keyID string) error {
	return c.execOne(ctx, "wrapped_master_keys "+keyID, "DELETE FROM wrapped_master_keys WHERE key_id = ?", keyID)
}

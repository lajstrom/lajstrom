package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The master keys (#1288; docs/spec/index-schema.md, The master key and its replacement): the
// values they seal, read a page at a time and written again by compare-and-set, and the one row
// of sealing_key, which names the key every process seals with.

// sealedColumns are the columns of the tables of ListSealed and Reseal: the column that names a
// row's additional data, and the sealed column.
var sealedColumns = map[string]struct{ aad, value string }{
	index.SealedKeys:     {aad: "fingerprint", value: "private_enc"},
	index.SealedWebhooks: {aad: "uid", value: "secret_enc"},
	index.SealedRefresh:  {aad: "id", value: "refresh_enc"},
}

// sealedOf returns the columns of table, or an error that names it.
func sealedOf(table string) (aad, value string, err error) {
	c, ok := sealedColumns[table]
	if !ok {
		return "", "", fmt.Errorf("index: %q is not a table of sealed values (keys, webhooks, oidc_refresh)", table)
	}
	return c.aad, c.value, nil
}

// ListSealed returns the sealed values of table whose id is above afterID, by id, at most limit:
// a key's with its fingerprint as the additional data, a webhook's with webhook:<uid>, an OIDC
// login's refresh token with oidc_refresh:<id>, an open upload's data key with the session's key.
func (c *conn) ListSealed(ctx context.Context, table string, afterID int64, limit int) ([]index.Sealed, error) {
	if table == index.SealedSessions {
		return c.sealedSessions(ctx, afterID, limit)
	}
	aad, value, err := sealedOf(table)
	if err != nil || limit < 1 {
		return nil, err
	}
	rows, err := getAll(ctx, c, table, func(s *index.Sealed) []any { return []any{&s.ID, &s.AAD, blobDest{&s.Value}} },
		"SELECT id, "+aad+", "+value+" FROM "+table+" WHERE id > ? ORDER BY id LIMIT ?", afterID, limit)
	for i := range rows {
		rows[i].Table = table
		switch table {
		case index.SealedWebhooks:
			rows[i].AAD = "webhook:" + rows[i].AAD
		case index.SealedRefresh:
			rows[i].AAD = "oidc_refresh:" + strconv.FormatInt(rows[i].ID, 10)
		}
	}
	return rows, err
}

// sealedSessions returns the data keys of the open uploads of the facade whose id is above
// afterID, by id, at most limit: sessions.files enc.key, nil for an upload that does not encrypt.
func (c *conn) sealedSessions(ctx context.Context, afterID int64, limit int) ([]index.Sealed, error) {
	if limit < 1 {
		return nil, nil
	}
	type upload struct {
		index.Sealed
		files json.RawMessage
	}
	rows, err := getAll(ctx, c, "sessions", func(u *upload) []any { return []any{&u.ID, &u.AAD, rawDest{&u.files}} },
		"SELECT id, key, files FROM sessions WHERE state = 'open' AND key LIKE 'blob-upload:%' AND id > ? ORDER BY id LIMIT ?",
		afterID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]index.Sealed, 0, len(rows))
	for _, u := range rows {
		var f struct {
			Enc *struct {
				Key []byte `json:"key"`
			} `json:"enc"`
		}
		if json.Unmarshal(u.files, &f) == nil && f.Enc != nil {
			u.Value = f.Enc.Key
		}
		u.Table = index.SealedSessions
		out = append(out, u.Sealed)
	}
	return out, nil
}

// Reseal writes sealed as the value of the row id of table where the row still holds old.
func (c *conn) Reseal(ctx context.Context, table string, id int64, old, sealed []byte) (bool, error) {
	_, value, err := sealedOf(table)
	if err != nil {
		return false, err
	}
	n, err := c.exec(ctx, fmt.Sprintf("%s id %d", table, id),
		"UPDATE "+table+" SET "+value+" = ? WHERE id = ? AND "+value+" = ?", blobArg(sealed), id, blobArg(old))
	return n == 1, err
}

const sealingKeyCols = "key_id, source, host, audit_id, set_at"

func sealingKeyDests(k *index.SealingKey) []any {
	return []any{&k.KeyID, &k.Source, &k.Host, nullIDDest{&k.AuditID}, timeDest{&k.SetAt}}
}

// GetSealingKey returns the row of sealing_key.
func (c *conn) GetSealingKey(ctx context.Context) (*index.SealingKey, error) {
	return getOne(ctx, c, "sealing_key", sealingKeyDests, "SELECT "+sealingKeyCols+" FROM sealing_key WHERE id = 1")
}

// PutSealingKey inserts the row when old is "" and there is none, or replaces the row that names
// old, in one statement, SetAt the database's time: a row that is not the one expected changes
// nothing and is the ErrConflict.
func (c *conn) PutSealingKey(ctx context.Context, k *index.SealingKey, old string) error {
	at, err := c.DatabaseTime(ctx)
	if err != nil {
		return err
	}
	k.SetAt = Normalize(at)
	args := []any{k.KeyID, k.Source, k.Host, idArg(k.AuditID), c.d.EncodeTime(k.SetAt)}
	var n int
	if old == "" {
		n, err = c.exec(ctx, "sealing_key",
			"INSERT INTO sealing_key (id, "+sealingKeyCols+") VALUES (1, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING", args...)
	} else {
		n, err = c.exec(ctx, "sealing_key",
			"UPDATE sealing_key SET key_id = ?, source = ?, host = ?, audit_id = ?, set_at = ? WHERE id = 1 AND key_id = ?",
			append(args, old)...)
	}
	if err == nil && n == 0 {
		err = fmt.Errorf("%w: sealing_key: another master key was named in between", index.ErrConflict)
	}
	return err
}

package sqlstore

import (
	"context"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Control plane: bootstrap_token, the one row of the token that makes the first admin.

const bootstrapCols = "hash, replica, created_at, expires_at, used_at"

func bootstrapDests(t *index.BootstrapToken) []any {
	return []any{&t.Hash, &t.Replica, timeDest{&t.CreatedAt}, timeDest{&t.ExpiresAt}, nullTimeDest{&t.UsedAt}}
}

// GetBootstrapToken returns the row of bootstrap_token.
func (c *conn) GetBootstrapToken(ctx context.Context) (*index.BootstrapToken, error) {
	return getOne(ctx, c, "bootstrap_token", bootstrapDests, "SELECT "+bootstrapCols+" FROM bootstrap_token WHERE id = 1")
}

// PutBootstrapToken inserts the row, or replaces the row whose hash is old, in one statement: a
// row that is not the one expected changes nothing and is the ErrConflict.
func (c *conn) PutBootstrapToken(ctx context.Context, t *index.BootstrapToken, old string) error {
	stamp(&t.CreatedAt, now())
	t.ExpiresAt, t.UsedAt = Normalize(t.ExpiresAt), nil
	args := []any{t.Hash, t.Replica, c.d.EncodeTime(t.CreatedAt), c.d.EncodeTime(t.ExpiresAt)}
	var (
		n   int
		err error
	)
	if old == "" {
		n, err = c.exec(ctx, "bootstrap_token",
			"INSERT INTO bootstrap_token (id, "+bootstrapCols+") VALUES (1, ?, ?, ?, ?, NULL) ON CONFLICT (id) DO NOTHING", args...)
	} else {
		n, err = c.exec(ctx, "bootstrap_token",
			"UPDATE bootstrap_token SET hash = ?, replica = ?, created_at = ?, expires_at = ?, used_at = NULL WHERE id = 1 AND hash = ?",
			append(args, old)...)
	}
	if err == nil && n == 0 {
		err = fmt.Errorf("%w: bootstrap_token: another token was made in between", index.ErrConflict)
	}
	return err
}

// UseBootstrapToken sets used_at of the row when it is the token of hash, unused and not
// expired at at; an UPDATE, so that it holds the row in a transaction.
func (c *conn) UseBootstrapToken(ctx context.Context, hash string, at time.Time) error {
	at = Normalize(at)
	return c.execOne(ctx, "bootstrap_token that is valid",
		"UPDATE bootstrap_token SET used_at = ? WHERE id = 1 AND hash = ? AND used_at IS NULL AND expires_at > ?",
		c.d.EncodeTime(at), hash, c.d.EncodeTime(at))
}

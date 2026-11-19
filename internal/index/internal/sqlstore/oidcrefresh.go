package sqlstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The refresh of OIDC logins (docs/spec/rbac.md §8, Asking the IdP again, #1282;
// docs/spec/index-schema.md, oidc_refresh, coordination): a row per login that keeps a refresh
// token, sealed, joined to the login's token at its mint, claimed by one replica at a time.

const oidcRefreshCols = "id, login_id, token_id, identity_id, refresh_enc, created_at, refreshed_at, next_at, failures," +
	" claimed_by, claimed_until"

func oidcRefreshDests(r *index.OIDCRefresh) []any {
	return []any{&r.ID, nullIDDest{&r.LoginID}, nullIDDest{&r.TokenID}, &r.IdentityID, blobDest{&r.Sealed},
		timeDest{&r.CreatedAt}, timeDest{&r.RefreshedAt}, timeDest{&r.NextAt}, &r.Failures, textDest{&r.ClaimedBy},
		nullTimeDest{&r.ClaimedUntil}}
}

// CreateOIDCRefresh inserts r, CreatedAt now when zero and RefreshedAt CreatedAt when zero.
func (c *conn) CreateOIDCRefresh(ctx context.Context, r *index.OIDCRefresh) error {
	created := stamp(&r.CreatedAt, now())
	stamp(&r.RefreshedAt, created)
	r.NextAt, r.ClaimedUntil = Normalize(r.NextAt), stampNull(r.ClaimedUntil)
	return c.scanRow(ctx, fmt.Sprintf("oidc_refresh of identity %d", r.IdentityID),
		"INSERT INTO oidc_refresh (login_id, token_id, identity_id, refresh_enc, created_at, refreshed_at, next_at, failures,"+
			" claimed_by, claimed_until) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
		[]any{idArg(r.LoginID), idArg(r.TokenID), r.IdentityID, blobArg(r.Sealed), c.d.EncodeTime(r.CreatedAt),
			c.d.EncodeTime(r.RefreshedAt), c.d.EncodeTime(r.NextAt), r.Failures, textArg(r.ClaimedBy),
			c.d.encodeNullTime(r.ClaimedUntil)},
		&r.ID)
}

// JoinOIDCRefresh sets the token and the due time of the row of loginID that has no token yet,
// and clears its login, in one UPDATE.
func (c *conn) JoinOIDCRefresh(ctx context.Context, loginID, tokenID int64, nextAt time.Time) error {
	return c.execOne(ctx, fmt.Sprintf("oidc_refresh of login %d", loginID),
		"UPDATE oidc_refresh SET token_id = ?, login_id = NULL, next_at = ? WHERE login_id = ? AND token_id IS NULL",
		tokenID, c.d.EncodeTime(Normalize(nextAt)), loginID)
}

// ClaimOIDCRefresh reads the ids of at most limit due rows whose claim is free or ended, by
// next_at (on Postgres FOR UPDATE SKIP LOCKED, so that replicas that claim at once take other
// rows), then claims them in one UPDATE that holds only where each row is still due and free:
// a compare-and-set, which keeps a row from two replicas on any engine, and returns the rows as
// it left them.
func (c *conn) ClaimOIDCRefresh(ctx context.Context, replica string, at, until time.Time, limit int) ([]index.OIDCRefresh, error) {
	if limit < 1 {
		return nil, nil
	}
	at, until = Normalize(at), Normalize(until)
	due := "next_at <= ? AND (claimed_until IS NULL OR claimed_until <= ?)"
	query := "SELECT id FROM oidc_refresh WHERE " + due + " ORDER BY next_at, id LIMIT ?"
	if c.d.Name != index.DialectSQLite {
		query += " FOR UPDATE SKIP LOCKED"
	}
	var claimed []index.OIDCRefresh
	err := c.withTx(ctx, func(c *conn) error {
		claimed = nil // a transaction the database aborts runs again
		ids, err := getAll(ctx, c, "oidc_refresh due", func(id *int64) []any { return []any{id} },
			query, c.d.EncodeTime(at), c.d.EncodeTime(at), limit)
		if err != nil || len(ids) == 0 {
			return err
		}
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")
		args := []any{replica, c.d.EncodeTime(until)}
		for _, id := range ids {
			args = append(args, id)
		}
		args = append(args, c.d.EncodeTime(at), c.d.EncodeTime(at))
		claimed, err = getAll(ctx, c, "oidc_refresh due", oidcRefreshDests,
			"UPDATE oidc_refresh SET claimed_by = ?, claimed_until = ? WHERE id IN ("+marks+") AND "+due+
				" RETURNING "+oidcRefreshCols, args...)
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(claimed, func(a, b index.OIDCRefresh) int {
		if n := a.NextAt.Compare(b.NextAt); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return claimed, nil
}

// FinishOIDCRefresh stores the outcome of a refresh and frees the claim, in one UPDATE that holds
// only while the row's claim is replica's and ends at r.ClaimedUntil.
func (c *conn) FinishOIDCRefresh(ctx context.Context, r *index.OIDCRefresh, replica string) error {
	what := fmt.Sprintf("oidc_refresh id %d", r.ID)
	if r.ClaimedUntil == nil || replica == "" {
		return fmt.Errorf("%w: %s is not claimed", index.ErrConflict, what)
	}
	r.RefreshedAt, r.NextAt = Normalize(r.RefreshedAt), Normalize(r.NextAt)
	set := "refreshed_at = ?, next_at = ?, failures = ?, claimed_by = NULL, claimed_until = NULL"
	args := []any{c.d.EncodeTime(r.RefreshedAt), c.d.EncodeTime(r.NextAt), r.Failures}
	if r.Sealed != nil {
		set = "refresh_enc = ?, " + set
		args = append([]any{blobArg(r.Sealed)}, args...)
	}
	args = append(args, r.ID, replica, c.d.EncodeTime(Normalize(*r.ClaimedUntil)))
	n, err := c.exec(ctx, what, "UPDATE oidc_refresh SET "+set+" WHERE id = ? AND claimed_by = ? AND claimed_until = ?", args...)
	switch {
	case err != nil:
		return err
	case n == 0:
		return fmt.Errorf("%w: %s: the claim of %s is not the row's any more", index.ErrConflict, what, replica)
	}
	r.ClaimedBy, r.ClaimedUntil = "", nil
	return nil
}

// DeleteOIDCRefresh deletes the row id.
func (c *conn) DeleteOIDCRefresh(ctx context.Context, id int64) error {
	return c.execOne(ctx, fmt.Sprintf("oidc_refresh id %d", id), "DELETE FROM oidc_refresh WHERE id = ?", id)
}

// ClearOIDCRefresh deletes every row.
func (c *conn) ClearOIDCRefresh(ctx context.Context) (int, error) {
	return c.exec(ctx, "oidc_refresh", "DELETE FROM oidc_refresh")
}

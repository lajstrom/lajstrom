package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Coordination: fetch_claims, the lock of a proxy fetch on one artifact across replicas
// (ADR 0013).

const fetchClaimCols = "id, repo_id, package, tag, principal, replica, claimed_at, heartbeat_at"

func fetchClaimDests(cl *index.FetchClaim) []any {
	return []any{&cl.ID, &cl.RepoID, &cl.Package, &cl.Tag, &cl.Principal, &cl.Replica, timeDest{&cl.ClaimedAt},
		timeDest{&cl.HeartbeatAt}}
}

// fetchClaimWhat names the claim of a fetch for errors.
func fetchClaimWhat(repoID int64, pkg, tag string) string {
	return fmt.Sprintf("fetch_claims %q tag %q in repo %d", pkg, tag, repoID)
}

// GetFetchClaim returns the claim of a fetch on tag of package pkg in a repo.
func (c *conn) GetFetchClaim(ctx context.Context, repoID int64, pkg, tag string) (*index.FetchClaim, error) {
	return getOne(ctx, c, fetchClaimWhat(repoID, pkg, tag), fetchClaimDests,
		"SELECT "+fetchClaimCols+" FROM fetch_claims WHERE repo_id = ? AND package = ? AND tag = ?", repoID, pkg, tag)
}

// fetchClaimAttempts bounds how often ClaimFetch tries to insert: a stale claim deleted takes
// one attempt more, and a row that changed between two statements one more again.
const fetchClaimAttempts = 3

// ClaimFetch inserts the claim in one transaction: INSERT … ON CONFLICT DO NOTHING, and when
// that inserts nothing the row of the key decides. A fresh claim is the ErrConflict; a stale one
// is deleted, only as it was read, and the insert tried again, so that the claim gets a new id.
func (c *conn) ClaimFetch(ctx context.Context, cl *index.FetchClaim, staleBefore time.Time) error {
	stamp(&cl.ClaimedAt, now())
	stamp(&cl.HeartbeatAt, cl.ClaimedAt)
	staleBefore = Normalize(staleBefore)
	what := fetchClaimWhat(cl.RepoID, cl.Package, cl.Tag)
	return c.withTx(ctx, func(c *conn) error {
		for range fetchClaimAttempts {
			inserted, err := c.insertID(ctx, what,
				"INSERT INTO fetch_claims (repo_id, package, tag, principal, replica, claimed_at, heartbeat_at)"+
					" VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (repo_id, package, tag) DO NOTHING RETURNING id",
				[]any{cl.RepoID, cl.Package, cl.Tag, cl.Principal, cl.Replica, c.d.EncodeTime(cl.ClaimedAt),
					c.d.EncodeTime(cl.HeartbeatAt)},
				&cl.ID)
			if inserted || err != nil {
				return err
			}
			existing, err := c.GetFetchClaim(ctx, cl.RepoID, cl.Package, cl.Tag)
			switch {
			case errors.Is(err, index.ErrNotFound): // released in between: insert again
				continue
			case err != nil:
				return err
			case !existing.HeartbeatAt.Before(staleBefore):
				return fmt.Errorf("%w: %s: held by %s for %s since %s", index.ErrConflict, what, existing.Replica,
					existing.Principal, existing.ClaimedAt.Format(time.RFC3339))
			}
			if _, err := c.exec(ctx, what, "DELETE FROM fetch_claims WHERE id = ? AND heartbeat_at < ?",
				existing.ID, c.d.EncodeTime(staleBefore)); err != nil {
				return err
			}
		}
		return fmt.Errorf("%w: %s changed under every attempt to claim it", index.ErrConflict, what)
	})
}

// HeartbeatFetchClaim sets HeartbeatAt of the claim.
func (c *conn) HeartbeatFetchClaim(ctx context.Context, id int64, at time.Time) error {
	return c.execOne(ctx, fmt.Sprintf("fetch_claims id %d", id), "UPDATE fetch_claims SET heartbeat_at = ? WHERE id = ?",
		c.d.EncodeTime(at), id)
}

// ReleaseFetchClaim removes the claim.
func (c *conn) ReleaseFetchClaim(ctx context.Context, id int64) error {
	return c.execOne(ctx, fmt.Sprintf("fetch_claims id %d", id), "DELETE FROM fetch_claims WHERE id = ?", id)
}

// ExpireFetchClaims removes the claims whose last beat is before before.
func (c *conn) ExpireFetchClaims(ctx context.Context, before time.Time) (int, error) {
	return c.exec(ctx, "fetch_claims", "DELETE FROM fetch_claims WHERE heartbeat_at < ?", c.d.EncodeTime(before))
}

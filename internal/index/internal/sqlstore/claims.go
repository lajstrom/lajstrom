package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Coordination: tag_claims, the lock that serialises the writers of one tag.

const tagClaimCols = "id, repo_id, package, tag, digest, principal, claimed_at"

func tagClaimDests(cl *index.TagClaim) []any {
	return []any{&cl.ID, &cl.RepoID, &cl.Package, &cl.Tag, (*string)(&cl.Digest), &cl.Principal, timeDest{&cl.ClaimedAt}}
}

// claimWhat names the claim on a tag for errors.
func claimWhat(repoID int64, pkg, tag string) string {
	return fmt.Sprintf("tag_claims %q tag %q in repo %d", pkg, tag, repoID)
}

// GetTagClaim returns the claim on tag of package pkg in a repo.
func (c *conn) GetTagClaim(ctx context.Context, repoID int64, pkg, tag string) (*index.TagClaim, error) {
	return getOne(ctx, c, claimWhat(repoID, pkg, tag), tagClaimDests,
		"SELECT "+tagClaimCols+" FROM tag_claims WHERE repo_id = ? AND package = ? AND tag = ?", repoID, pkg, tag)
}

// claimAttempts bounds how often ClaimTag applies the claim rule. The second attempt follows a
// row that changed under the first: a claim released, or taken over, between two statements.
const claimAttempts = 2

// ClaimTag applies the claim rule in one transaction, like UpsertVersion: INSERT … ON CONFLICT
// DO NOTHING, and when that inserts nothing the row of the key decides. The same digest shares
// the claim; another digest takes over a stale claim with an UPDATE that only matches the row
// as it was read, and gets a *index.TagConflictError for a fresh one. When the UPDATE matches
// nothing, or the row is gone, another writer changed it in between and the rule is applied
// again, so the loser of a race gets the TagConflictError, never a driver error. ClaimedAt is
// set to now when zero.
func (c *conn) ClaimTag(ctx context.Context, cl *index.TagClaim, staleBefore time.Time) error {
	stamp(&cl.ClaimedAt, now())
	staleBefore = Normalize(staleBefore)
	return c.withTx(ctx, func(c *conn) error {
		var seen *index.TagClaim
		for range claimAttempts {
			done, existing, err := c.claimOnce(ctx, cl, staleBefore)
			if done || err != nil {
				return err
			}
			if existing != nil {
				seen = existing
			}
		}
		if seen != nil {
			return &index.TagConflictError{Tag: cl.Tag, Existing: seen.Digest}
		}
		return fmt.Errorf("%w: %s changed under every attempt to claim it", index.ErrConflict,
			claimWhat(cl.RepoID, cl.Package, cl.Tag))
	})
}

// claimOnce applies the claim rule once. done reports that the rule decided (with err nil or
// not); otherwise the row changed in between, and existing is the row as it was read, if any.
func (c *conn) claimOnce(ctx context.Context, cl *index.TagClaim, staleBefore time.Time) (done bool, existing *index.TagClaim, err error) {
	what := claimWhat(cl.RepoID, cl.Package, cl.Tag)
	inserted, err := c.insertID(ctx, what,
		"INSERT INTO tag_claims (repo_id, package, tag, digest, principal, claimed_at) VALUES (?, ?, ?, ?, ?, ?)"+
			" ON CONFLICT (repo_id, package, tag) DO NOTHING RETURNING id",
		[]any{cl.RepoID, cl.Package, cl.Tag, string(cl.Digest), cl.Principal, c.d.EncodeTime(cl.ClaimedAt)},
		&cl.ID)
	if inserted || err != nil {
		return true, nil, err
	}
	existing, err = c.GetTagClaim(ctx, cl.RepoID, cl.Package, cl.Tag)
	switch {
	case errors.Is(err, index.ErrNotFound): // released in between: insert again
		return false, nil, nil
	case err != nil:
		return true, nil, err
	case existing.Digest == cl.Digest:
		*cl = *existing
		return true, existing, nil
	case !existing.ClaimedAt.Before(staleBefore):
		return true, existing, &index.TagConflictError{Tag: cl.Tag, Existing: existing.Digest}
	}
	n, err := c.exec(ctx, what,
		"UPDATE tag_claims SET digest = ?, principal = ?, claimed_at = ? WHERE id = ? AND digest <> ? AND claimed_at < ?",
		string(cl.Digest), cl.Principal, c.d.EncodeTime(cl.ClaimedAt), existing.ID, string(cl.Digest), c.d.EncodeTime(staleBefore))
	switch {
	case err != nil:
		return true, existing, err
	case n == 0: // taken over or released in between
		return false, existing, nil
	}
	cl.ID = existing.ID
	return true, existing, nil
}

// ReleaseTagClaim removes the claim.
func (c *conn) ReleaseTagClaim(ctx context.Context, id int64) error {
	return c.execOne(ctx, fmt.Sprintf("tag_claims id %d", id), "DELETE FROM tag_claims WHERE id = ?", id)
}

// ExpireTagClaims removes the claims made before before.
func (c *conn) ExpireTagClaims(ctx context.Context, before time.Time) (int, error) {
	return c.exec(ctx, "tag_claims", "DELETE FROM tag_claims WHERE claimed_at < ?", c.d.EncodeTime(before))
}

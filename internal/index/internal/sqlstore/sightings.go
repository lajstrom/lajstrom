package sqlstore

import (
	"context"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Operational: prune_sightings, the registry's answers MANIFEST_UNKNOWN that a removal by
// reindex waits on (#446).

// RecordSighting inserts the sighting or sets LastAt of its row, and fills FirstAt from the
// row, in one statement.
func (c *conn) RecordSighting(ctx context.Context, s *index.Sighting) error {
	stamp(&s.LastAt, now())
	what := fmt.Sprintf("prune_sightings %s@%s in repo %d", s.Path, s.Digest, s.RepoID)
	at := c.d.EncodeTime(s.LastAt)
	return c.scanRow(ctx, what,
		"INSERT INTO prune_sightings (repo_id, path, digest, first_at, last_at) VALUES (?, ?, ?, ?, ?)"+
			" ON CONFLICT (repo_id, path, digest) DO UPDATE SET last_at = excluded.last_at RETURNING first_at",
		[]any{s.RepoID, s.Path, s.Digest.String(), at, at}, timeDest{&s.FirstAt})
}

// ClearSightings removes the sightings of a repo last recorded before before.
func (c *conn) ClearSightings(ctx context.Context, repoID int64, before time.Time) (int, error) {
	return c.exec(ctx, fmt.Sprintf("prune_sightings in repo %d", repoID),
		"DELETE FROM prune_sightings WHERE repo_id = ? AND last_at < ?", repoID, c.d.EncodeTime(before))
}

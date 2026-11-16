package sqlstore

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The summaries of the rows before migration 0029 (docs/spec/index-schema.md, The summaries of
// the rows before 0029).

// summaryArg is the value of versions.summary of v: its Summary, also when it is empty and the
// row keeps the Metadata it is read from; NULL for neither.
func summaryArg(v *index.Version) any {
	if v.Summary == "" && len(v.Metadata) == 0 {
		return nil
	}
	return v.Summary
}

// ListVersionsWithoutSummary returns at most limit versions rows after the id afterID, in id
// order, whose summary is NULL and whose metadata is not.
func (c *conn) ListVersionsWithoutSummary(ctx context.Context, afterID int64, limit int) ([]index.Version, error) {
	if limit < 1 {
		return []index.Version{}, nil
	}
	return getAll(ctx, c, fmt.Sprintf("versions without a summary after id %d", afterID), versionDests,
		"SELECT "+versionCols+" FROM versions WHERE id > ? AND summary IS NULL AND metadata IS NOT NULL ORDER BY id LIMIT ?",
		afterID, limit)
}

// SetVersionSummaries writes each summary to its row while the row's summary is NULL and its
// metadata is not, one statement per row in one transaction, and returns how many rows it wrote.
func (c *conn) SetVersionSummaries(ctx context.Context, summaries map[int64]string) (int, error) {
	written := 0
	err := c.withTx(ctx, func(c *conn) error {
		written = 0
		for _, id := range slices.Sorted(maps.Keys(summaries)) {
			n, err := c.exec(ctx, fmt.Sprintf("the summary of versions id %d", id),
				"UPDATE versions SET summary = ? WHERE id = ? AND summary IS NULL AND metadata IS NOT NULL", summaries[id], id)
			if err != nil {
				return err
			}
			written += n
		}
		return nil
	})
	return written, err
}

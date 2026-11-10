package sqlstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The npm journal (ADR 0012): journal, one row per entry in the registry, and journal_state,
// their fold. Both refer to packages with ON DELETE CASCADE, so a package's rows go with it.

const journalCols = "id, package_id, seq, tag, digest, artifact_type, attrs, created_at, indexed_at"

func journalDests(e *index.JournalEntry) []any {
	return []any{&e.ID, &e.PackageID, &e.Seq, &e.Tag, (*string)(&e.Digest), &e.ArtifactType,
		jsonDest[map[string]string]{&e.Attrs}, timeDest{&e.CreatedAt}, timeDest{&e.IndexedAt}}
}

const journalStateCols = "package_id, kind, key, value, seq"

func journalStateDests(s *index.JournalState) []any {
	return []any{&s.PackageID, &s.Kind, &s.Key, &s.Value, &s.Seq}
}

// ListJournal returns the journal entries of a package ordered by Seq.
func (c *conn) ListJournal(ctx context.Context, packageID int64) ([]index.JournalEntry, error) {
	return getAll(ctx, c, fmt.Sprintf("journal of package %d", packageID), journalDests,
		"SELECT "+journalCols+" FROM journal WHERE package_id = ? ORDER BY seq", packageID)
}

// ListJournalState returns the journal_state rows of a package ordered by kind and key.
func (c *conn) ListJournalState(ctx context.Context, packageID int64) ([]index.JournalState, error) {
	return getAll(ctx, c, fmt.Sprintf("journal_state of package %d", packageID), journalStateDests,
		"SELECT "+journalStateCols+" FROM journal_state WHERE package_id = ? ORDER BY kind, key", packageID)
}

// UpsertJournal inserts e, or sees the row of (PackageID, Seq) when it has e's tag and digest:
// its IndexedAt becomes e's and e is filled from it. Another tag or digest under the Seq, or e's
// tag under another Seq, is a *index.TagConflictError, also for the loser of a race between two
// inserts of one entry. CreatedAt and IndexedAt are set to now when zero.
func (c *conn) UpsertJournal(ctx context.Context, e *index.JournalEntry) error {
	ts := now()
	stamp(&e.CreatedAt, ts)
	stamp(&e.IndexedAt, ts)
	return c.withTx(ctx, func(c *conn) error {
		if found, err := c.seeJournal(ctx, e); found || err != nil {
			return err
		}
		inserted, err := c.insertID(ctx, fmt.Sprintf("journal %q of package %d", e.Tag, e.PackageID),
			"INSERT INTO journal (package_id, seq, tag, digest, artifact_type, attrs, created_at, indexed_at)"+
				" VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING RETURNING id",
			[]any{e.PackageID, e.Seq, e.Tag, string(e.Digest), e.ArtifactType, attrsArg(e.Attrs),
				c.d.EncodeTime(e.CreatedAt), c.d.EncodeTime(e.IndexedAt)},
			&e.ID)
		if inserted || err != nil {
			return err
		}
		if found, err := c.seeJournal(ctx, e); found || err != nil {
			return err
		}
		return fmt.Errorf("index: journal %q of package %d: the insert found the entry taken and then gone", e.Tag, e.PackageID)
	})
}

// seeJournal applies the rule of UpsertJournal to the rows that hold e's Seq or e's tag and
// reports whether there is one: the same entry is seen (IndexedAt, e filled), another is a
// *index.TagConflictError.
func (c *conn) seeJournal(ctx context.Context, e *index.JournalEntry) (bool, error) {
	rows, err := getAll(ctx, c, fmt.Sprintf("journal %q of package %d", e.Tag, e.PackageID), journalDests,
		"SELECT "+journalCols+" FROM journal WHERE package_id = ? AND (seq = ? OR tag = ?) ORDER BY seq",
		e.PackageID, e.Seq, e.Tag)
	switch {
	case err != nil:
		return false, err
	case len(rows) == 0:
		return false, nil
	}
	existing := rows[0]
	if len(rows) > 1 || existing.Seq != e.Seq || existing.Tag != e.Tag || existing.Digest != e.Digest {
		return true, &index.TagConflictError{Tag: e.Tag, Existing: existing.Digest}
	}
	at := e.IndexedAt
	*e = existing
	e.IndexedAt = at
	return true, c.execOne(ctx, fmt.Sprintf("journal id %d", e.ID), "UPDATE journal SET indexed_at = ? WHERE id = ?",
		c.d.EncodeTime(at), e.ID)
}

// TouchJournal sets IndexedAt of the entries with the given ids, in statements of at most
// touchBatch ids; ids that do not exist are skipped.
func (c *conn) TouchJournal(ctx context.Context, ids []int64, at time.Time) error {
	return c.withTx(ctx, func(c *conn) error {
		for chunk := range slices.Chunk(ids, touchBatch) {
			args := make([]any, 0, len(chunk)+1)
			args = append(args, c.d.EncodeTime(at))
			for _, id := range chunk {
				args = append(args, id)
			}
			marks := strings.Repeat(", ?", len(chunk))[2:]
			if _, err := c.exec(ctx, fmt.Sprintf("journal %d to touch", len(chunk)),
				"UPDATE journal SET indexed_at = ? WHERE id IN ("+marks+")", args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteJournal removes the journal entry.
func (c *conn) DeleteJournal(ctx context.Context, id int64) error {
	return c.execOne(ctx, fmt.Sprintf("journal id %d", id), "DELETE FROM journal WHERE id = ?", id)
}

// ReplaceJournalState replaces the journal_state rows of the package with state, in order, and
// fills their PackageID. The package must exist (index.ErrNotFound); two rows of one kind and
// key are index.ErrConflict.
func (c *conn) ReplaceJournalState(ctx context.Context, packageID int64, state []index.JournalState) error {
	return c.withTx(ctx, func(c *conn) error {
		what := fmt.Sprintf("packages id %d", packageID)
		var one int
		if err := c.scanRow(ctx, what, "SELECT 1 FROM packages WHERE id = ?", []any{packageID}, &one); err != nil {
			return err
		}
		if _, err := c.exec(ctx, "journal_state of "+what, "DELETE FROM journal_state WHERE package_id = ?", packageID); err != nil {
			return err
		}
		for i := range state {
			s := &state[i]
			s.PackageID = packageID
			_, err := c.exec(ctx, fmt.Sprintf("journal_state %s %q of package %d", s.Kind, s.Key, packageID),
				"INSERT INTO journal_state (package_id, kind, key, value, seq) VALUES (?, ?, ?, ?, ?)",
				packageID, s.Kind, s.Key, s.Value, s.Seq)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

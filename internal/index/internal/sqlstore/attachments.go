package sqlstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The attachments of a version (#89; docs/spec/index-schema.md, Attachments): one row per layer
// of a referrer of the version's manifest, with its label. They refer to versions with ON DELETE
// CASCADE, so a version's rows go with it.

const attachmentCols = "id, version_id, digest, tag, layer, media_type, artifact_type, kind, predicate_type, size, annotations," +
	" tool, tool_version, database, input, verification, signed_by, reason, trust, created_at, indexed_at"

func attachmentDests(a *index.Attachment) []any {
	return []any{&a.ID, &a.VersionID, (*string)(&a.Digest), &a.Tag, (*string)(&a.Layer), &a.MediaType, &a.ArtifactType, &a.Kind,
		&a.PredicateType, &a.Size, jsonDest[map[string]string]{&a.Annotations}, &a.Tool, &a.ToolVersion, &a.Database,
		(*string)(&a.Input), &a.Verification, textDest{&a.SignedBy}, &a.Reason, &a.Trust, nullTimeDest{&a.CreatedAt},
		timeDest{&a.IndexedAt}}
}

// ListAttachments returns the rows of the versions ids, newest first: CreatedAt descending, the
// rows without one after the others, then by digest and layer. The ids go in statements of at
// most touchBatch, and the rows are merged in that order.
func (c *conn) ListAttachments(ctx context.Context, versionIDs []int64) ([]index.Attachment, error) {
	all := []index.Attachment{}
	for chunk := range slices.Chunk(versionIDs, touchBatch) {
		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := getAll(ctx, c, fmt.Sprintf("attachments of %d versions", len(chunk)), attachmentDests,
			"SELECT "+attachmentCols+" FROM attachments WHERE version_id IN ("+strings.Repeat(", ?", len(chunk))[2:]+")", args...)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
	}
	slices.SortStableFunc(all, compareAttachments)
	return all, nil
}

// compareAttachments orders attachments newest first, those without a time last, then by digest
// and layer.
func compareAttachments(a, b index.Attachment) int {
	switch {
	case a.CreatedAt != nil && b.CreatedAt == nil:
		return -1
	case a.CreatedAt == nil && b.CreatedAt != nil:
		return 1
	case a.CreatedAt != nil && !a.CreatedAt.Equal(*b.CreatedAt):
		return b.CreatedAt.Compare(*a.CreatedAt)
	}
	if n := strings.Compare(string(a.Digest), string(b.Digest)); n != 0 {
		return n
	}
	return strings.Compare(string(a.Layer), string(b.Layer))
}

// ReplaceAttachments makes the rows of the referrer d of the version exactly rows, in one
// transaction, and fills their ID, VersionID and Digest; a row with a Tag also removes the
// version's rows under that tag of another referrer. IndexedAt is now when zero. A version that
// does not exist is index.ErrNotFound, and nothing is written.
func (c *conn) ReplaceAttachments(ctx context.Context, versionID int64, d digest.Digest, rows []index.Attachment) error {
	ts := now()
	return c.withTx(ctx, func(c *conn) error {
		what := fmt.Sprintf("versions id %d", versionID)
		var one int
		if err := c.scanRow(ctx, what, "SELECT 1 FROM versions WHERE id = ?", []any{versionID}, &one); err != nil {
			return err
		}
		if _, err := c.exec(ctx, fmt.Sprintf("attachments %s of %s", d, what),
			"DELETE FROM attachments WHERE version_id = ? AND digest = ?", versionID, string(d)); err != nil {
			return err
		}
		tags := map[string]bool{}
		for i := range rows {
			a := &rows[i]
			if a.Tag != "" && !tags[a.Tag] {
				tags[a.Tag] = true
				if _, err := c.exec(ctx, fmt.Sprintf("attachments under %q of %s", a.Tag, what),
					"DELETE FROM attachments WHERE version_id = ? AND tag = ?", versionID, a.Tag); err != nil {
					return err
				}
			}
			a.VersionID, a.Digest = versionID, d
			stamp(&a.IndexedAt, ts)
			a.CreatedAt = stampNull(a.CreatedAt)
			err := c.scanRow(ctx, fmt.Sprintf("attachments %s layer %q of %s", d, a.Layer, what),
				"INSERT INTO attachments (version_id, digest, tag, layer, media_type, artifact_type, kind, predicate_type, size,"+
					" annotations, tool, tool_version, database, input, verification, signed_by, reason, trust, created_at, indexed_at)"+
					" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
				[]any{versionID, string(d), a.Tag, string(a.Layer), a.MediaType, a.ArtifactType, a.Kind, a.PredicateType, a.Size,
					attrsArg(a.Annotations), a.Tool, a.ToolVersion, a.Database, string(a.Input), a.Verification, textArg(a.SignedBy),
					a.Reason, a.Trust, c.d.encodeNullTime(a.CreatedAt), c.d.EncodeTime(a.IndexedAt)},
				&a.ID)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// TouchAttachments sets IndexedAt of the rows with the given ids, in statements of at most
// touchBatch ids; ids that do not exist are skipped.
func (c *conn) TouchAttachments(ctx context.Context, ids []int64, at time.Time) error {
	return c.withTx(ctx, func(c *conn) error {
		for chunk := range slices.Chunk(ids, touchBatch) {
			args := make([]any, 0, len(chunk)+1)
			args = append(args, c.d.EncodeTime(at))
			for _, id := range chunk {
				args = append(args, id)
			}
			if _, err := c.exec(ctx, fmt.Sprintf("attachments %d to touch", len(chunk)),
				"UPDATE attachments SET indexed_at = ? WHERE id IN ("+strings.Repeat(", ?", len(chunk))[2:]+")", args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteAttachments removes the rows of the referrer d among the versions of a repo, and returns
// how many.
func (c *conn) DeleteAttachments(ctx context.Context, repoID int64, d digest.Digest) (int, error) {
	return c.exec(ctx, fmt.Sprintf("attachments %s in repo %d", d, repoID),
		"DELETE FROM attachments WHERE digest = ? AND version_id IN"+
			" (SELECT v.id FROM versions v JOIN packages p ON p.id = v.package_id WHERE p.repo_id = ?)",
		string(d), repoID)
}

// SweepAttachments removes the rows of a version whose IndexedAt is before seenBefore, and
// returns how many.
func (c *conn) SweepAttachments(ctx context.Context, versionID int64, seenBefore time.Time) (int, error) {
	return c.exec(ctx, fmt.Sprintf("attachments of version %d not seen since %s", versionID, seenBefore.Format(time.RFC3339)),
		"DELETE FROM attachments WHERE version_id = ? AND indexed_at < ?", versionID, c.d.EncodeTime(seenBefore))
}

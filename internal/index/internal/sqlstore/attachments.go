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
// does not exist is index.ErrNotFound, and nothing is written. A row with a Verdict gets its
// verdicts row, and the policy generation moves when a verdicts row is written or goes with the
// rows replaced.
func (c *conn) ReplaceAttachments(ctx context.Context, versionID int64, d digest.Digest, rows []index.Attachment) error {
	ts := now()
	return c.withTx(ctx, func(c *conn) error {
		what := fmt.Sprintf("versions id %d", versionID)
		var repoID int64
		if err := c.scanRow(ctx, what, "SELECT p.repo_id FROM versions v JOIN packages p ON p.id = v.package_id WHERE v.id = ?",
			[]any{versionID}, &repoID); err != nil {
			return err
		}
		verdicts, err := c.attachedVerdicts(ctx, fmt.Sprintf("attachments %s of %s", d, what),
			"SELECT id FROM attachments WHERE version_id = ? AND digest = ?", versionID, string(d))
		if err != nil {
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
				tagged, err := c.attachedVerdicts(ctx, fmt.Sprintf("attachments under %q of %s", a.Tag, what),
					"SELECT id FROM attachments WHERE version_id = ? AND tag = ?", versionID, a.Tag)
				if err != nil {
					return err
				}
				verdicts = verdicts || tagged
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
			if a.Verdict != nil {
				v := a.Verdict
				v.RepoID, v.AttachmentID, v.Referrer, v.Layer, v.CreatedAt = repoID, a.ID, d, a.Layer, a.CreatedAt
				v.IndexedAt = a.IndexedAt
				if err := c.insertVerdict(ctx, v); err != nil {
					return err
				}
				verdicts = true
			}
		}
		if !verdicts {
			return nil
		}
		return c.nextPolicyGeneration(ctx)
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
// The policy generation moves when a verdicts row goes with them.
func (c *conn) DeleteAttachments(ctx context.Context, repoID int64, d digest.Digest) (int, error) {
	what := fmt.Sprintf("attachments %s in repo %d", d, repoID)
	sel := "SELECT a.id FROM attachments a JOIN versions v ON v.id = a.version_id JOIN packages p ON p.id = v.package_id" +
		" WHERE a.digest = ? AND p.repo_id = ?"
	var n int
	err := c.withTx(ctx, func(c *conn) error {
		verdicts, err := c.attachedVerdicts(ctx, what, sel, string(d), repoID)
		if err != nil {
			return err
		}
		if n, err = c.exec(ctx, what, "DELETE FROM attachments WHERE id IN ("+sel+")", string(d), repoID); err != nil || !verdicts {
			return err
		}
		return c.nextPolicyGeneration(ctx)
	})
	return n, err
}

// SweepAttachments removes the rows of a version whose IndexedAt is before seenBefore, and
// returns how many.
func (c *conn) SweepAttachments(ctx context.Context, versionID int64, seenBefore time.Time) (int, error) {
	return c.exec(ctx, fmt.Sprintf("attachments of version %d not seen since %s", versionID, seenBefore.Format(time.RFC3339)),
		"DELETE FROM attachments WHERE version_id = ? AND indexed_at < ?", versionID, c.d.EncodeTime(seenBefore))
}

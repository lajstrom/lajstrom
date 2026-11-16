package sqlstore

import (
	"context"
	"fmt"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The mapping of an encrypted repo (#114; docs/spec/index-schema.md): per OCI repository of a
// repo, the plaintext digest of a blob or a manifest, the stored digest, and the wrapping of the
// blob's data key. The rows refer to repos with ON DELETE CASCADE.

const blobMapCols = "id, repo_id, path, kind, plain_digest, stored_digest, size, key_fingerprint, wrapped, pubopts, indexed_at"

func blobMapDests(b *index.BlobMap) []any {
	return []any{&b.ID, &b.RepoID, &b.Path, &b.Kind, (*string)(&b.Plain), (*string)(&b.Stored), &b.Size, &b.KeyFingerprint,
		&b.Wrapped, &b.PubOpts, timeDest{&b.IndexedAt}}
}

// GetBlobMap returns the row of the stored digest d at the package path of the repo.
func (c *conn) GetBlobMap(ctx context.Context, repoID int64, path string, d digest.Digest) (*index.BlobMap, error) {
	return getOne(ctx, c, fmt.Sprintf("blob_map %s at %q of repo %d", d, path, repoID), blobMapDests,
		"SELECT "+blobMapCols+" FROM blob_map WHERE repo_id = ? AND path = ? AND stored_digest = ?", repoID, path, string(d))
}

// FindBlobMap returns the rows of the plaintext digest d at the package path, of kind or of both
// when kind is "", the newest first.
func (c *conn) FindBlobMap(ctx context.Context, repoID int64, path string, d digest.Digest, kind string) ([]index.BlobMap, error) {
	w := where{}
	w.add("repo_id = ?", repoID)
	w.add("path = ?", path)
	w.add("plain_digest = ?", string(d))
	if kind != "" {
		w.add("kind = ?", kind)
	}
	return getAll(ctx, c, fmt.Sprintf("blob_map of %s at %q of repo %d", d, path, repoID), blobMapDests,
		"SELECT "+blobMapCols+" FROM blob_map"+w.clause()+" ORDER BY indexed_at DESC, id DESC", w.args...)
}

// ListBlobMap returns the rows of the repo at the package path, or at every path when path is "".
func (c *conn) ListBlobMap(ctx context.Context, repoID int64, path string) ([]index.BlobMap, error) {
	w := where{}
	w.add("repo_id = ?", repoID)
	if path != "" {
		w.add("path = ?", path)
	}
	return getAll(ctx, c, fmt.Sprintf("blob_map at %q of repo %d", path, repoID), blobMapDests,
		"SELECT "+blobMapCols+" FROM blob_map"+w.clause()+" ORDER BY path, stored_digest", w.args...)
}

// PutBlobMap inserts the rows, or replaces those of the same repo, path and stored digest, in one
// transaction, and fills their ID; IndexedAt is now when zero.
func (c *conn) PutBlobMap(ctx context.Context, rows []index.BlobMap) error {
	ts := now()
	return c.withTx(ctx, func(c *conn) error {
		for i := range rows {
			b := &rows[i]
			stamp(&b.IndexedAt, ts)
			err := c.scanRow(ctx, fmt.Sprintf("blob_map %s at %q of repo %d", b.Stored, b.Path, b.RepoID),
				"INSERT INTO blob_map (repo_id, path, kind, plain_digest, stored_digest, size, key_fingerprint, wrapped, pubopts, indexed_at)"+
					" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"+
					" ON CONFLICT (repo_id, path, stored_digest) DO UPDATE SET kind = excluded.kind, plain_digest = excluded.plain_digest,"+
					" size = excluded.size, key_fingerprint = excluded.key_fingerprint, wrapped = excluded.wrapped,"+
					" pubopts = excluded.pubopts, indexed_at = excluded.indexed_at RETURNING id",
				[]any{b.RepoID, b.Path, b.Kind, string(b.Plain), string(b.Stored), b.Size, b.KeyFingerprint, b.Wrapped, b.PubOpts,
					c.d.EncodeTime(b.IndexedAt)},
				&b.ID)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteBlobMap removes the row of the stored digest d at the package path.
func (c *conn) DeleteBlobMap(ctx context.Context, repoID int64, path string, d digest.Digest) error {
	_, err := c.exec(ctx, fmt.Sprintf("blob_map %s at %q of repo %d", d, path, repoID),
		"DELETE FROM blob_map WHERE repo_id = ? AND path = ? AND stored_digest = ?", repoID, path, string(d))
	return err
}

// SweepBlobMap removes the rows of the repo at the package path, or at every path when path is "",
// whose IndexedAt is before seenBefore and whose stored digest no version, file or attachment of
// the repo names, but the blobs indexed after keepBlobsAfter.
func (c *conn) SweepBlobMap(ctx context.Context, repoID int64, path string, seenBefore, keepBlobsAfter time.Time) (int, error) {
	q := "DELETE FROM blob_map WHERE repo_id = ? AND indexed_at < ? AND NOT (kind = ? AND indexed_at > ?)" +
		" AND stored_digest NOT IN (SELECT v.digest FROM versions v JOIN packages p ON p.id = v.package_id WHERE p.repo_id = ?)" +
		" AND stored_digest NOT IN (SELECT f.digest FROM files f JOIN versions v ON v.id = f.version_id" +
		" JOIN packages p ON p.id = v.package_id WHERE p.repo_id = ?)" +
		" AND stored_digest NOT IN (SELECT a.digest FROM attachments a JOIN versions v ON v.id = a.version_id" +
		" JOIN packages p ON p.id = v.package_id WHERE p.repo_id = ?)"
	args := []any{repoID, c.d.EncodeTime(seenBefore), index.BlobMapBlob, c.d.EncodeTime(keepBlobsAfter), repoID, repoID, repoID}
	if path != "" {
		q += " AND path = ?"
		args = append(args, path)
	}
	return c.exec(ctx, fmt.Sprintf("blob_map at %q of repo %d", path, repoID), q, args...)
}

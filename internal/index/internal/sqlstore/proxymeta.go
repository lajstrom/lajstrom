package sqlstore

import (
	"context"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// proxy_meta: a cache of the upstream's documents (ADR 0013). Reindex neither reads nor writes
// it, and ResetDerived leaves it; a write invalidates nothing on its own.

const proxyMetaCols = "id, repo_id, name, kind, state, content, content_type, sha256, etag, last_modified, checked_at"

func proxyMetaDests(m *index.ProxyMeta) []any {
	return []any{&m.ID, &m.RepoID, &m.Name, &m.Kind, &m.State, blobDest{&m.Content}, textDest{&m.ContentType},
		textDest{&m.SHA256}, textDest{&m.ETag}, textDest{&m.LastModified}, timeDest{&m.CheckedAt}}
}

// proxyMetaWhat names a row of proxy_meta for errors.
func proxyMetaWhat(repoID int64, name, kind string) string {
	return fmt.Sprintf("proxy_meta %q %q in repo %d", name, kind, repoID)
}

// GetProxyMeta returns the row of a repo's name key and document kind.
func (c *conn) GetProxyMeta(ctx context.Context, repoID int64, name, kind string) (*index.ProxyMeta, error) {
	return getOne(ctx, c, proxyMetaWhat(repoID, name, kind), proxyMetaDests,
		"SELECT "+proxyMetaCols+" FROM proxy_meta WHERE repo_id = ? AND name = ? AND kind = ?", repoID, name, kind)
}

// PutProxyMeta inserts m or replaces the row of (RepoID, Name, Kind), keeping its id. CheckedAt
// is now when zero. A nil Content is NULL, the content of an absent row.
func (c *conn) PutProxyMeta(ctx context.Context, m *index.ProxyMeta) error {
	var content any
	if m.Content != nil {
		content = m.Content
	}
	return c.scanRow(ctx, proxyMetaWhat(m.RepoID, m.Name, m.Kind),
		"INSERT INTO proxy_meta (repo_id, name, kind, state, content, content_type, sha256, etag, last_modified, checked_at)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (repo_id, name, kind) DO UPDATE SET state = excluded.state,"+
			" content = excluded.content, content_type = excluded.content_type, sha256 = excluded.sha256, etag = excluded.etag,"+
			" last_modified = excluded.last_modified, checked_at = excluded.checked_at RETURNING id",
		[]any{m.RepoID, m.Name, m.Kind, m.State, content, textArg(m.ContentType), textArg(m.SHA256), textArg(m.ETag),
			textArg(m.LastModified), c.d.EncodeTime(stamp(&m.CheckedAt, now()))},
		&m.ID)
}

// TouchProxyMeta sets CheckedAt of the row.
func (c *conn) TouchProxyMeta(ctx context.Context, id int64, checkedAt time.Time) error {
	return c.execOne(ctx, fmt.Sprintf("proxy_meta id %d", id), "UPDATE proxy_meta SET checked_at = ? WHERE id = ?",
		c.d.EncodeTime(Normalize(checkedAt)), id)
}

// DeleteProxyMeta removes the rows of a name key in a repo, or every row of the repo when name
// is "".
func (c *conn) DeleteProxyMeta(ctx context.Context, repoID int64, name string) (int, error) {
	var w where
	w.add("repo_id = ?", repoID)
	if name != "" {
		w.add("name = ?", name)
	}
	return c.exec(ctx, fmt.Sprintf("proxy_meta of repo %d", repoID), "DELETE FROM proxy_meta"+w.clause(), w.args...)
}

// ExpireProxyMeta removes the absent rows of a repo checked before before.
func (c *conn) ExpireProxyMeta(ctx context.Context, repoID int64, before time.Time) (int, error) {
	return c.exec(ctx, fmt.Sprintf("proxy_meta of repo %d", repoID),
		"DELETE FROM proxy_meta WHERE repo_id = ? AND state = ? AND checked_at < ?",
		repoID, index.ProxyMetaAbsent, c.d.EncodeTime(Normalize(before)))
}

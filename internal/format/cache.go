package format

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// CachedDocument returns the index document kind of scope of repo: from index_cache when it is
// there, otherwise it renders the scope with p.RenderIndex, stores every document the plugin
// returned (Cache.SHA256 = hex sha256 of Content, RenderedAt = now in UTC) and returns the one
// asked for; index.ErrNotFound when the plugin renders no document of that kind (the others are
// stored all the same). Lookup, render and store run in one st.Tx, whose Writer is the Reader
// RenderIndex gets, so that on SQLite the fill cannot interleave with an invalidation by
// ingest; on Postgres (read committed) a fill that started before an invalidation can store a
// stale document until the next invalidation. The apt index issue (#40) decides whether that
// window needs more. Errors of the store and of RenderIndex are returned as they are.
func CachedDocument(ctx context.Context, st index.Store, p Plugin, repo index.Repo, scope IndexScope, kind string) (*index.Cache, error) {
	var found *index.Cache
	err := st.Tx(ctx, func(tx index.Writer) error {
		c, err := tx.GetIndexCache(ctx, repo.ID, string(scope), kind)
		if err == nil {
			found = c
			return nil
		}
		if !errors.Is(err, index.ErrNotFound) {
			return err
		}
		docs, err := p.RenderIndex(ctx, repo, scope, tx)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, d := range docs {
			sum := sha256.Sum256(d.Content)
			c := &index.Cache{
				RepoID:     repo.ID,
				Scope:      string(scope),
				Kind:       d.Kind,
				Content:    d.Content,
				SHA256:     hex.EncodeToString(sum[:]),
				RenderedAt: now,
			}
			if err := tx.PutIndexCache(ctx, c); err != nil {
				return err
			}
			if d.Kind == kind {
				found = c
			}
		}
		return nil
	})
	switch {
	case err != nil:
		return nil, err
	case found == nil:
		return nil, fmt.Errorf("%w: %s document of %s/%s %q", index.ErrNotFound, kind, repo.Format, repo.Name, scope)
	}
	return found, nil
}

// ServeDocument writes c with contentType, ETag `"<sha256>"` and Last-Modified RenderedAt through
// http.ServeContent, so If-None-Match, If-Modified-Since and Range work.
func ServeDocument(w http.ResponseWriter, r *http.Request, c *index.Cache, contentType string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("ETag", `"`+c.SHA256+`"`)
	http.ServeContent(w, r, "", c.RenderedAt, bytes.NewReader(c.Content))
}

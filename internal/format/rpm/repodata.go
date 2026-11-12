package rpm

import (
	"context"
	"fmt"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// repodata serves repodata/{file}: repomd.xml, from index_cache or rendered with the three
// documents it lists, and a document the current repomd.xml lists by its location. A name it
// does not list is 404 without a render: the documents are stored together, so repomd.xml is the
// list of what the cache holds, and a request for an older render's file does not render on
// every try.
func (h *handlers) repodata(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r, auth.ActionRead)
	if !ok {
		return
	}
	ctx := r.Context()
	file := r.PathValue("file")
	c, err := h.cached(ctx, repo, kindRepomd)
	if err == nil && file != kindRepomd {
		if !listed(c.Content)[file] {
			err = fmt.Errorf("%w: rpm/%s has no repodata/%s in the current repomd.xml", index.ErrNotFound, repo.Name, file)
		} else {
			c, err = h.cached(ctx, repo, file)
		}
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ct := gzipContentType
	if file == kindRepomd {
		ct = xmlContentType
	}
	format.ServeDocument(w, r, c, ct)
}

// cached returns the document kind of the repodata from index_cache, rendered with the whole
// repodata on a miss (format.CachedDocument). Nothing here invalidates: ingest and delete
// invalidate every document of the repo in the transaction that changes its versions, so the
// first GET after a publish renders the repodata again.
func (h *handlers) cached(ctx context.Context, repo index.Repo, kind string) (*index.Cache, error) {
	return format.CachedDocument(ctx, h.deps.Index, h.plugin, repo, scopeRepodata, kind)
}

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
	"github.com/lajstrom/lajstrom/internal/policy"
)

// CachedDocument returns the index document kind of scope of repo: from index_cache when it is
// there, otherwise it renders the scope with p.RenderIndex, stores every document the plugin
// returned (Cache.SHA256 = hex sha256 of Content, RenderedAt = now in UTC) and returns the one
// asked for; index.ErrNotFound when the plugin renders no document of that kind (the others are
// stored all the same). Lookup, render and store run in one st.Tx, whose Writer is the Reader
// RenderIndex gets. The render reads the repo's IndexCacheGeneration first, and the documents
// are stored only while it is current: a render that an invalidation overtook on Postgres (read
// committed) is answered and not stored, so the next request renders again (INV-27). Errors of
// the store and of RenderIndex are returned as they are.
//
// Of a repo of kind group, p is an IndexGrouper, else the error wraps ErrUnsupported: the scope
// is rendered with RenderGroupIndex over the group's members (ListGroupMembers, in the same
// transaction), and the documents are kept under the group, with the group's generation, which
// an invalidation of a member's documents increments (docs/spec/proxy-and-group.md §8.6).
//
// The policy of ctx (policy.FromContext) is read once per render (policy.View): RenderIndex
// reads the index through its Reader, which leaves out the versions it refuses, and every
// document is stored with View.Until as its ExpiresAt; a row whose ExpiresAt has passed is
// rendered again (docs/spec/policy.md §6.1).
func CachedDocument(ctx context.Context, st index.Store, p Plugin, repo index.Repo, scope IndexScope, kind string) (*index.Cache, error) {
	var found *index.Cache
	err := st.Tx(ctx, func(tx index.Writer) error {
		now := time.Now().UTC()
		c, err := tx.GetIndexCache(ctx, repo.ID, string(scope), kind)
		if err == nil && !c.Expired(now) {
			found = c
			return nil
		}
		if err != nil && !errors.Is(err, index.ErrNotFound) {
			return err
		}
		gen, err := tx.IndexCacheGeneration(ctx, repo.ID)
		if err != nil {
			return err
		}
		view, err := policy.FromContext(ctx).View(ctx, tx, repo)
		if err != nil {
			return err
		}
		docs, err := render(ctx, view.Reader(tx), p, repo, scope)
		if err != nil {
			return err
		}
		expires := view.Until()
		for _, d := range docs {
			sum := sha256.Sum256(d.Content)
			c := &index.Cache{
				RepoID:     repo.ID,
				Scope:      string(scope),
				Kind:       d.Kind,
				Content:    d.Content,
				SHA256:     hex.EncodeToString(sum[:]),
				RenderedAt: now,
				ExpiresAt:  expires,
			}
			if _, err := tx.PutIndexCache(ctx, c, gen); err != nil {
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

// render renders scope of repo: RenderIndex, or RenderGroupIndex over the members of a group.
func render(ctx context.Context, r index.Reader, p Plugin, repo index.Repo, scope IndexScope) ([]Document, error) {
	if repo.Kind != index.KindGroup {
		return p.RenderIndex(ctx, repo, scope, r)
	}
	g, ok := p.(IndexGrouper)
	if !ok {
		return nil, fmt.Errorf("%w: format %s renders no documents of a group", ErrUnsupported, repo.Format)
	}
	members, err := r.ListGroupMembers(ctx, repo.ID)
	if err != nil {
		return nil, fmt.Errorf("format: the members of %s/%s: %w", repo.Format, repo.Name, err)
	}
	return g.RenderGroupIndex(ctx, repo, members, scope, r)
}

// ServeDocument writes c with contentType, ETag `"<sha256>"` and Last-Modified RenderedAt through
// http.ServeContent, so If-None-Match, If-Modified-Since and Range work.
func ServeDocument(w http.ResponseWriter, r *http.Request, c *index.Cache, contentType string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("ETag", `"`+c.SHA256+`"`)
	http.ServeContent(w, r, "", c.RenderedAt, bytes.NewReader(c.Content))
}

package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

// The results of lajstrom_proxy_documents_total.
const (
	docFresh       = "fresh"
	docRevalidated = "revalidated"
	docRefreshed   = "refreshed"
	docAbsent      = "absent"
	docStale       = "stale"
	docOffline     = "offline"
	docShared      = "shared"
)

// Document returns the document of kind of name in the proxy repo repo (format.Proxy): after
// the allow and deny lists, the upstream's document as proxy_meta keeps it, asked again when the
// row has expired (§4.2), rendered with the plugin's RenderProxy and cached in index_cache under
// the scope of the name's key. A name the upstream does not have is index.ErrNotFound. When the
// upstream fails, the row is used however old; without one, the versions the repo holds are
// rendered, and without those the upstream's failure is the error (§6). A group's document is
// its members' merged (groupDocument).
func (c *Core) Document(ctx context.Context, repo index.Repo, name, kind string) (*index.Cache, error) {
	if repo.Kind == index.KindGroup {
		return c.groupDocument(ctx, repo, name, kind)
	}
	p, err := c.resolve(repo)
	if err != nil {
		return nil, err
	}
	key, err := p.key(name)
	if err != nil {
		return nil, err
	}
	upErr := c.refresh(ctx, p, name, key, kind)
	return c.render(ctx, p, name, key, kind, upErr)
}

// refresh brings the proxy_meta row of (key, kind) up to date as §4.2 says: a fresh row stays,
// an offline repo asks nothing, otherwise the upstream is asked with the row's validators, once
// on this replica for all the requests that miss the row at the same time (revalidate). What
// the upstream fails to answer leaves the row as it is, and is returned for render.
func (c *Core) refresh(ctx context.Context, p *proxied, name, key, kind string) error {
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, key, kind)
	if err != nil {
		return err
	}
	switch {
	case row != nil && p.fresh(row, c.o.Now()):
		countDoc(p, docFresh)
		return nil
	case p.up == nil:
		countDoc(p, docOffline)
		return nil
	}
	fl, joined := c.flights.join(fmt.Sprintf("meta:%d:%s:%s", p.repo.ID, kind, key), func() (bool, error) {
		fctx, cancel := c.detach(ctx)
		defer cancel()
		return false, c.revalidate(fctx, p, name, key, kind)
	})
	select {
	case <-fl.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if joined {
		countDoc(p, docShared)
	}
	return fl.err
}

// revalidate asks the upstream for the document, on a context of its own: a 200 replaces the
// row and drops the name's rendered documents in one transaction; a 304 moves checked_at; a 404
// or a 410 makes the row absent, or deletes it with a negativeTTL of 0s. It reads the row again
// first: a refresh that ended after the caller read it may have made it fresh.
func (c *Core) revalidate(ctx context.Context, p *proxied, name, key, kind string) error {
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, key, kind)
	if err != nil {
		return err
	}
	now := c.o.Now()
	if row != nil && p.fresh(row, now) {
		countDoc(p, docFresh)
		return nil
	}
	err = c.ask(ctx, p, row, name, key, kind, now)
	switch {
	case err == nil, ctx.Err() != nil:
		return err
	case errors.Is(err, errMissing):
		return c.absent(ctx, p, key, kind, now)
	}
	countDoc(p, docStale)
	c.o.Log.WarnContext(ctx, "proxy: the upstream did not answer for a document; what is cached or held is served",
		"repo", p.id(), "upstream", p.settings.Upstream, "name", name, "kind", kind, "cached", row != nil, "error", err.Error())
	return err
}

// ask sends the request for the document and writes what the upstream answered.
func (c *Core) ask(ctx context.Context, p *proxied, row *index.ProxyMeta, name, key, kind string, now time.Time) error {
	req, err := p.hooks.MetadataRequest(name, kind)
	if err != nil {
		return err
	}
	target, err := p.up.Resolve(req.Path)
	if err != nil {
		return err
	}
	var etag, lastModified string
	if row != nil && row.State == index.ProxyMetaPresent {
		etag, lastModified = row.ETag, row.LastModified
	}
	resp, err := p.up.Get(ctx, "document", target, req.Accept, etag, lastModified)
	if err != nil {
		return err
	}
	if resp.Status == 304 {
		if etag == "" && lastModified == "" {
			return fmt.Errorf("%w: upstream %s answered 304 to a request without validators", format.ErrBadGateway, p.settings.Upstream)
		}
		countDoc(p, docRevalidated)
		return c.o.Index.TouchProxyMeta(ctx, row.ID, now)
	}
	body, err := readDocument(resp, p.up)
	if err != nil {
		return err
	}
	parsed, err := p.hooks.ParseMetadata(name, kind, body)
	if err != nil {
		return fmt.Errorf("%w: upstream %s: a document of %q that does not parse: %s", format.ErrBadGateway, p.settings.Upstream, name, err.Error())
	}
	sum := sha256.Sum256(parsed.Keep)
	m := index.ProxyMeta{RepoID: p.repo.ID, Name: key, Kind: kind, State: index.ProxyMetaPresent, Content: parsed.Keep,
		ContentType: parsed.ContentType, SHA256: hex.EncodeToString(sum[:]), ETag: resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"), CheckedAt: now}
	if m.Content == nil {
		m.Content = []byte{}
	}
	countDoc(p, docRefreshed)
	return c.o.Index.Tx(ctx, func(w index.Writer) error {
		if err := w.PutProxyMeta(ctx, &m); err != nil {
			return err
		}
		return w.InvalidateIndexCache(ctx, p.repo.ID, key)
	})
}

// readDocument reads the body of a document, which must not be larger than the upstream's
// maxMetadataSize: a larger one is not cached (§6).
func readDocument(resp *Response, up *Upstream) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	limit := up.MaxMetadataSize()
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("%w: upstream %s: a document of %d bytes, above maxMetadataSize %d", format.ErrBadGateway,
			up.Name(), resp.ContentLength, limit)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	switch {
	case err != nil:
		return nil, up.transportError(context.Background(), err)
	case int64(len(body)) > limit:
		return nil, fmt.Errorf("%w: upstream %s: a document above maxMetadataSize %d", format.ErrBadGateway, up.Name(), limit)
	}
	return body, nil
}

// absent records that the upstream does not have the name: an absent row of the kind asked, or
// none of the name when negativeTTL is 0s, with the name's rendered documents dropped (§4.2).
func (c *Core) absent(ctx context.Context, p *proxied, key, kind string, now time.Time) error {
	countDoc(p, docAbsent)
	return c.o.Index.Tx(ctx, func(w index.Writer) error {
		if p.settings.NegativeTTL == 0 {
			if _, err := w.DeleteProxyMeta(ctx, p.repo.ID, key); err != nil {
				return err
			}
		} else {
			m := index.ProxyMeta{RepoID: p.repo.ID, Name: key, Kind: kind, State: index.ProxyMetaAbsent, CheckedAt: now}
			if err := w.PutProxyMeta(ctx, &m); err != nil {
				return err
			}
		}
		return w.InvalidateIndexCache(ctx, p.repo.ID, key)
	})
}

// render answers the document from what is there after refresh, in one transaction, as
// format.CachedDocument does for a hosted repo: an absent row is index.ErrNotFound; a present
// one gives the cached document of index_cache, or one rendered from it and stored; no row
// gives the document of what the repo holds, not stored, or upErr, or index.ErrNotFound when
// the repo holds nothing of the name.
func (c *Core) render(ctx context.Context, p *proxied, name, key, kind string, upErr error) (*index.Cache, error) {
	var out *index.Cache
	err := c.o.Index.Tx(ctx, func(w index.Writer) error {
		row, err := metaRow(ctx, w, p.repo.ID, key, kind)
		switch {
		case err != nil:
			return err
		case row != nil && row.State == index.ProxyMetaAbsent:
			return fmt.Errorf("%w: proxy: %s: upstream %s does not have %q", index.ErrNotFound, p.id(), p.settings.Upstream, name)
		case row != nil:
			if cached, err := w.GetIndexCache(ctx, p.repo.ID, key, kind); err == nil {
				out = cached
				return nil
			} else if !errors.Is(err, index.ErrNotFound) {
				return err
			}
			if out, err = c.renderOne(ctx, p, w, name, key, kind, row.Content); err != nil {
				return err
			}
			return w.PutIndexCache(ctx, out)
		}
		held, err := holds(ctx, w, p, name)
		switch {
		case err != nil:
			return err
		case !held && upErr != nil:
			return upErr
		case !held:
			return fmt.Errorf("%w: proxy: %s: %q is neither cached nor held", index.ErrNotFound, p.id(), name)
		}
		out, err = c.renderOne(ctx, p, w, name, key, kind, nil)
		return err
	})
	return out, err
}

// renderOne renders the document with the plugin, as the index_cache row it is cached as.
func (c *Core) renderOne(ctx context.Context, p *proxied, r index.Reader, name, key, kind string, kept []byte) (*index.Cache, error) {
	doc, err := p.hooks.RenderProxy(ctx, p.repo, name, kind, kept, r, c.base(p.repo))
	if err != nil {
		return nil, fmt.Errorf("proxy: %s: render %s of %q: %w", p.id(), kind, name, err)
	}
	sum := sha256.Sum256(doc.Content)
	return &index.Cache{RepoID: p.repo.ID, Scope: key, Kind: kind, Content: doc.Content, SHA256: hex.EncodeToString(sum[:]),
		RenderedAt: c.o.Now()}, nil
}

// holds reports whether the repo holds a version of name.
func holds(ctx context.Context, r index.Reader, p *proxied, name string) (bool, error) {
	pkgPath, err := p.plugin.EscapeName(name)
	if err != nil {
		return false, nil //nolint:nilerr // a name the format cannot escape is not held
	}
	pkg, err := r.GetPackageByNameOCI(ctx, p.repo.ID, pkgPath)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	vs, err := r.ListVersions(ctx, pkg.ID)
	return len(vs) > 0, err
}

// countDoc records a read of a document in lajstrom_proxy_documents_total.
func countDoc(p *proxied, result string) {
	metrics.ProxyDocuments.WithLabelValues(p.settings.Upstream, result).Inc()
}

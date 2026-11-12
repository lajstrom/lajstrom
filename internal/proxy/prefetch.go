package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// PrefetchFailed is the status of a PrefetchResult that failed; the others are those of a miss:
// held, fetched and shared.
const PrefetchFailed = "failed"

// MaxPrefetchNames bounds the names of one Prefetch (docs/spec/proxy-and-group.md §5.1).
const MaxPrefetchNames = 100

// defaultPrefetchConcurrency is how many entries one Prefetch fetches at a time by default.
const defaultPrefetchConcurrency = 4

// notAttempted is the message of what a Prefetch did not start because its context ended.
const notAttempted = "not attempted: the request ended first"

// PrefetchResult is what became of one entry of a prefetch, or of a name that failed before
// its entries (no Version).
type PrefetchResult struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Variant string `json:"variant,omitempty"`
	// Status is held, fetched, shared or failed (PrefetchFailed).
	Status string `json:"status"`
	// HTTPStatus is the status that a request of the file would have had, for failed.
	HTTPStatus int `json:"httpStatus,omitempty"`
	// Message says why it failed.
	Message string `json:"message,omitempty"`
}

// Prefetch fetches entries of the documents of names into the proxy repo repo before a client
// asks for them (docs/spec/proxy-and-group.md §5.1): with all every entry of a name's document,
// otherwise those of the versions that the document's pointers name (format.Prefetcher.Pointed),
// or the newest when they name none. Each entry goes the way of a miss from §5 step 3
// (proxyArtifact), by the principal of ctx, at most Options.PrefetchConcurrency at a time.
//
// The results are in the order of names, one per entry and one per name that failed before its
// entries. A repo that is not a proxy, of a format without the hook, or names that are not 1 to
// MaxPrefetchNames give an error wrapping format.ErrInvalid; an offline repo index.ErrConflict.
// Once ctx ends, nothing more starts and what is left is failed; a fetch that started ends.
func (c *Core) Prefetch(ctx context.Context, repo index.Repo, names []string, all bool) ([]PrefetchResult, error) {
	id := repo.Format + "/" + repo.Name
	if repo.Kind != index.KindProxy {
		return nil, fmt.Errorf("%w: proxy: %s is a %s repo; only a proxy repo takes a prefetch", format.ErrInvalid, id, repo.Kind)
	}
	if len(names) == 0 || len(names) > MaxPrefetchNames {
		return nil, fmt.Errorf("%w: names: %d of them; a prefetch takes 1 to %d", format.ErrInvalid, len(names), MaxPrefetchNames)
	}
	p, err := c.resolve(repo)
	if err != nil {
		return nil, err
	}
	hooks, ok := p.plugin.(format.Prefetcher)
	if !ok {
		return nil, fmt.Errorf("%w: proxy: format %s has no prefetch", format.ErrInvalid, repo.Format)
	}
	if p.up == nil {
		return nil, fmt.Errorf("%w: proxy: %s is offline, or its upstream %q is not configured: it asks its upstream nothing",
			index.ErrConflict, id, p.settings.Upstream)
	}
	per := make([][]PrefetchResult, len(names))
	tasks := make(chan func(), c.o.PrefetchConcurrency)
	var wg sync.WaitGroup
	for range c.o.PrefetchConcurrency {
		wg.Go(func() {
			for t := range tasks {
				t()
			}
		})
	}
	kind := hooks.PrefetchKind()
	for i, name := range names {
		if ctx.Err() != nil {
			per[i] = []PrefetchResult{{Name: name, Status: PrefetchFailed, Message: notAttempted}}
			continue
		}
		entries, row, err := c.prefetchEntries(ctx, p, hooks, name, kind, all)
		if err != nil {
			per[i] = []PrefetchResult{c.prefetchFailed(ctx, p, PrefetchResult{Name: name}, err)}
			continue
		}
		per[i] = make([]PrefetchResult, len(entries))
		for j, e := range entries {
			out := &per[i][j]
			tasks <- func() { *out = c.prefetchEntry(ctx, p, name, kind, e, row) }
		}
	}
	close(tasks)
	wg.Wait()
	return slices.Concat(per...), nil
}

// prefetchEntries reads the document of kind of name as a miss reads it (§4.2, through the
// lists) and returns the entries the prefetch takes with the document's row.
func (c *Core) prefetchEntries(ctx context.Context, p *proxied, hooks format.Prefetcher, name, kind string, all bool) (
	[]format.UpstreamEntry, *index.ProxyMeta, error) {
	key, err := p.key(name)
	if err != nil {
		return nil, nil, err
	}
	upErr := c.refresh(ctx, p, name, key, kind)
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, key, kind)
	switch {
	case err != nil:
		return nil, nil, err
	case row == nil && upErr != nil:
		return nil, nil, upErr
	case row == nil || row.State == index.ProxyMetaAbsent:
		return nil, nil, fmt.Errorf("%w: proxy: %s: upstream %s does not have %q", index.ErrNotFound, p.id(), p.settings.Upstream, name)
	}
	parsed, err := hooks.ParseMetadata(name, kind, row.Content)
	if err != nil {
		return nil, nil, fmt.Errorf("proxy: %s: the kept document of %q does not parse: %w", p.id(), name, err)
	}
	if len(parsed.Entries) == 0 {
		return nil, nil, fmt.Errorf("%w: proxy: %s: upstream %s lists no version of %q", index.ErrNotFound, p.id(), p.settings.Upstream, name)
	}
	if all {
		return parsed.Entries, row, nil
	}
	pointed, err := hooks.Pointed(name, kind, row.Content)
	if err != nil {
		return nil, nil, fmt.Errorf("proxy: %s: the pointers of the kept document of %q: %w", p.id(), name, err)
	}
	return pointedEntries(parsed.Entries, pointed), row, nil
}

// pointedEntries returns the entries of the versions pointed names; when they name none, the
// newest entry by publish time, the last one listed among those of the same time.
func pointedEntries(entries []format.UpstreamEntry, pointed []string) []format.UpstreamEntry {
	var out []format.UpstreamEntry
	for _, e := range entries {
		if slices.Contains(pointed, e.Version) {
			out = append(out, e)
		}
	}
	if len(out) > 0 {
		return out
	}
	newest := entries[0]
	for _, e := range entries[1:] {
		if !e.Published.Before(newest.Published) {
			newest = e
		}
	}
	return []format.UpstreamEntry{newest}
}

// prefetchEntry fetches one entry, as a miss of its first file would, unless ctx has ended.
func (c *Core) prefetchEntry(ctx context.Context, p *proxied, name, kind string, e format.UpstreamEntry, row *index.ProxyMeta) PrefetchResult {
	res := PrefetchResult{Name: name, Version: e.Version, Variant: e.Variant}
	switch {
	case ctx.Err() != nil:
		res.Status, res.Message = PrefetchFailed, notAttempted
		return res
	case len(e.Files) == 0:
		return c.prefetchFailed(ctx, p, res, fmt.Errorf("%w: proxy: %s: upstream %s lists no file of %s %s", format.ErrBadGateway,
			p.id(), p.settings.Upstream, name, e.Version))
	}
	a, err := p.artifactOf(format.ArtifactRef{Name: name, Version: e.Version, Variant: e.Variant, File: e.Files[0].Name, Kind: kind})
	if err != nil {
		return c.prefetchFailed(ctx, p, res, err)
	}
	if _, res.Status, err = c.proxyArtifact(ctx, p, a, &e, row); err != nil {
		return c.prefetchFailed(ctx, p, res, err)
	}
	return res
}

// prefetchFailed makes res failed by err, with the status a request would have had. The message
// is err's when the status is a 4xx or the upstream's failure, whose messages name the upstream
// and never its URL (§6); any other 5xx answers its status text, as the API does, and err is
// logged.
func (c *Core) prefetchFailed(ctx context.Context, p *proxied, res PrefetchResult, err error) PrefetchResult {
	res.Status, res.HTTPStatus, res.Message = PrefetchFailed, format.StatusOf(err), err.Error()
	var unavailable *format.UnavailableError
	upstream := errors.Is(err, format.ErrBadGateway) || errors.Is(err, format.ErrGatewayTimeout) || errors.As(err, &unavailable)
	if res.HTTPStatus >= http.StatusInternalServerError && !upstream {
		res.Message = format.ClientMessage(err)
		c.o.Log.WarnContext(ctx, "proxy: a prefetch failed", "repo", p.id(), "name", res.Name, "version", res.Version,
			"error", err.Error())
	}
	return res
}

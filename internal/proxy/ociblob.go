package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Mirrorer is the write path of an oci proxy repo: *ingest.Pipeline (ADR 0018 point 2).
type Mirrorer interface {
	MirrorManifest(ctx context.Context, repo index.Repo, m ingest.Mirror, principal auth.Principal) error
	MirrorBlob(ctx context.Context, repo index.Repo, path string, desc ocispec.Descriptor, r io.Reader) error
}

var _ Mirrorer = (*ingest.Pipeline)(nil)

// OCIBlob returns the descriptor of the blob d at path in the oci proxy repo repo, as the
// manifest that lists it gives it, and with fetch makes the backend hold it
// (docs/spec/proxy-and-group.md §11.5): unless the backend holds it already or the repo is
// offline, it is fetched from the upstream once across replicas, streamed into the backend
// through the one write path, checked against its size and digest, and what became complete is
// pushed. The caller serves it from the backend. A digest that no manifest held or cached at
// the path lists is index.ErrNotFound, with no request to the upstream; a size above the
// repo's maxFileSize or the registry's maxBlobSize is format.ErrForbidden and proxy.reject;
// bytes that are not the blob are format.ErrBadGateway and proxy.reject.
func (c *Core) OCIBlob(ctx context.Context, repo index.Repo, path string, d digest.Digest, fetch bool) (ocispec.Descriptor, error) {
	p, err := c.resolveOCI(ctx, repo, path)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	desc, err := c.ociListedBlob(ctx, p, d)
	if err != nil || !fetch {
		return desc, err
	}
	if held, err := c.ociHasBlob(ctx, p, desc); held || err != nil {
		return desc, err
	}
	if p.up == nil {
		return ocispec.Descriptor{}, fmt.Errorf("%w: proxy: %s is offline and does not hold %s@%s", index.ErrNotFound, p.id(), p.path, d)
	}
	fl, joined := c.flights.join(fmt.Sprintf("ociblob:%d:%s:%s", p.repo.ID, p.path, d), func() (bool, error) {
		fctx, cancel := c.detach(ctx)
		defer cancel()
		return c.ociFetchBlob(fctx, p, desc)
	})
	select {
	case <-fl.done:
	case <-ctx.Done():
		return ocispec.Descriptor{}, ctx.Err()
	}
	switch {
	case fl.err != nil:
		ociCountFetch(p, fetchError)
		return ocispec.Descriptor{}, fl.err
	case joined || fl.shared:
		ociCountFetch(p, fetchShared)
	default:
		ociCountFetch(p, fetchFetched)
	}
	return desc, nil
}

// ociListedBlob returns the blob d at p's path as the manifest that lists it gives it (its
// oci.ref row), within the bounds of a blob of p.
func (c *Core) ociListedBlob(ctx context.Context, p *ociProxied, d digest.Digest) (ocispec.Descriptor, error) {
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, metaKey(kindOCIRef, d))
	var ref ociRefRow
	switch {
	case err != nil:
		return ocispec.Descriptor{}, err
	case row == nil || json.Unmarshal(row.Content, &ref) != nil || len(ref.Parents) == 0 || manifestTypes[ref.MediaType]:
		return ocispec.Descriptor{}, fmt.Errorf("%w: proxy: %s: no manifest at %s lists the blob %s", index.ErrNotFound, p.id(), p.path, d)
	}
	desc := ocispec.Descriptor{MediaType: ref.MediaType, Digest: d, Size: ref.Size}
	if limit, bound := c.ociLimit(ctx, p); limit >= 0 && desc.Size > limit {
		c.ociReject(ctx, p, d, "size", map[string]any{"size": desc.Size, "limit": limit})
		return ocispec.Descriptor{}, fmt.Errorf("%w: proxy: %s: the blob %s of %d bytes is above %s, %d", format.ErrForbidden, p.id(), d,
			desc.Size, bound, limit)
	}
	return desc, nil
}

// manifestTypes are the media types of manifests and indexes, which an index lists and which
// are no blobs.
var manifestTypes = map[string]bool{
	ocispec.MediaTypeImageManifest: true, ocispec.MediaTypeImageIndex: true,
	"application/vnd.docker.distribution.manifest.v2+json": true, "application/vnd.docker.distribution.manifest.list.v2+json": true,
}

// ociLimit is the bound of a blob of p: the repo's maxFileSize, or the registry's maxBlobSize
// when that is smaller; -1 when there is none.
func (c *Core) ociLimit(ctx context.Context, p *ociProxied) (int64, string) {
	limit, bound := int64(-1), ""
	if n, err := format.MaxFileSize(p.repo); err == nil && n >= 0 {
		limit, bound = n, "the maxFileSize of "+p.id()
	}
	if caps, err := p.prov.Capabilities(ctx); err == nil && caps.MaxBlobSize > 0 && (limit < 0 || caps.MaxBlobSize < limit) {
		limit, bound = caps.MaxBlobSize, "the maxBlobSize of registry "+p.repo.Registry
	}
	return limit, bound
}

// ociHasBlob reports whether the backend holds the blob desc at p's path.
func (c *Core) ociHasBlob(ctx context.Context, p *ociProxied, desc ocispec.Descriptor) (bool, error) {
	ok, err := p.rr.Exists(ctx, desc)
	if err != nil && !errors.Is(err, provider.ErrNotFound) {
		return false, fmt.Errorf("proxy: the blob %s of %s in the backend: %w", desc.Digest, p.name, err)
	}
	return ok, nil
}

// ociFetchBlob fetches the blob desc under its claim (repo, path, digest) into the backend and
// pushes what became complete. shared reports that another replica fetched it.
func (c *Core) ociFetchBlob(ctx context.Context, p *ociProxied, desc ocispec.Descriptor) (shared bool, err error) {
	held := func(ctx context.Context) (bool, error) { return c.ociHasBlob(ctx, p, desc) }
	release, shared, err := c.ociClaim(ctx, p, desc.Digest.String(), held)
	if shared || err != nil {
		return shared, err
	}
	defer release()
	if ok, err := held(ctx); ok || err != nil { // fetched before the claim was taken
		return ok, err
	}
	body, err := p.up.open(ctx, p.path, desc)
	if err != nil {
		return false, err
	}
	defer func() { _ = body.Close() }()
	err = c.o.Mirror.MirrorBlob(ctx, p.repo, p.path, desc, body)
	switch {
	case errors.Is(err, ingest.ErrInvalid):
		c.ociReject(ctx, p, desc.Digest, "checksum", map[string]any{"error": err.Error()})
		return false, fmt.Errorf("%w: proxy: %s: upstream %s served other bytes for %s: %s", format.ErrBadGateway, p.id(), p.upstreamName(),
			desc.Digest, err.Error())
	case err != nil:
		return false, err
	}
	c.o.Log.InfoContext(ctx, "proxy: a blob fetched from the upstream", "repo", p.id(), "upstream", p.upstreamName(), "path", p.path,
		"digest", desc.Digest.String(), "size", desc.Size, "principal", auth.PrincipalFrom(ctx).String())
	if err := c.ociHeld(ctx, p, desc.Digest); err != nil {
		return false, err
	}
	c.ociComplete(ctx, p, desc.Digest)
	return false, nil
}

// open asks the upstream for the blob desc, following its redirects (to public addresses only,
// generic.NewUpstream), and returns its body.
func (u *OCIUpstream) open(ctx context.Context, path string, desc ocispec.Descriptor) (io.ReadCloser, error) {
	b, err := u.client.OpenBlob(ctx, path, desc.Digest, "", false)
	if err != nil {
		return nil, u.classify(ctx, err)
	}
	if b.Body == nil || b.Size >= 0 && b.Size != desc.Size {
		if b.Body != nil {
			_ = b.Body.Close()
		}
		return nil, fmt.Errorf("%w: upstream %s answered the blob %s with %d bytes, not %d", format.ErrBadGateway, u.name, desc.Digest,
			b.Size, desc.Size)
	}
	return b.Body, nil
}

// ociHeld records that the backend holds d at p's path.
func (c *Core) ociHeld(ctx context.Context, p *ociProxied, d digest.Digest) error {
	return c.o.Index.PutProxyMeta(ctx, presentRow(p, metaKey(kindOCIHeld, d), []byte{}, "", "", c.o.Now()))
}

// isHeld reports whether the backend holds d at p's path, as its oci.held row says.
func (c *Core) isHeld(ctx context.Context, p *ociProxied, d digest.Digest) (bool, error) {
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, metaKey(kindOCIHeld, d))
	return row != nil, err
}

// ociCountFetch records a blob request in lajstrom_proxy_fetches_total.
func ociCountFetch(p *ociProxied, result string) {
	metrics.ProxyFetches.WithLabelValues(p.upstreamName(), result).Inc()
}

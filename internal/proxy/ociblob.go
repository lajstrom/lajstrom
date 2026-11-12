package proxy

import (
	"cmp"
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
	MirrorMount(ctx context.Context, repo index.Repo, path, from string, desc ocispec.Descriptor) (bool, error)
}

var _ Mirrorer = (*ingest.Pipeline)(nil)

// OCIBlob returns the descriptor of the blob d at path in the oci proxy repo repo, as the
// manifest that lists it gives it, and with fetch makes the backend hold it
// (docs/spec/proxy-and-group.md §11.5): unless the backend holds it already, it is mounted from
// another path of the repo where the backend holds it, offline too; else, unless the repo is
// offline, it is fetched from the upstream once across replicas, streamed into the backend
// through the one write path and checked against its size and digest; then what became complete
// is pushed. The caller serves it from the backend, but with stream when this request holds the
// blob's claim: then rc is the verified tee of the copy (§11.8), which the caller copies to its
// client and aborts the connection on an error of; every other request waits for the copy. A
// digest that no manifest held or cached at the path lists is index.ErrNotFound, with no request
// to the upstream; a size above the repo's maxFileSize or the registry's maxBlobSize is
// format.ErrForbidden and proxy.reject; bytes that are not the blob are format.ErrBadGateway and
// proxy.reject.
func (c *Core) OCIBlob(ctx context.Context, repo index.Repo, path string, d digest.Digest, fetch, stream bool) (ocispec.Descriptor, io.ReadCloser, error) {
	p, err := c.resolveOCI(ctx, repo, path)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	desc, err := c.ociListedBlob(ctx, p, d)
	if err != nil || !fetch {
		return desc, nil, err
	}
	if held, err := c.ociHeldOrMounted(ctx, p, desc); held || err != nil {
		return desc, nil, err
	}
	if p.up == nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("%w: proxy: %s is offline and does not hold %s@%s", index.ErrNotFound, p.id(), p.path, d)
	}
	var t *tee
	if stream && c.o.TeeBuffer >= 0 {
		t = newTee(desc.Size, int64(cmp.Or(c.o.TeeBuffer, defaultTeeBuffer)))
	}
	fl, joined := c.flights.join(blobFlight(p, d), func() (bool, error) {
		fctx, cancel := c.detach(ctx)
		defer cancel()
		return c.ociFetchBlob(fctx, p, desc, t)
	})
	if rc, err := c.ociStream(ctx, p, desc, t, fl, joined, stream); rc != nil || err != nil {
		return desc, rc, err
	}
	return desc, nil, c.ociAwait(ctx, p, fl, joined)
}

// ociStream returns the stream that answers a request of the blob desc with stream, nil when
// the request waits for the copy: the tee t when its flight fl holds the claim (§11.8); a
// transit when the copy runs under a claim the request does not hold, the flight it joined or
// another replica's (ociTransit, #875).
func (c *Core) ociStream(ctx context.Context, p *ociProxied, desc ocispec.Descriptor, t *tee, fl *flight, joined, stream bool) (io.ReadCloser, error) {
	switch {
	case joined && stream:
		return c.ociTransit(ctx, p, desc), nil
	case joined || t == nil:
		return nil, nil
	}
	select {
	case <-t.started: // this request's fetch holds the claim, and streams
		if t.serve(func(err error) { ociCountFetch(p, fetchOutcome(err)) }) {
			return t.reader(ctx), nil
		}
	case <-t.elsewhere: // another replica holds the claim, and copies
		return c.ociTransit(ctx, p, desc), nil
	case <-fl.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, nil
}

// blobFlight is the key of the flight of the blob d at p's path, which a client's miss and a
// completion share.
func blobFlight(p *ociProxied, d digest.Digest) string {
	return fmt.Sprintf("ociblob:%d:%s:%s", p.repo.ID, p.path, d)
}

// ociAwait waits for the fetch fl of a blob, which this request started or joined, and counts
// its outcome.
func (c *Core) ociAwait(ctx context.Context, p *ociProxied, fl *flight, joined bool) error {
	select {
	case <-fl.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	switch {
	case fl.err != nil:
		ociCountFetch(p, fetchError)
		return fl.err
	case joined || fl.shared:
		ociCountFetch(p, fetchShared)
	default:
		ociCountFetch(p, fetchFetched)
	}
	return nil
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
// pushes what became complete; with t, the claim holder's request reads the bytes as they go
// (§11.8). shared reports that another replica fetched it.
func (c *Core) ociFetchBlob(ctx context.Context, p *ociProxied, desc ocispec.Descriptor, t *tee) (shared bool, err error) {
	if t != nil {
		defer func() { t.finish(cmp.Or(err, errNotFetched(shared))) }()
	}
	held := func(ctx context.Context) (bool, error) { return c.ociHasBlob(ctx, p, desc) }
	waiting := held // ociClaim asks it only while another replica holds the claim
	if t != nil {
		waiting = func(ctx context.Context) (bool, error) {
			t.waits()
			return held(ctx)
		}
	}
	release, shared, err := c.ociClaim(ctx, p, desc.Digest.String(), waiting)
	if shared || err != nil {
		return shared, err
	}
	defer release()
	if ok, err := held(ctx); ok || err != nil { // fetched before the claim was taken
		return ok, err
	}
	got, err := c.ociCopy(ctx, p, desc, t)
	if err == nil {
		err = c.ociHeldBlob(ctx, p, desc.Digest)
	}
	if err != nil {
		attrs := []any{"repo", p.id(), "upstream", p.upstreamName(), "path", p.path, "digest", desc.Digest.String(), "size", desc.Size,
			"got", got}
		if host := redirectHost(err); host != "" { // the CDN that an egress filter must allow (#889)
			attrs = append(attrs, "host", host, "redirected", true)
		}
		c.o.Log.WarnContext(ctx, "proxy: a blob not fetched from the upstream", append(attrs, "error", err.Error())...)
		return false, err
	}
	if t != nil {
		t.finish(nil) // the claim holder completes the blob before the pushes of what it completes
	}
	c.ociComplete(ctx, p, desc.Digest)
	return false, nil
}

// ociCopy streams the blob desc from the upstream into the backend, and through t to the claim
// holder when t is not nil, and returns the bytes it got from the upstream and what the fetch
// answers for its end (ociCopyFailed), nil once the backend committed the blob.
func (c *Core) ociCopy(ctx context.Context, p *ociProxied, desc ocispec.Descriptor, t *tee) (got int64, err error) {
	body, err := p.up.open(ctx, p.path, desc)
	if err != nil {
		return 0, err
	}
	up := c.resumable(ctx, p, desc, body)
	defer up.Close()
	var src io.Reader = up
	if t != nil {
		t.begin()
		src = io.TeeReader(up, t)
	}
	if err := c.ociCopyFailed(ctx, p, desc, up, c.o.Mirror.MirrorBlob(ctx, p.repo, p.path, desc, src)); err != nil {
		return up.n, err
	}
	c.o.Log.InfoContext(ctx, "proxy: a blob fetched from the upstream", "repo", p.id(), "upstream", p.upstreamName(), "path", p.path,
		"digest", desc.Digest.String(), "size", desc.Size, "resumes", up.resumes, "principal", auth.PrincipalFrom(ctx).String())
	return up.n, nil
}

// ociCopyFailed returns what a fetch answers for err, the end of the copy of the blob desc from
// the upstream's body up into the backend, nil when it was committed: a body that broke and was
// not resumed is the upstream's failure, classified as §6 says, format.ErrBadGateway, or
// format.ErrGatewayTimeout for one that stopped moving for upstreams[].timeout (§2.1, #883,
// #884); bytes that are not the blob are format.ErrBadGateway and proxy.reject; an error of the
// backend keeps its own, and its refusal by a rule of its own is proxy.reject too.
func (c *Core) ociCopyFailed(ctx context.Context, p *ociProxied, desc ocispec.Descriptor, up *upstreamBody, err error) error {
	switch {
	case err == nil:
		return nil
	case up.ended != nil && ctx.Err() == nil:
		return fmt.Errorf("proxy: %s: the blob %s broke after %d of its %d bytes: %w", p.id(), desc.Digest, up.n, desc.Size,
			p.up.classify(ctx, up.ended))
	case errors.Is(err, ingest.ErrInvalid):
		c.ociReject(ctx, p, desc.Digest, "checksum", map[string]any{"error": err.Error()})
		return fmt.Errorf("%w: proxy: %s: upstream %s served other bytes for %s: %s", format.ErrBadGateway, p.id(), p.upstreamName(),
			desc.Digest, err.Error())
	case errors.Is(err, provider.ErrRejected) && !errors.Is(err, provider.ErrUnavailable):
		// a rule of the backend's, Quay's build rule among them (§11.2): its 4xx names it
		c.ociReject(ctx, p, desc.Digest, "backend", map[string]any{"registry": p.repo.Registry, "error": err.Error()})
	}
	return err
}

// errNotFetched is the end of a tee whose fetch did not stream: another replica fetched the blob
// (shared), or it was held when the claim was taken.
func errNotFetched(shared bool) error {
	if shared {
		return errors.New("proxy: another replica fetched the blob")
	}
	return errors.New("proxy: the blob was held when the claim was taken")
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

// fetchOutcome is the result of lajstrom_proxy_fetches_total of the claim holder's request, whose
// fetch ended with err: fetched once the backend committed the blob, error otherwise.
func fetchOutcome(err error) string {
	if err != nil {
		return fetchError
	}
	return fetchFetched
}

// ociCountFetch records a blob request in lajstrom_proxy_fetches_total.
func ociCountFetch(p *ociProxied, result string) {
	metrics.ProxyFetches.WithLabelValues(p.upstreamName(), result).Inc()
}

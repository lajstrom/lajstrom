package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/server"
)

// defaultMaxManifestSize is Options.MaxManifestSize when it is zero: server.maxManifestSize's.
const defaultMaxManifestSize = 4 << 20

// ociTagRow is the content of an oci.tag row: what the upstream's tag names.
type ociTagRow struct {
	Digest    digest.Digest `json:"digest"`
	MediaType string        `json:"mediaType"`
	Size      int64         `json:"size"`
}

// ociRefRow is the content of an oci.ref row: what a manifest or an index at the path says of a
// blob or a child it lists, and the digests of those that list it.
type ociRefRow struct {
	MediaType string          `json:"mediaType"`
	Size      int64           `json:"size"`
	Parents   []digest.Digest `json:"parents"`
}

// OCIManifest returns the manifest that ref, a tag or a digest, names at path in the oci proxy
// repo repo (docs/spec/proxy-and-group.md §11.4): its descriptor, and its exact bytes when the
// backend does not hold it, nil when it does (the caller reads them from the backend by the
// digest). A tag is its proxy_meta row while fresh, else asked of the upstream with a HEAD; when
// the upstream fails the row is used however old, and without one the backend's tag, unless the
// repository's tags do not move there. A digest is the backend's, else its cached row, else,
// unless the repo is offline, fetched from the upstream once across replicas. Errors wrap
// index.ErrNotFound (404), format.ErrForbidden (the lists), format.ErrInvalid (a name the
// backend does not take), format.ErrBadGateway, format.ErrGatewayTimeout, or a
// *format.UnavailableError (the upstream's 429).
func (c *Core) OCIManifest(ctx context.Context, repo index.Repo, path, ref string) (ocispec.Descriptor, []byte, error) {
	p, err := c.resolveOCI(ctx, repo, path)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	var want ocispec.Descriptor
	if strings.Contains(ref, ":") {
		if want.Digest, err = digest.Parse(ref); err != nil {
			return ocispec.Descriptor{}, nil, fmt.Errorf("%w: %q is not a digest", format.ErrInvalid, ref)
		}
	} else if want, err = c.ociTag(ctx, p, ref); err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	return c.ociDocument(ctx, p, want, ref)
}

// ociTag returns what the tag names at p's path (§11.4, steps 1 and 2).
func (c *Core) ociTag(ctx context.Context, p *ociProxied, tag string) (ocispec.Descriptor, error) {
	if profile.IsKeepTag(tag) {
		return ocispec.Descriptor{}, ociManifestUnknown(p, tag)
	}
	kind := kindOCITag + tag
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, kind)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	var upErr error
	switch {
	case row != nil && p.fresh(row, c.o.Now()):
		ociCountDoc(p, docFresh)
	case p.up == nil:
		ociCountDoc(p, docOffline)
	default:
		upErr = c.ociRefreshTag(ctx, p, tag)
		if row, err = metaRow(ctx, c.o.Index, p.repo.ID, p.path, kind); err != nil {
			return ocispec.Descriptor{}, err
		}
	}
	return c.ociTagAnswer(ctx, p, tag, row, upErr)
}

// ociTagAnswer answers the tag from what is there after the upstream was asked, or not: row, and
// upErr, the upstream's answer when it was asked.
func (c *Core) ociTagAnswer(ctx context.Context, p *ociProxied, tag string, row *index.ProxyMeta, upErr error) (ocispec.Descriptor, error) {
	switch {
	case errors.Is(upErr, index.ErrNotFound), row != nil && row.State == index.ProxyMetaAbsent:
		return ocispec.Descriptor{}, ociManifestUnknown(p, tag)
	case upErr != nil && !upstreamFailed(upErr):
		return ocispec.Descriptor{}, upErr
	case row != nil:
		if upErr != nil {
			ociCountDoc(p, docStale)
			c.o.Log.WarnContext(ctx, "proxy: the upstream did not answer for a tag; the cached digest is served", "repo", p.id(),
				"upstream", p.upstreamName(), "path", p.path, "tag", tag, "error", upErr.Error())
		}
		return tagDescriptor(row)
	}
	desc, held, err := c.backendTag(ctx, p, tag)
	switch {
	case err != nil:
		return ocispec.Descriptor{}, err
	case held:
		return desc, nil
	case upErr != nil:
		return ocispec.Descriptor{}, upErr
	}
	return ocispec.Descriptor{}, ociManifestUnknown(p, tag)
}

// tagDescriptor reads an oci.tag row.
func tagDescriptor(row *index.ProxyMeta) (ocispec.Descriptor, error) {
	var t ociTagRow
	if err := json.Unmarshal(row.Content, &t); err != nil || t.Digest.Validate() != nil {
		return ocispec.Descriptor{}, fmt.Errorf("proxy: the cached tag %s of %s does not read", strings.TrimPrefix(row.Kind, kindOCITag), row.Name)
	}
	return ocispec.Descriptor{Digest: t.Digest, MediaType: t.MediaType, Size: t.Size}, nil
}

// backendTag returns what the backend's tag names, for a tag without a row (§11.4, step 2):
// held is false when the backend has no such tag, or when the repository's tags do not move
// there, so that the backend's tag is stale (an oci.fixed-tags row).
func (c *Core) backendTag(ctx context.Context, p *ociProxied, tag string) (desc ocispec.Descriptor, held bool, err error) {
	fixed, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, kindOCIFixedTags)
	if err != nil || fixed != nil {
		return ocispec.Descriptor{}, false, err
	}
	desc, err = p.rr.Resolve(ctx, tag)
	switch {
	case errors.Is(err, provider.ErrNotFound):
		return ocispec.Descriptor{}, false, nil
	case err != nil:
		return ocispec.Descriptor{}, false, fmt.Errorf("proxy: the tag %s of %s in the backend: %w", tag, p.name, err)
	}
	return desc, true, nil
}

// ociRefreshTag asks the upstream for the tag, once on this replica for all the requests that
// miss it at the same time, on a context of its own: errMissing when the upstream does not have
// it, the upstream's failure, or nil.
func (c *Core) ociRefreshTag(ctx context.Context, p *ociProxied, tag string) error {
	fl, joined := c.flights.join(fmt.Sprintf("ocitag:%d:%s:%s", p.repo.ID, p.path, tag), func() (bool, error) {
		fctx, cancel := c.detach(ctx)
		defer cancel()
		return false, c.ociRevalidateTag(fctx, p, tag)
	})
	select {
	case <-fl.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if joined {
		ociCountDoc(p, docShared)
	}
	return fl.err
}

// ociRevalidateTag resolves the tag upstream with a HEAD and writes what it found: the same
// digest moves checked_at, another replaces the row, and a tag the upstream does not have is an
// absent row and errMissing. It reads the row again first: a refresh that ended meanwhile may
// have made it fresh.
func (c *Core) ociRevalidateTag(ctx context.Context, p *ociProxied, tag string) error {
	kind := kindOCITag + tag
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, kind)
	if err != nil {
		return err
	}
	now := c.o.Now()
	if row != nil && p.fresh(row, now) {
		if row.State == index.ProxyMetaAbsent {
			return errMissing
		}
		return nil
	}
	desc, err := p.up.resolve(ctx, p.path, tag)
	switch {
	case errors.Is(err, index.ErrNotFound):
		ociCountDoc(p, docAbsent)
		return errors.Join(errMissing, c.ociAbsent(ctx, p, kind, now))
	case err != nil:
		return err
	case row != nil && row.State == index.ProxyMetaPresent && row.ETag == desc.Digest.String():
		ociCountDoc(p, docRevalidated)
		return c.o.Index.TouchProxyMeta(ctx, row.ID, now)
	}
	ociCountDoc(p, docRefreshed)
	content, _ := json.Marshal(ociTagRow{Digest: desc.Digest, MediaType: desc.MediaType, Size: desc.Size}) // always marshals
	return c.o.Index.PutProxyMeta(ctx, presentRow(p, kind, content, "application/json", desc.Digest.String(), now))
}

// presentRow is a present proxy_meta row of p's path.
func presentRow(p *ociProxied, kind string, content []byte, contentType, etag string, now time.Time) *index.ProxyMeta {
	sum := sha256.Sum256(content)
	return &index.ProxyMeta{RepoID: p.repo.ID, Name: p.path, Kind: kind, State: index.ProxyMetaPresent, Content: content,
		ContentType: contentType, SHA256: hex.EncodeToString(sum[:]), ETag: etag, CheckedAt: now}
}

// ociAbsent records that the upstream does not have what kind names at p's path: an absent
// row, which negativeTTL keeps fresh, and Sweep deletes after it.
func (c *Core) ociAbsent(ctx context.Context, p *ociProxied, kind string, now time.Time) error {
	return c.o.Index.PutProxyMeta(ctx, &index.ProxyMeta{RepoID: p.repo.ID, Name: p.path, Kind: kind, State: index.ProxyMetaAbsent,
		CheckedAt: now})
}

// ociDocument returns the manifest or index want names (§11.4, step 3): the backend's, the
// cached one, or one fetched.
func (c *Core) ociDocument(ctx context.Context, p *ociProxied, want ocispec.Descriptor, ref string) (ocispec.Descriptor, []byte, error) {
	d := want.Digest
	if desc, err := p.rr.Resolve(ctx, d.String()); err == nil {
		return desc, nil, nil
	} else if !errors.Is(err, provider.ErrNotFound) {
		return ocispec.Descriptor{}, nil, fmt.Errorf("proxy: %s@%s in the backend: %w", p.name, d, err)
	}
	kind := metaKey(kindOCIManifest, d)
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, kind)
	switch {
	case err != nil:
		return ocispec.Descriptor{}, nil, err
	case row != nil && row.State == index.ProxyMetaPresent:
		return ocispec.Descriptor{MediaType: row.ContentType, Digest: d, Size: int64(len(row.Content))}, row.Content, nil
	case p.up == nil, row != nil && p.fresh(row, c.o.Now()):
		return ocispec.Descriptor{}, nil, ociManifestUnknown(p, ref)
	}
	fl, _ := c.flights.join(fmt.Sprintf("ocidoc:%d:%s:%s", p.repo.ID, p.path, d), func() (bool, error) {
		fctx, cancel := c.detach(ctx)
		defer cancel()
		return c.ociFetchDocument(fctx, p, want)
	})
	select {
	case <-fl.done:
	case <-ctx.Done():
		return ocispec.Descriptor{}, nil, ctx.Err()
	}
	if fl.err != nil {
		return ocispec.Descriptor{}, nil, fl.err
	}
	row, err = metaRow(ctx, c.o.Index, p.repo.ID, p.path, kind)
	switch {
	case err != nil:
		return ocispec.Descriptor{}, nil, err
	case row == nil || row.State != index.ProxyMetaPresent:
		return ocispec.Descriptor{}, nil, ociManifestUnknown(p, ref)
	}
	return ocispec.Descriptor{MediaType: row.ContentType, Digest: d, Size: int64(len(row.Content))}, row.Content, nil
}

// ociFetchDocument fetches the manifest or index want under its claim (repo, path, digest),
// checks it against its digest and caches it with what it lists. shared reports that another
// replica fetched it while this one waited.
func (c *Core) ociFetchDocument(ctx context.Context, p *ociProxied, want ocispec.Descriptor) (shared bool, err error) {
	kind := metaKey(kindOCIManifest, want.Digest)
	cached := func(ctx context.Context) (bool, error) {
		row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, kind)
		return row != nil && row.State == index.ProxyMetaPresent, err
	}
	release, shared, err := c.ociClaim(ctx, p, want.Digest.String(), cached)
	if shared || err != nil {
		return shared, err
	}
	defer release()
	if ok, err := cached(ctx); ok || err != nil { // another replica's fetch ended before this claim
		return ok, err
	}
	desc, err := c.ociListed(ctx, p, want)
	if err != nil {
		return false, err
	}
	raw, err := p.up.fetch(ctx, p.path, desc, c.maxManifestSize())
	switch {
	case errors.Is(err, index.ErrNotFound):
		return false, errors.Join(errMissing, c.ociAbsent(ctx, p, kind, c.o.Now()))
	case errors.Is(err, errNotDigest):
		c.ociReject(ctx, p, want.Digest, "checksum", map[string]any{"error": err.Error()})
		return false, fmt.Errorf("%w: proxy: %s: upstream %s: %s", format.ErrBadGateway, p.id(), p.upstreamName(), err.Error())
	case err != nil:
		return false, err
	}
	desc.Size = int64(len(raw))
	return false, c.ociCache(ctx, p, desc, raw)
}

// ociListed completes want with its media type and size: from the tag that named it, else from
// the manifest or index that lists it (its oci.ref row), else from a HEAD of the digest
// upstream, which Docker Hub does not count.
func (c *Core) ociListed(ctx context.Context, p *ociProxied, want ocispec.Descriptor) (ocispec.Descriptor, error) {
	if want.MediaType != "" {
		return want, nil
	}
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, metaKey(kindOCIRef, want.Digest))
	if err != nil {
		return want, err
	}
	var ref ociRefRow
	if row != nil && json.Unmarshal(row.Content, &ref) == nil && ref.MediaType != "" {
		return ocispec.Descriptor{MediaType: ref.MediaType, Digest: want.Digest, Size: ref.Size}, nil
	}
	desc, err := p.up.resolve(ctx, p.path, want.Digest.String())
	switch {
	case errors.Is(err, index.ErrNotFound):
		return want, errors.Join(errMissing, c.ociAbsent(ctx, p, metaKey(kindOCIManifest, want.Digest), c.o.Now()))
	case err != nil:
		return want, err
	case desc.Digest != want.Digest:
		return want, fmt.Errorf("%w: proxy: upstream %s answered %s for %s", format.ErrBadGateway, p.upstreamName(), desc.Digest, want.Digest)
	}
	return desc, nil
}

// errNotDigest is the error of a manifest whose bytes do not have their digest.
var errNotDigest = errors.New("proxy: the manifest's bytes do not have its digest")

// fetch asks the upstream for the manifest or index desc by its digest, a counted pull on Docker
// Hub, and returns its bytes, at most limit, checked against the digest.
func (u *OCIUpstream) fetch(ctx context.Context, path string, desc ocispec.Descriptor, limit int64) ([]byte, error) {
	if desc.Size > limit {
		return nil, fmt.Errorf("%w: upstream %s: a manifest of %d bytes, above server.maxManifestSize %d", format.ErrBadGateway, u.name,
			desc.Size, limit)
	}
	rr, err := u.client.Repository(ctx, path)
	if err != nil {
		return nil, u.classify(ctx, err)
	}
	rc, err := rr.Fetch(ctx, desc)
	if err != nil {
		return nil, u.classify(ctx, err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, limit+1))
	switch {
	case err != nil:
		return nil, u.classify(ctx, err)
	case int64(len(raw)) > limit:
		return nil, fmt.Errorf("%w: upstream %s: a manifest above server.maxManifestSize %d", format.ErrBadGateway, u.name, limit)
	case desc.Digest.Algorithm().FromBytes(raw) != desc.Digest:
		return nil, fmt.Errorf("%w: %s", errNotDigest, desc.Digest)
	}
	return raw, nil
}

// ociCache writes the manifest or index desc, its bytes raw, and an oci.ref row for each blob
// or child it lists, with desc among its parents, in one transaction.
func (c *Core) ociCache(ctx context.Context, p *ociProxied, desc ocispec.Descriptor, raw []byte) error {
	listed, err := listedBy(raw)
	if err != nil {
		c.ociReject(ctx, p, desc.Digest, "invalid", map[string]any{"error": err.Error()})
		return fmt.Errorf("%w: proxy: %s: upstream %s served %s, which is not a manifest: %s", format.ErrBadGateway, p.id(),
			p.upstreamName(), desc.Digest, err.Error())
	}
	now := c.o.Now()
	return c.o.Index.Tx(ctx, func(w index.Writer) error {
		for _, l := range listed {
			kind := metaKey(kindOCIRef, l.Digest)
			row, err := metaRow(ctx, w, p.repo.ID, p.path, kind)
			if err != nil {
				return err
			}
			ref := ociRefRow{MediaType: l.MediaType, Size: l.Size}
			if row != nil {
				_ = json.Unmarshal(row.Content, &ref) // a row that does not read is replaced
				ref.MediaType, ref.Size = l.MediaType, l.Size
			}
			if !slices.Contains(ref.Parents, desc.Digest) {
				ref.Parents = append(ref.Parents, desc.Digest)
			}
			content, _ := json.Marshal(ref) // always marshals
			if err := w.PutProxyMeta(ctx, presentRow(p, kind, content, "application/json", "", now)); err != nil {
				return err
			}
		}
		return w.PutProxyMeta(ctx, presentRow(p, metaKey(kindOCIManifest, desc.Digest), raw, desc.MediaType, desc.Digest.String(), now))
	})
}

// listedBy returns what the manifest or index raw lists: its config and layers, or its
// manifests, each with a digest.
func listedBy(raw []byte) ([]ocispec.Descriptor, error) {
	var doc struct {
		Config    *ocispec.Descriptor  `json:"config"`
		Layers    []ocispec.Descriptor `json:"layers"`
		Manifests []ocispec.Descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	listed := append(doc.Manifests, doc.Layers...)
	if doc.Config != nil {
		listed = append(listed, *doc.Config)
	}
	for _, l := range listed {
		if err := l.Digest.Validate(); err != nil || l.Size < 0 {
			return nil, fmt.Errorf("it lists %q of %d bytes", l.Digest, l.Size)
		}
	}
	return listed, nil
}

// maxManifestSize is Options.MaxManifestSize with its default.
func (c *Core) maxManifestSize() int64 {
	if c.o.MaxManifestSize > 0 {
		return c.o.MaxManifestSize
	}
	return defaultMaxManifestSize
}

// ociReject writes the audit row proxy.reject of what the upstream served at p's path under
// the digest d, and counts it.
func (c *Core) ociReject(ctx context.Context, p *ociProxied, d digest.Digest, reason string, details map[string]any) {
	metrics.ProxyFetches.WithLabelValues(p.upstreamName(), fetchRejected).Inc()
	details["upstream"], details["ref"], details["reason"] = p.upstreamName(), d.String(), reason
	if id := server.RequestID(ctx); id != "" {
		details["requestId"] = id
	}
	ev := index.AuditEvent{At: c.o.Now(), Principal: auth.PrincipalFrom(ctx).String(), Action: "proxy.reject", Repo: p.id(),
		Subject: p.name, Digest: d, IP: server.ClientAddr(ctx), Details: marshal(details)}
	var err error
	if c.o.Audit != nil {
		err = c.o.Audit.Write(ctx, ev)
	} else {
		err = c.o.Index.AppendAudit(context.WithoutCancel(ctx), &ev)
	}
	if err != nil {
		c.o.Log.ErrorContext(ctx, "proxy: audit row of a refused fetch not written", "repo", p.id(), "name", p.name, "error", err.Error())
	}
}

// ociCountDoc records a read of a tag in lajstrom_proxy_documents_total.
func ociCountDoc(p *ociProxied, result string) {
	metrics.ProxyDocuments.WithLabelValues(p.upstreamName(), result).Inc()
}

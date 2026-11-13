package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// The prefetch of an oci proxy repo (docs/spec/proxy-and-group.md §11.10, #886): the images of a
// list of references, each the way a pull goes, on the pull's own flights, claims and write path.

// The kinds of the results of PrefetchOCI: what a reference names, an index or a manifest; a
// platform's manifest of an index; a blob of a manifest.
const (
	PrefetchIndex    = "index"
	PrefetchManifest = "manifest"
	PrefetchBlob     = "blob"
)

// errNotAttempted is what a prefetch did not start because its context ended.
var errNotAttempted = errors.New(notAttempted)

// PrefetchOCI fetches the images of refs into the oci proxy repo repo before a client pulls them
// (docs/spec/proxy-and-group.md §11.10), by the principal of ctx. Each reference, a path below
// the repo with a tag or a digest, goes the way a pull goes: the lists and CheckName, the tag,
// the manifest or index it names, the manifest of each of platforms that an index lists, each
// under its claim; then every blob of a manifest the backend does not hold, mounted from another
// path of the repo or fetched under its claim, without a tee; then the completion, the
// manifests, the indexes and the tags that became complete pushed. An index without platforms is
// a failed result, 400.
//
// The results are in the order of refs: what each names, each platform of an index, each blob
// that was not held, or one failed result for a reference that failed before. A repo that is not
// an oci proxy, refs that are not 1 to MaxPrefetchNames and platforms that are not
// os/architecture[/variant] give an error wrapping format.ErrInvalid; an offline repo
// index.ErrConflict. At most Options.PrefetchConcurrency images are resolved at a time, and then
// as many blobs fetched. Once ctx ends nothing more starts, and what is left is failed; a fetch
// that started ends.
func (c *Core) PrefetchOCI(ctx context.Context, repo index.Repo, refs, platforms []string) ([]PrefetchResult, error) {
	id := repo.Format + "/" + repo.Name
	switch {
	case repo.Format != ociFormat || repo.Kind != index.KindProxy:
		return nil, fmt.Errorf("%w: proxy: %s is a %s repo of format %s; only an oci proxy repo takes a prefetch of images",
			format.ErrInvalid, id, repo.Kind, repo.Format)
	case len(refs) == 0 || len(refs) > MaxPrefetchNames:
		return nil, fmt.Errorf("%w: images: %d of them; a prefetch takes 1 to %d", format.ErrInvalid, len(refs), MaxPrefetchNames)
	case c.o.Mirror == nil:
		return nil, fmt.Errorf("%w: proxy: this server has no write path for oci proxy repos", format.ErrUnsupported)
	}
	want, err := parsePlatforms(platforms)
	if err != nil {
		return nil, err
	}
	settings, err := ParseSettings(repo.Upstream)
	if err != nil {
		return nil, fmt.Errorf("proxy: the settings of %s: %w", id, err)
	}
	if _, ok := c.o.OCIUpstreams[settings.Upstream]; !ok || settings.Offline {
		return nil, fmt.Errorf("%w: proxy: %s is offline, or its upstream %q is not configured: it asks its upstream nothing",
			index.ErrConflict, id, settings.Upstream)
	}
	images := make([]*ociImage, len(refs))
	prefetchEach(c.o.PrefetchConcurrency, len(refs), func(i int) { images[i] = c.prefetchImage(ctx, repo, refs[i], want) })
	tasks := ociBlobTasks(images)
	prefetchEach(c.o.PrefetchConcurrency, len(tasks), func(i int) {
		for _, b := range tasks[i] { // the first path fetches, the others mount from it
			c.prefetchBlob(ctx, b)
		}
	})
	var out []PrefetchResult
	for _, img := range images {
		out = append(out, c.prefetchFinish(ctx, img)...)
	}
	return out, nil
}

// prefetchEach calls fn(i) for each i below n, at most k at a time, in order, and returns when
// every call has.
func prefetchEach(k, n int, fn func(i int)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, k)
	for i := range n {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			fn(i)
		})
	}
	wg.Wait()
}

// ociImage is one image of a prefetch: the reference as the request gave it; the repo at its path
// (nil once it failed before what it names was known, with top saying why); the tag it names; what
// it names; and the manifests whose blobs the prefetch looks at.
type ociImage struct {
	ref       string
	p         *ociProxied
	tag       string
	top       PrefetchResult
	doc       ociDoc
	manifests []*ociImageManifest
}

// ociImageManifest is a manifest of an image of a prefetch, what its reference names or a
// platform of its index: its result, its document, and the blobs it lists, but when the backend
// holds it or its document could not be had.
type ociImageManifest struct {
	res    PrefetchResult
	doc    ociDoc
	listed []ocispec.Descriptor
	blobs  []*ociBlobFetch
}

// ociBlobFetch is a blob of a prefetch at a path, which every manifest of the request that lists
// it there shares; owner, the first, reports it.
type ociBlobFetch struct {
	p      *ociProxied
	desc   ocispec.Descriptor
	owner  *ociImageManifest
	status string
	bytes  int64
	err    error
	res    *PrefetchResult
}

// prefetchImage resolves the image ref names in repo, as a pull reads it: its path, the tag, what
// it names and, for an index, the manifest of each platform of want that it lists (§11.10, step
// 2). It fetches no blob.
func (c *Core) prefetchImage(ctx context.Context, repo index.Repo, ref string, want []ociPlatform) *ociImage {
	img := &ociImage{ref: ref, top: PrefetchResult{Name: ref}}
	fail := func(err error) *ociImage {
		img.p, img.manifests = nil, nil
		img.top = c.prefetchFailedAt(ctx, repo.Format+"/"+repo.Name, img.top, err)
		return img
	}
	if ctx.Err() != nil {
		return fail(errNotAttempted)
	}
	path, tag, d, err := parseImageRef(ref)
	if err != nil {
		return fail(err)
	}
	p, err := c.resolveOCI(ctx, repo, c.OCIPath(repo, path))
	if err != nil {
		return fail(err)
	}
	top := ocispec.Descriptor{Digest: d}
	if d == "" {
		if top, err = c.ociTag(ctx, p, tag); err != nil {
			return fail(err)
		}
		img.tag = tag
	}
	img.top.Kind, img.top.Digest = kindOf(top.MediaType), top.Digest
	if img.doc, err = c.prefetchDocument(ctx, p, top); err != nil {
		return fail(err)
	}
	img.p = p
	if !isIndex(img.doc.desc.MediaType) {
		img.top.Kind = PrefetchManifest
		img.manifests = []*ociImageManifest{prefetchListed(img.top, img.doc)}
		return img
	}
	img.top.Kind, img.top.Status, img.top.Bytes = PrefetchIndex, img.doc.status, img.doc.bytes
	children, err := indexChildren(img.doc.raw)
	switch {
	case err != nil:
		return fail(fmt.Errorf("%w: proxy: %s: the index %s does not read: %s", format.ErrBadGateway, p.id(), top.Digest, err.Error()))
	case len(want) == 0:
		return fail(fmt.Errorf("%w: %s names an index, of the platforms %s: name those to fetch in platforms", format.ErrInvalid, ref,
			platformList(children)))
	}
	for _, w := range want {
		img.manifests = append(img.manifests, c.prefetchPlatform(ctx, img, children, w)...)
	}
	return img
}

// prefetchPlatform returns the manifests of the index of img that w names, each with its document
// as a pull of it reads it, or a failed result, 404, when it lists none.
func (c *Core) prefetchPlatform(ctx context.Context, img *ociImage, children []ocispec.Descriptor, w ociPlatform) []*ociImageManifest {
	var out []*ociImageManifest
	for _, ch := range children {
		if ch.Platform == nil || platformOf(ch.Platform) != w {
			continue
		}
		res := PrefetchResult{Name: img.ref, Kind: PrefetchManifest, Platform: platformName(ch.Platform), Digest: ch.Digest}
		switch {
		case img.doc.backend: // a complete index: the backend holds every child
			res.Status = fetchHeld
			out = append(out, &ociImageManifest{res: res, doc: ociDoc{status: fetchHeld, backend: true}})
			continue
		case ctx.Err() != nil:
			out = append(out, &ociImageManifest{res: c.prefetchFailedAt(ctx, img.p.id(), res, errNotAttempted)})
			continue
		}
		doc, err := c.prefetchDocument(ctx, img.p, ocispec.Descriptor{MediaType: ch.MediaType, Digest: ch.Digest, Size: ch.Size})
		if err == nil && isIndex(doc.desc.MediaType) {
			err = fmt.Errorf("%w: proxy: %s: the index %s lists the index %s for %s, which a prefetch does not follow", format.ErrBadGateway,
				img.p.id(), img.top.Digest, ch.Digest, res.Platform)
		}
		if err != nil {
			out = append(out, &ociImageManifest{res: c.prefetchFailedAt(ctx, img.p.id(), res, err)})
			continue
		}
		out = append(out, prefetchListed(res, doc))
	}
	if len(out) == 0 {
		err := fmt.Errorf("%w: proxy: %s: the index %s lists no manifest for %s", index.ErrNotFound, img.p.id(), img.top.Digest, w)
		out = append(out, &ociImageManifest{res: c.prefetchFailedAt(ctx, img.p.id(),
			PrefetchResult{Name: img.ref, Kind: PrefetchManifest, Platform: w.String()}, err)})
	}
	return out
}

// prefetchListed is the manifest of res whose document is doc, with the blobs it lists unless
// the backend holds it.
func prefetchListed(res PrefetchResult, doc ociDoc) *ociImageManifest {
	res.Status, res.Bytes = doc.status, doc.bytes
	m := &ociImageManifest{res: res, doc: doc}
	if !doc.backend {
		m.listed, _ = listedBy(doc.raw) // the cache took it only once listedBy read it
	}
	return m
}

// ociDoc is a manifest or an index that a prefetch made the repo have: its descriptor; its bytes,
// but for a manifest the backend holds; held, fetched or shared; whether the backend holds it,
// and so everything it lists; and the bytes this request got of it from the upstream.
type ociDoc struct {
	desc    ocispec.Descriptor
	raw     []byte
	status  string
	backend bool
	bytes   int64
}

// prefetchDocument makes the repo have the manifest or index want at p's path as a pull's GET of
// it does (§11.4, step 3): the backend's, else its cached row, else fetched in the flight of a
// client's miss and under its claim (ociFetchDocument). Errors are those of OCIManifest.
func (c *Core) prefetchDocument(ctx context.Context, p *ociProxied, want ocispec.Descriptor) (ociDoc, error) {
	d := want.Digest
	desc, err := p.rr.Resolve(ctx, d.String())
	switch {
	case err == nil:
		doc := ociDoc{desc: desc, status: fetchHeld, backend: true}
		if isIndex(desc.MediaType) {
			doc.raw, err = c.ociHeldBytes(ctx, p, desc)
		}
		return doc, err
	case !errors.Is(err, provider.ErrNotFound):
		return ociDoc{}, fmt.Errorf("proxy: %s@%s in the backend: %w", p.name, d, err)
	}
	kind := metaKey(kindOCIManifest, d)
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, kind)
	switch {
	case err != nil:
		return ociDoc{}, err
	case row != nil && row.State == index.ProxyMetaPresent:
		return rowDoc(row, d, fetchHeld), nil
	case p.up == nil, row != nil && p.fresh(row, c.o.Now()):
		return ociDoc{}, ociManifestUnknown(p, d.String())
	}
	return c.prefetchFetchDocument(ctx, p, want)
}

// prefetchFetchDocument fetches the manifest or index want at p's path in the flight of a
// client's miss and under its claim (ociFetchDocument), and returns it, fetched or shared.
func (c *Core) prefetchFetchDocument(ctx context.Context, p *ociProxied, want ocispec.Descriptor) (ociDoc, error) {
	d, kind := want.Digest, metaKey(kindOCIManifest, want.Digest)
	fl, joined := c.flights.join(docFlight(p, d), func() (bool, error) {
		fctx, cancel := c.detach(ctx)
		defer cancel()
		return c.ociFetchDocument(fctx, p, want)
	})
	select {
	case <-fl.done:
	case <-ctx.Done():
		return ociDoc{}, ctx.Err()
	}
	if fl.err != nil {
		return ociDoc{}, fl.err
	}
	switch row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, kind); {
	case err != nil:
		return ociDoc{}, err
	case row == nil || row.State != index.ProxyMetaPresent:
		return ociDoc{}, ociManifestUnknown(p, d.String())
	case joined || fl.shared:
		return rowDoc(row, d, fetchShared), nil
	default:
		doc := rowDoc(row, d, fetchFetched)
		doc.bytes = int64(len(row.Content))
		return doc, nil
	}
}

// rowDoc is the document of the cached oci.manifest row of d.
func rowDoc(row *index.ProxyMeta, d digest.Digest, status string) ociDoc {
	return ociDoc{desc: ocispec.Descriptor{MediaType: row.ContentType, Digest: d, Size: int64(len(row.Content))}, raw: row.Content,
		status: status}
}

// ociHeldBytes returns the bytes of the index desc, which the backend holds: its cached row, else
// the backend's, checked against the digest.
func (c *Core) ociHeldBytes(ctx context.Context, p *ociProxied, desc ocispec.Descriptor) ([]byte, error) {
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, metaKey(kindOCIManifest, desc.Digest))
	switch {
	case err != nil:
		return nil, err
	case row != nil && row.State == index.ProxyMetaPresent:
		return row.Content, nil
	}
	rc, err := p.rr.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("proxy: %s@%s in the backend: %w", p.name, desc.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, c.maxManifestSize()+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("proxy: %s@%s in the backend: %w", p.name, desc.Digest, err)
	case desc.Digest.Algorithm().FromBytes(raw) != desc.Digest:
		return nil, fmt.Errorf("proxy: %s@%s in the backend: its bytes do not have its digest", p.name, desc.Digest)
	}
	return raw, nil
}

// ociBlobTasks returns the blobs of the manifests of images, each once per path, as tasks of one
// digest each: the paths that list it, in the order of the images. A manifest's blobs are those
// of the tasks, shared with the other manifests of the request that list them at its path.
func ociBlobTasks(images []*ociImage) [][]*ociBlobFetch {
	at := map[string]*ociBlobFetch{}
	byDigest := map[digest.Digest]int{}
	var tasks [][]*ociBlobFetch
	for _, img := range images {
		for _, m := range img.manifests {
			for _, l := range m.listed {
				key := img.p.path + "@" + l.Digest.String()
				b, ok := at[key]
				if !ok {
					b = &ociBlobFetch{p: img.p, desc: l, owner: m}
					at[key] = b
					i, ok := byDigest[l.Digest]
					if !ok {
						i = len(tasks)
						byDigest[l.Digest] = i
						tasks = append(tasks, nil)
					}
					tasks[i] = append(tasks[i], b)
				}
				m.blobs = append(m.blobs, b)
			}
		}
	}
	return tasks
}

// prefetchBlob makes the backend hold the blob b at its path as a client's miss of it does
// (§11.5, steps 1 and 2), without a tee: held already, else mounted from another path of the
// repo, else fetched in the blob's flight and under its claim (ociFetchBlob), which pushes what
// became complete.
func (c *Core) prefetchBlob(ctx context.Context, b *ociBlobFetch) {
	p := b.p
	if ctx.Err() != nil {
		b.err = errNotAttempted
		return
	}
	desc, err := c.ociListedBlob(ctx, p, b.desc.Digest)
	if err != nil {
		b.err = err
		return
	}
	switch held, err := c.ociHasBlob(ctx, p, desc); {
	case err != nil:
		b.err = err
		return
	case held:
		b.status = fetchHeld
		if ok, err := c.isHeld(ctx, p, desc.Digest); !ok && err == nil { // a purge of the cache drops rows, not blobs
			if err := c.ociHeldBlob(ctx, p, desc.Digest); err != nil {
				c.o.Log.WarnContext(ctx, "proxy: a blob the backend holds not recorded as held", "repo", p.id(), "path", p.path,
					"digest", desc.Digest.String(), "error", err.Error())
			}
		}
		return
	}
	if c.ociMount(ctx, p, desc) {
		b.status = fetchHeld
		return
	}
	if p.up == nil {
		b.err = fmt.Errorf("%w: proxy: %s is offline and does not hold %s@%s", index.ErrNotFound, p.id(), p.path, desc.Digest)
		return
	}
	fl, joined := c.flights.join(blobFlight(p, desc.Digest), func() (bool, error) {
		fctx, cancel := c.detach(ctx)
		defer cancel()
		return c.ociFetchBlob(fctx, p, desc, nil)
	})
	if b.err = c.ociAwait(ctx, p, fl, joined); b.err != nil {
		return
	}
	if joined || fl.shared {
		b.status = fetchShared
		return
	}
	b.status, b.bytes = fetchFetched, desc.Size
}

// prefetchFinish pushes what the prefetch of img made complete, as the completion of a pull does
// (§11.5, step 3; §11.6), and returns its results: what its reference names, its platforms, and
// the blobs that were not held.
func (c *Core) prefetchFinish(ctx context.Context, img *ociImage) []PrefetchResult {
	if img.p == nil {
		return []PrefetchResult{img.top}
	}
	if ctx.Err() == nil {
		c.prefetchComplete(ctx, img)
	}
	var out []PrefetchResult
	if img.top.Kind == PrefetchIndex {
		out = append(out, img.top)
	}
	for _, m := range img.manifests {
		out = append(out, c.manifestResult(ctx, m))
		for _, b := range m.blobs {
			if b.owner == m && b.status != fetchHeld {
				out = append(out, *c.blobResult(ctx, img.ref, b))
			}
		}
	}
	return out
}

// prefetchComplete pushes what became complete of img: each manifest whose blobs are held, the
// index once every child is, and the tag (§11.5, step 3; §11.6). A failure is logged: the next
// read tries again.
func (c *Core) prefetchComplete(ctx context.Context, img *ociImage) {
	p := img.p
	if !img.doc.backend {
		for _, m := range img.manifests {
			if m.res.Status != PrefetchFailed && !m.doc.backend {
				c.ociPushUp(ctx, p, m.res.Digest)
			}
		}
		if img.top.Kind == PrefetchIndex {
			if _, err := c.ociPushComplete(ctx, p, img.top.Digest); err != nil {
				c.o.Log.WarnContext(ctx, "proxy: a complete index not pushed; the next read of it tries again", "repo", p.id(),
					"path", p.path, "digest", img.top.Digest.String(), "error", err.Error())
			}
		}
	}
	if img.tag != "" {
		c.ociSyncTag(ctx, p, img.tag, false)
	}
}

// manifestResult is the result of m: its document's when the document failed or the backend
// holds it; otherwise failed when one of its blobs failed, fetched when this request fetched it
// or one of them, else shared when another did, else held.
func (c *Core) manifestResult(ctx context.Context, m *ociImageManifest) PrefetchResult {
	res := m.res
	if res.Status == PrefetchFailed || m.doc.backend {
		return res
	}
	fetched, shared := res.Status == fetchFetched, res.Status == fetchShared
	var failed []*ociBlobFetch
	for _, b := range m.blobs {
		switch {
		case b.err != nil:
			failed = append(failed, b)
		case b.status == fetchFetched:
			fetched = true
		case b.status == fetchShared:
			shared = true
		}
		if b.owner == m {
			res.Bytes += b.bytes
		}
	}
	switch {
	case len(failed) > 0:
		first := c.blobResult(ctx, res.Name, failed[0])
		res.Status, res.HTTPStatus, res.RetryAfter = PrefetchFailed, first.HTTPStatus, first.RetryAfter
		res.Message = fmt.Sprintf("%d of its %d blobs failed; %s: %s", len(failed), len(m.blobs), failed[0].desc.Digest, first.Message)
	case fetched:
		res.Status = fetchFetched
	case shared:
		res.Status = fetchShared
	default:
		res.Status = fetchHeld
	}
	return res
}

// blobResult is the result of b, made once.
func (c *Core) blobResult(ctx context.Context, ref string, b *ociBlobFetch) *PrefetchResult {
	if b.res != nil {
		return b.res
	}
	res := PrefetchResult{Name: ref, Kind: PrefetchBlob, Platform: b.owner.res.Platform, Digest: b.desc.Digest, Status: b.status,
		Bytes: b.bytes}
	if b.err != nil {
		res = c.prefetchFailedAt(ctx, b.p.id(), res, b.err)
	}
	b.res = &res
	return b.res
}

// ociPathGrammar and ociTagGrammar are the distribution spec's grammars of a repository path and
// of a tag.
var (
	ociPathGrammar = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
	ociTagGrammar  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// parseImageRef reads a reference of a prefetch, <path>[:<tag>][@<digest>] below the repo: the
// tag latest without a tag or a digest, and the digest alone when both are given, as docker pull
// takes it (§11.10, step 1). A path that does not keep to the grammar, has a component that is a
// file of a registry's image layout (as /v2 refuses it, http-routing.md §2) or names a registry's
// host is format.ErrInvalid.
func parseImageRef(ref string) (path, tag string, d digest.Digest, err error) {
	invalid := func(why string, args ...any) error {
		return fmt.Errorf("%w: %q: %s", format.ErrInvalid, ref, fmt.Sprintf(why, args...))
	}
	rest := ref
	if at := strings.IndexByte(rest, '@'); at >= 0 {
		if d, err = digest.Parse(rest[at+1:]); err != nil {
			return "", "", "", invalid("%q is not a digest", rest[at+1:])
		}
		rest = rest[:at]
	}
	if colon := strings.LastIndexByte(rest, ':'); colon >= 0 && !strings.Contains(rest[colon:], "/") {
		if tag = rest[colon+1:]; !ociTagGrammar.MatchString(tag) {
			return "", "", "", invalid("%q is not a tag", tag)
		}
		rest = rest[:colon]
	}
	first, _, more := strings.Cut(rest, "/")
	switch {
	case more && (strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost"):
		return "", "", "", invalid("it names the registry %s: a reference is a path below the repo", first)
	case !ociPathGrammar.MatchString(rest):
		return "", "", "", invalid("%q is not a repository path", rest)
	}
	for c := range strings.SplitSeq(rest, "/") {
		if profile.LayoutFileComponent(c) {
			return "", "", "", invalid("the path component %q is a file of a registry's image layout", c)
		}
	}
	switch {
	case d != "":
		tag = ""
	case tag == "":
		tag = "latest"
	}
	return rest, tag, d, nil
}

// ociPlatform is a platform of a prefetch, os/architecture[/variant], as containerd normalises it:
// arm64/v8 is arm64, arm is arm/v7, amd64/v1 is amd64 (§11.10, step 1).
type ociPlatform struct{ os, arch, variant string }

// parsePlatforms reads the platforms of a prefetch, each once.
func parsePlatforms(in []string) ([]ociPlatform, error) {
	var out []ociPlatform
	for i, s := range in {
		parts := strings.Split(strings.ToLower(s), "/")
		if len(parts) < 2 || len(parts) > 3 || slices.Contains(parts, "") {
			return nil, fmt.Errorf("%w: platforms[%d]: %q is not <os>/<architecture>[/<variant>]", format.ErrInvalid, i, s)
		}
		pl := ociPlatform{os: parts[0], arch: parts[1]}
		if len(parts) == 3 {
			pl.variant = parts[2]
		}
		if pl = pl.normal(); !slices.Contains(out, pl) {
			out = append(out, pl)
		}
	}
	return out, nil
}

// normal is pl as containerd normalises it.
func (pl ociPlatform) normal() ociPlatform {
	switch {
	case pl.arch == "arm64" && pl.variant == "v8", pl.arch == "amd64" && pl.variant == "v1":
		pl.variant = ""
	case pl.arch == "arm" && pl.variant == "":
		pl.variant = "v7"
	}
	return pl
}

func (pl ociPlatform) String() string {
	if pl.variant == "" {
		return pl.os + "/" + pl.arch
	}
	return pl.os + "/" + pl.arch + "/" + pl.variant
}

// platformOf is the platform of an index's child, normalised.
func platformOf(p *ocispec.Platform) ociPlatform {
	return ociPlatform{os: strings.ToLower(p.OS), arch: strings.ToLower(p.Architecture), variant: strings.ToLower(p.Variant)}.normal()
}

// platformName is the platform of an index's child as the index gives it.
func platformName(p *ocispec.Platform) string {
	return ociPlatform{os: p.OS, arch: p.Architecture, variant: p.Variant}.String()
}

// platformList names the platforms of an index's children, each once, in their order.
func platformList(children []ocispec.Descriptor) string {
	var names []string
	for _, ch := range children {
		name := "?"
		if ch.Platform != nil {
			name = platformName(ch.Platform)
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}

// indexChildren returns the manifests the index raw lists.
func indexChildren(raw []byte) ([]ocispec.Descriptor, error) {
	var idx ocispec.Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, err
	}
	return idx.Manifests, nil
}

// isIndex reports whether mediaType is an index's: OCI's or Docker's manifest list.
func isIndex(mediaType string) bool {
	return mediaType == ocispec.MediaTypeImageIndex || mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
}

// kindOf is the kind of the result of a document of mediaType, "" when it is not known.
func kindOf(mediaType string) string {
	switch {
	case mediaType == "":
		return ""
	case isIndex(mediaType):
		return PrefetchIndex
	}
	return PrefetchManifest
}

// retryAfterOf is when to ask again after err, in whole seconds, rounded up: the Retry-After of
// the upstream's 429, or of an open circuit breaker; 0 for none.
func retryAfterOf(err error) int {
	var busy *format.UnavailableError
	wait := time.Duration(0)
	if errors.As(err, &busy) {
		wait = busy.RetryAfter
	} else if d, ok := provider.RetryAfter(err); ok {
		wait = d
	}
	if wait <= 0 {
		return 0
	}
	return int((wait + time.Second - 1) / time.Second)
}

package proxy_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// pull pulls the platform i of img through c as docker does: the tag, the platform's manifest,
// then its config and layer.
func pull(t *testing.T, e *ociEnv, c *proxy.Core, path, tag string, img image, i int) {
	t.Helper()
	ctx := t.Context()
	_, _, err := c.OCIManifest(ctx, e.repo, path, tag)
	must(t, err)
	_, _, err = c.OCIManifest(ctx, e.repo, path, img.manifests[i].Digest.String())
	must(t, err)
	for _, b := range img.blobs[i] {
		if _, _, err := c.OCIBlob(ctx, e.repo, path, b.Digest, true, false); err != nil {
			t.Fatalf("OCIBlob %s: %v", b.Digest, err)
		}
	}
}

// tagged returns what tag names in the backend at path, "" for nothing.
func (e *ociEnv) tagged(t *testing.T, path, tag string) digest.Digest {
	t.Helper()
	rr, err := e.reg.Repository(t.Context(), "oci/hub/"+path)
	must(t, err)
	desc, err := rr.Resolve(t.Context(), tag)
	if err != nil {
		return ""
	}
	return desc.Digest
}

// TestOCIBlobPull: a pull of one platform fetches its blobs into the backend and pushes its
// manifest under its keep-tag once both are held; the index waits for its other platform, and
// the tag for the index (docs/spec/proxy-and-group.md §11.5, §11.6). A second pull asks the
// upstream for no blob.
func TestOCIBlobPull(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64", "arm64")
	c := e.core("a", proxy.Options{})
	pull(t, e, c, "library/nginx", "1.27", img, 1)
	arm := img.manifests[1].Digest
	if got := e.tagged(t, "library/nginx", profile.KeepTag(arm)); got != arm {
		t.Fatalf("the arm64 manifest's keep-tag names %q", got)
	}
	if got := e.tagged(t, "library/nginx", "1.27"); got != "" {
		t.Fatalf("1.27 is in the backend before its index is complete: %s", got)
	}
	if got := e.tagged(t, "library/nginx", profile.KeepTag(img.index.Digest)); got != "" {
		t.Fatalf("the incomplete index is in the backend")
	}
	pull(t, e, c, "library/nginx", "1.27", img, 0)
	if got := e.tagged(t, "library/nginx", "1.27"); got != img.index.Digest {
		t.Fatalf("1.27 names %q once every platform is held, want the index", got)
	}
	before := e.hub.count("GET /v2/library/nginx/blobs/")
	for i := range 2 {
		for _, b := range img.blobs[i] {
			if _, _, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", b.Digest, true, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := e.hub.count("GET /v2/library/nginx/blobs/") - before; n != 0 {
		t.Errorf("the second pull asked the upstream for %d blobs, want none", n)
	}
	if n := e.hub.count("GET /v2/library/nginx/blobs/"); n != 4 {
		t.Errorf("%d blob GETs in all, want 4", n)
	}
}

// TestOCIBlobNotListed: a digest no held or cached manifest lists is a 404 with no upstream
// request, and a HEAD (fetch false) is answered from the listing without a fetch.
func TestOCIBlobNotListed(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.core("a", proxy.Options{})
	if _, _, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", img.blobs[0][1].Digest, true, false); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("a blob nothing lists: %v, want index.ErrNotFound", err)
	}
	pull(t, e, c, "library/nginx", "1.27", img, 0)
	_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	if _, _, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest, true, false); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a manifest's digest as a blob: %v, want index.ErrNotFound", err)
	}
	other := e.hub.image(t, "library/nginx", "1.28", "amd64")
	_, _, err = c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.28")
	must(t, err)
	_, _, err = c.OCIManifest(t.Context(), e.repo, "library/nginx", other.manifests[0].Digest.String())
	must(t, err)
	desc, _, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", other.blobs[0][1].Digest, false, false)
	if err != nil || desc.Size != other.blobs[0][1].Size {
		t.Fatalf("a HEAD: %+v, %v", desc, err)
	}
	if n := e.hub.count("GET /v2/library/nginx/blobs/" + other.blobs[0][1].Digest.String()); n != 0 {
		t.Errorf("a HEAD fetched the blob %d times", n)
	}
}

// TestOCIBlobOnce: concurrent misses of one blob, on one replica and on two, fetch it once.
func TestOCIBlobOnce(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	cores := []*proxy.Core{e.core("a", proxy.Options{}), e.core("b", proxy.Options{})}
	_, _, err := cores[0].OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	layer := img.blobs[0][1]
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			_, _, err := cores[i%2].OCIBlob(t.Context(), e.repo, "library/nginx", layer.Digest, true, false)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	if n := e.hub.count("GET /v2/library/nginx/blobs/" + layer.Digest.String()); n != 1 {
		t.Errorf("%d GETs of the layer, want 1", n)
	}
}

// TestOCIBlobRefused: bytes that are not the blob are a 502 and proxy.reject, and nothing is
// held; a blob above maxFileSize is a 403 before the download.
func TestOCIBlobRefused(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.core("a", proxy.Options{})
	_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	layer := img.blobs[0][1]
	e.hub.mu.Lock()
	b := e.hub.blobs["library/nginx"][layer.Digest]
	e.hub.blobs["library/nginx"][layer.Digest] = append([]byte("X"), b[1:]...)
	e.hub.mu.Unlock()
	if _, _, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", layer.Digest, true, false); !errors.Is(err, format.ErrBadGateway) {
		t.Fatalf("other bytes: %v, want format.ErrBadGateway", err)
	}
	if _, err := e.reg.StatBlob(t.Context(), "oci/hub/library/nginx", layer.Digest); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("the backend holds the refused bytes: %v", err)
	}
	e.repo.Policy = []byte(`{"maxFileSize": 10}`)
	if _, _, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", layer.Digest, true, false); !errors.Is(err, format.ErrForbidden) {
		t.Fatalf("above maxFileSize: %v, want format.ErrForbidden", err)
	}
	evs, err := e.st.ListAudit(t.Context(), index.AuditFilter{Action: "proxy.reject"})
	if err != nil || len(evs) != 2 {
		t.Errorf("proxy.reject rows = %+v, %v", evs, err)
	}
}

// TestOCITagsFollow: a tag the upstream moves is moved in the backend once the new image is
// held, and the keep-tag of the old one stays, so a client that pinned it is served.
func TestOCITagsFollow(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	first := e.hub.image(t, "library/alpine", "3", "arm64")
	c := e.core("a", proxy.Options{})
	pull(t, e, c, "library/alpine", "3", first, 0)
	if got := e.tagged(t, "library/alpine", "3"); got != first.index.Digest {
		t.Fatalf("3 names %q, want the first index", got)
	}
	second := e.hub.image(t, "library/alpine", "3", "amd64")
	e.advance(11 * time.Minute)
	pull(t, e, c, "library/alpine", "3", second, 0)
	if got := e.tagged(t, "library/alpine", "3"); got != second.index.Digest {
		t.Errorf("3 names %q after the upstream moved it, want the second index", got)
	}
	if got := e.tagged(t, "library/alpine", profile.KeepTag(first.index.Digest)); got != first.index.Digest {
		t.Errorf("the first index's keep-tag names %q", got)
	}
}

// fixedTags is a backend whose tags do not move: a push of a tag that names another digest is
// provider.ErrConflict, as Quay's 409 TAG_IMMUTABLE; immutable says so in Capabilities.
type fixedTags struct {
	*providertest.Fake
	immutable bool
}

func (f fixedTags) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	caps, err := f.Fake.Capabilities(ctx)
	caps.ImmutableTags = f.immutable
	return caps, err
}

func (f fixedTags) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.Fake.Repository(ctx, name)
	return fixedRepo{rr}, err
}

type fixedRepo struct{ provider.Repository }

func (r fixedRepo) PushReference(ctx context.Context, desc ocispec.Descriptor, content io.Reader, ref string) error {
	if d, err := r.Resolve(ctx, ref); err == nil && d.Digest != desc.Digest {
		return provider.ErrConflict
	}
	return r.Repository.PushReference(ctx, desc, content, ref)
}

// TestOCITagsFixed: on a backend with ImmutableTags no upstream tag is pushed; on one that
// refuses to move a tag, the refusal turns the fallback on for the repository, and from then
// on its tags answer from proxy_meta only (§11.6).
func TestOCITagsFixed(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	first := e.hub.image(t, "library/alpine", "3", "arm64")
	reg := fixedTags{Fake: e.reg, immutable: true}
	c := e.coreOn(reg, "a", proxy.Options{})
	pull(t, e, c, "library/alpine", "3", first, 0)
	if got := e.tagged(t, "library/alpine", "3"); got != "" {
		t.Fatalf("an immutable backend got the tag 3: %s", got)
	}
	if got := e.tagged(t, "library/alpine", profile.KeepTag(first.index.Digest)); got != first.index.Digest {
		t.Fatalf("the keep-tag names %q", got)
	}

	e2 := newOCIEnv(t, ociSettings)
	first = e2.hub.image(t, "library/alpine", "3", "arm64")
	c = e2.coreOn(fixedTags{Fake: e2.reg}, "a", proxy.Options{})
	pull(t, e2, c, "library/alpine", "3", first, 0)
	if got := e2.tagged(t, "library/alpine", "3"); got != first.index.Digest {
		t.Fatalf("3 names %q, want the first index", got)
	}
	second := e2.hub.image(t, "library/alpine", "3", "amd64")
	e2.advance(11 * time.Minute)
	pull(t, e2, c, "library/alpine", "3", second, 0)
	if r := e2.row(t, "library/alpine", "oci.fixed-tags"); r == nil {
		t.Fatal("no oci.fixed-tags row after the backend refused to move 3")
	}
	desc, _, err := c.OCIManifest(t.Context(), e2.repo, "library/alpine", "3")
	if err != nil || desc.Digest != second.index.Digest {
		t.Fatalf("3 after the refusal: %v, %v; want the second index from the cache", desc.Digest, err)
	}
	pushes := slices.IndexFunc(e2.reg.PushReferences(), func(r providertest.PushRecord) bool { return r.Tag == "3" && r.Digest == second.index.Digest })
	if pushes >= 0 {
		t.Error("the refused tag was pushed")
	}
}

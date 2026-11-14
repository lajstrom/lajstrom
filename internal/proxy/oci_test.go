package proxy_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

const ociSettings = `{"name": "hub", "metadataTTL": "10m", "negativeTTL": "2m"}`

// TestOCITagTTL: a tag is resolved with a HEAD and its index fetched once; while the tag's row
// is fresh, a pull asks the upstream nothing (docs/spec/proxy-and-group.md §11.4).
func TestOCITagTTL(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64", "arm64")
	c := e.core("a", proxy.Options{})
	for range 2 {
		desc, raw, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27")
		must(t, err)
		if desc.Digest != img.index.Digest || desc.MediaType != ocispec.MediaTypeImageIndex || digest.FromBytes(raw) != img.index.Digest {
			t.Fatalf("OCIManifest = %+v, %d bytes; want the index %s", desc, len(raw), img.index.Digest)
		}
	}
	if head, get := e.hub.count("HEAD /v2/library/nginx/manifests/"), e.hub.count("GET /v2/library/nginx/manifests/"); head != 1 || get != 1 {
		t.Errorf("%d HEAD and %d GET of manifests, want 1 and 1", head, get)
	}
	if r := e.row(t, "library/nginx", "oci.tag:1.27"); r == nil || r.ETag != img.index.Digest.String() {
		t.Errorf("the tag's row = %+v", r)
	}
}

// TestOCIRevalidateHead: an expired tag whose digest has not moved costs one HEAD and no GET,
// which Docker Hub does not count; a moved tag is followed.
func TestOCIRevalidateHead(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.core("a", proxy.Options{})
	_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27")
	must(t, err)
	e.advance(11 * time.Minute)
	_, _, err = c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27")
	must(t, err)
	if head, get := e.hub.count("HEAD /v2/library/nginx/manifests/1.27"), e.hub.count("GET /v2/library/nginx/manifests/"); head != 2 || get != 1 {
		t.Errorf("%d HEAD and %d GET, want 2 and 1", head, get)
	}
	moved := e.hub.image(t, "library/nginx", "1.27", "amd64", "riscv64")
	e.advance(11 * time.Minute)
	desc, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27")
	if err != nil || desc.Digest != moved.index.Digest {
		t.Fatalf("after the upstream moved the tag: %v, %v; want %s", desc.Digest, err, moved.index.Digest)
	}
}

// TestOCIIndexLazy: a child of an index is fetched when it is asked for, so a pull of one
// platform fetches that platform alone; what the index lists is recorded with its media type.
func TestOCIIndexLazy(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64", "arm64")
	c := e.core("a", proxy.Options{})
	_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27")
	must(t, err)
	arm := img.manifests[1]
	desc, raw, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", arm.Digest.String())
	must(t, err)
	if desc.MediaType != ocispec.MediaTypeImageManifest || desc.Size != arm.Size || !bytes.Equal(raw, mustRaw(t, e, "library/nginx", arm)) {
		t.Fatalf("the arm64 manifest: %+v", desc)
	}
	if n := e.hub.count("GET /v2/library/nginx/manifests/" + img.manifests[0].Digest.String()); n != 0 {
		t.Errorf("the amd64 manifest was fetched %d times, want never", n)
	}
	if n := e.hub.count("HEAD /v2/library/nginx/manifests/sha256:"); n != 0 {
		t.Errorf("%d HEADs of a child, want none: the index gave its media type", n)
	}
	var ref struct {
		MediaType string          `json:"mediaType"`
		Parents   []digest.Digest `json:"parents"`
	}
	r := e.row(t, "library/nginx", "oci.ref:"+img.blobs[1][1].Digest.String())
	if r == nil || json.Unmarshal(r.Content, &ref) != nil || ref.MediaType != ocispec.MediaTypeImageLayerGzip || len(ref.Parents) != 1 || ref.Parents[0] != arm.Digest {
		t.Errorf("the layer's oci.ref row = %+v", r)
	}
}

// mustRaw returns the bytes the hub keeps of desc.
func mustRaw(t *testing.T, e *ociEnv, repo string, desc ocispec.Descriptor) []byte {
	t.Helper()
	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()
	return e.hub.docs[repo][desc.Digest.String()].raw
}

// TestOCIUnknownRepository: Docker Hub's 401 for a repository that does not exist is a 404 with
// a negative row, which answers the next request without the upstream (§11.7).
func TestOCIUnknownRepository(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	c := e.core("a", proxy.Options{})
	for range 2 {
		if _, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginxx", "latest"); !errors.Is(err, index.ErrNotFound) {
			t.Fatalf("an unknown repository: %v, want index.ErrNotFound", err)
		}
	}
	if n := e.hub.count("HEAD /v2/library/nginxx/"); n != 1 {
		t.Errorf("%d reads, want one", n)
	}
	if r := e.row(t, "library/nginxx", "oci.tag:latest"); r == nil || r.State != index.ProxyMetaAbsent {
		t.Errorf("the negative row = %+v", r)
	}
}

// TestOCIRateLimited: the upstream's 429 serves the stale row, and without one is a 503 with the
// upstream's Retry-After.
func TestOCIRateLimited(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	e.hub.image(t, "library/redis", "7", "amd64")
	c := e.core("a", proxy.Options{})
	_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27")
	must(t, err)
	e.hub.tooMany.Store(true)
	e.advance(time.Hour)
	desc, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27")
	if err != nil || desc.Digest != img.index.Digest {
		t.Fatalf("a 429 with a stale row: %v, %v; want the row's digest", desc.Digest, err)
	}
	_, _, err = c.OCIManifest(t.Context(), e.repo, "library/redis", "7")
	var busy *format.UnavailableError
	if !errors.As(err, &busy) || busy.RetryAfter > 7*time.Second || busy.RetryAfter < 6*time.Second {
		t.Fatalf("a 429 without a row: %v, want a *format.UnavailableError of 7s", err)
	}
}

// TestOCIOffline: an offline repo sends no request; a tag without a row is the backend's, unless
// the repository's tags do not move there.
func TestOCIOffline(t *testing.T) {
	e := newOCIEnv(t, `{"name": "hub", "offline": true}`)
	e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.core("a", proxy.Options{})
	if _, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("offline, nothing held: %v, want index.ErrNotFound", err)
	}
	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`)
	held := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	rr, err := e.reg.Repository(t.Context(), "oci/hub/library/nginx")
	must(t, err)
	must(t, rr.PushReference(t.Context(), held, bytes.NewReader(raw), "1.27"))
	desc, body, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27")
	if err != nil || desc.Digest != held.Digest || body != nil {
		t.Fatalf("offline, the backend's tag: %v, %d bytes, %v; want %s from the backend", desc.Digest, len(body), err, held.Digest)
	}
	must(t, e.st.PutProxyMeta(t.Context(), &index.ProxyMeta{RepoID: e.repo.ID, Name: "library/nginx", Kind: "oci.fixed-tags",
		State: index.ProxyMetaPresent, Content: []byte{}, CheckedAt: e.clock()}))
	if _, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("offline, tags that do not move: %v, want index.ErrNotFound", err)
	}
	if n := e.hub.count(""); n != 0 {
		t.Errorf("%d requests to the upstream, want none", n)
	}
}

// TestOCIManifestDigestMismatch: a manifest whose bytes do not have its digest is a 502 and an
// audit row proxy.reject, and nothing is cached.
func TestOCIManifestDigestMismatch(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	e.hub.corrupted[img.index.Digest] = true
	c := e.core("a", proxy.Options{})
	if _, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27"); !errors.Is(err, format.ErrBadGateway) {
		t.Fatalf("other bytes: %v, want format.ErrBadGateway", err)
	}
	if r := e.row(t, "library/nginx", "oci.manifest:"+img.index.Digest.String()); r != nil {
		t.Errorf("a row of the refused bytes: %+v", r)
	}
	evs, err := e.st.ListAudit(t.Context(), index.AuditFilter{Action: "proxy.reject"})
	if err != nil || len(evs) != 1 || evs[0].Digest != img.index.Digest {
		t.Errorf("proxy.reject rows = %+v, %v", evs, err)
	}
}

// TestOCIManifestOnce: concurrent misses of one manifest, on one replica and on two, fetch it
// once (the claim of path and digest).
func TestOCIManifestOnce(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	cores := []*proxy.Core{e.core("a", proxy.Options{}), e.core("b", proxy.Options{})}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			_, _, err := cores[i%2].OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	if n := e.hub.count("GET /v2/library/nginx/manifests/"); n != 1 {
		t.Errorf("%d GETs of the manifest, want 1", n)
	}
}

// TestOCINames: the lists and the backend's limits refuse a path before any request, and
// Docker Hub's one-component names are library/<name>.
func TestOCINames(t *testing.T) {
	e := newOCIEnv(t, `{"name": "hub", "deny": ["acme/*"]}`)
	e.reg.MaxNameDepth = 4
	c := e.core("a", proxy.Options{})
	if _, _, err := c.OCIManifest(t.Context(), e.repo, "acme/private", "1"); !errors.Is(err, format.ErrForbidden) {
		t.Errorf("a denied path: %v, want format.ErrForbidden", err)
	}
	if _, _, err := c.OCIManifest(t.Context(), e.repo, "owner/img/sub", "1"); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("a name deeper than the backend takes: %v, want format.ErrInvalid", err)
	}
	if n := e.hub.count(""); n != 0 {
		t.Errorf("%d requests to the upstream, want none", n)
	}
	for path, want := range map[string]string{"nginx": "library/nginx", "library/nginx": "library/nginx", "bitnami/redis": "bitnami/redis"} {
		if got := c.OCIPath(e.repo, path); got != want {
			t.Errorf("OCIPath(%q) = %q, want %q", path, got, want)
		}
	}
	notHub := proxy.New(proxy.Options{Index: e.st, OCIUpstreams: map[string]*proxy.OCIUpstream{"hub": proxy.NewOCIUpstream("hub", nil, false)}})
	if got := notHub.OCIPath(e.repo, "nginx"); got != "nginx" {
		t.Errorf("OCIPath of an upstream that is not Docker Hub = %q", got)
	}
}

// TestOCIBuildRoutes: on a registry that creates no repository whose name ends in a build route
// (capabilities.buildRoutes, Quay), a path whose OCI repository oci_repos does not have and is one
// is 400 naming the rule, before any request to the upstream; one that oci_repos has, which the
// registry holds, is mirrored (#662).
func TestOCIBuildRoutes(t *testing.T) {
	ctx := t.Context()
	e := newOCIEnv(t, ociSettings)
	e.reg.BuildRoutes = true
	e.hub.image(t, "paketobuildpacks/build", "1.3", "amd64")
	c := e.core("a", proxy.Options{})
	_, _, err := c.OCIManifest(ctx, e.repo, "paketobuildpacks/build", "1.3")
	if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "the OCI repository oci/hub/paketobuildpacks/build ends in a build route") {
		t.Errorf("a new path that ends in build: %v, want format.ErrInvalid naming the rule", err)
	}
	if n := e.hub.count(""); n != 0 || len(e.reg.Pushes()) != 0 {
		t.Errorf("%d requests to the upstream and %d blobs pushed, want none", n, len(e.reg.Pushes()))
	}
	must(t, e.st.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: e.repo.ID, Path: "oci/hub/paketobuildpacks/build", SeenAt: e.clock()}))
	if _, _, err := c.OCIManifest(ctx, e.repo, "paketobuildpacks/build", "1.3"); err != nil {
		t.Errorf("a path oci_repos has: %v", err)
	}
	if _, _, err := c.OCIManifest(ctx, e.repo, "paketobuildpacks/builder", "1.3"); errors.Is(err, format.ErrInvalid) {
		t.Errorf("paketobuildpacks/builder, no build route: %v", err)
	}
}

// TestLoadOCIUpstreams: the oci upstreams of the configuration, with their credentials; npm's
// LoadUpstreams leaves them out.
func TestLoadOCIUpstreams(t *testing.T) {
	cfg := ociConfig()
	ups, err := proxy.LoadOCIUpstreams(cfg, nil)
	must(t, err)
	if len(ups) != 2 || ups["hub"] == nil || ups["ghcr"] == nil || ups["hub"].Name() != "hub" {
		t.Fatalf("LoadOCIUpstreams = %v", ups)
	}
	npm, err := proxy.LoadUpstreams(cfg, proxy.UpstreamOptions{})
	must(t, err)
	if len(npm) != 1 || npm["npmjs"] == nil {
		t.Fatalf("LoadUpstreams = %v, want npmjs alone", npm)
	}
	cfg[1].Auth.TokenFile = "/nonexistent/token"
	if _, err := proxy.LoadOCIUpstreams(cfg, nil); err == nil || !strings.Contains(err.Error(), "upstreams[1].auth.tokenFile") {
		t.Errorf("a missing token file: %v", err)
	}
}

// ociConfig is an npm upstream and two oci ones.
func ociConfig() []config.Upstream {
	return []config.Upstream{
		{Name: "npmjs", Format: "npm", URL: "https://registry.npmjs.org/"},
		{Name: "hub", Format: config.FormatOCI, URL: "https://registry-1.docker.io"},
		{Name: "ghcr", Format: config.FormatOCI, URL: "https://ghcr.io"},
	}
}

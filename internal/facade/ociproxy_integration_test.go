//go:build integration

package facade

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// counter is a reverse proxy in front of the upstream that counts the GETs of blobs and
// manifests it passes on.
type counter struct {
	mu    sync.Mutex
	blobs map[string]int // path → GETs
	gets  int            // GETs of manifests
}

func (c *counter) handler(target *url.URL) http.Handler {
	rp := httputil.NewSingleHostReverseProxy(target)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			c.mu.Lock()
			switch {
			case strings.Contains(r.URL.Path, "/blobs/"):
				c.blobs[r.URL.Path]++
			case strings.Contains(r.URL.Path, "/manifests/"):
				c.gets++
			}
			c.mu.Unlock()
		}
		rp.ServeHTTP(w, r)
	})
}

func (c *counter) blobGETs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.blobs {
		n += v
	}
	return n
}

// TestIntegrationOCIProxy is #442 against real registries (docs/spec/proxy-and-group.md §11):
// the backend is LAJSTROM_TEST_REGISTRY, and the upstream LAJSTROM_TEST_UPSTREAM_REGISTRY, by
// default the same registry under another repository, seeded with an image of two platforms.
// A pull of one platform keeps its blobs and its manifest under its keep-tag and leaves the
// index out, which distribution would refuse with a child missing; a pull of the other keeps
// the index and the tag; a second pull of both makes no upstream blob request.
func TestIntegrationOCIProxy(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	upAddr := cmp.Or(os.Getenv("LAJSTROM_TEST_UPSTREAM_REGISTRY"), addr)
	ctx := t.Context()
	auth := config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")}
	backend, err := generic.New(config.Registry{Name: "it", Type: generic.Type, URL: "http://" + addr,
		Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"), Auth: auth}, nil)
	must(t, err)

	// The upstream's image, pushed straight to it.
	path := "it-upstream-" + randomHex(t) + "/img"
	seed, err := generic.New(config.Registry{Name: "seed", Type: generic.Type, URL: "http://" + upAddr, Auth: auth}, nil)
	must(t, err)
	img := seedImage(t, seed, path, "1.0", "amd64", "arm64")

	target, _ := url.Parse("http://" + upAddr)
	count := &counter{blobs: map[string]int{}}
	front := httptest.NewServer(count.handler(target))
	t.Cleanup(front.Close)
	cred := generic.Credential{}
	if upAddr == addr {
		cred = generic.Credential{Username: auth.Username, Password: auth.Password}
	}
	client, err := generic.NewUpstream(config.Upstream{Name: "up", Format: config.FormatOCI, URL: front.URL}, cred, nil)
	must(t, err)

	st, err := sqlite.Open(ctx, fmt.Sprintf("file:facade-oci-it-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	repo := index.Repo{Format: "oci", Name: "hub", Registry: "it", Prefix: "oci/hub", Kind: index.KindProxy, Upstream: json.RawMessage(`{"name": "up"}`)}
	must(t, st.CreateRepo(ctx, &repo))
	providers := provider.Map{"it": backend}
	log := slog.New(slog.NewTextHandler(&logBuffer{}, nil))
	pipeline := ingest.New(ingest.Options{Providers: providers, Index: st, Log: log})
	core := proxy.New(proxy.Options{Index: st, Providers: providers, Mirror: pipeline, Log: log,
		OCIUpstreams: map[string]*proxy.OCIUpstream{"up": proxy.NewOCIUpstream("up", client, false)}})
	srv := httptest.NewServer(New(Options{Index: st, Providers: providers, Log: log, Tokens: keys.NewEphemeralTokens(), Ingest: pipeline,
		OCIProxy: core}))
	t.Cleanup(srv.Close)
	base := srv.URL + "/v2/oci/hub/" + path

	pullPlatform(t, base, "1.0", img, 1)
	// held is what tag names in the backend, waiting a little for want: the claim holder's tee
	// ends with the commit of the blob, before the push of what it completed (§11.8).
	held := func(tag string, want digest.Digest) digest.Digest {
		rr, err := backend.Repository(ctx, "oci/hub/"+path)
		must(t, err)
		var got digest.Digest
		for range 50 {
			if desc, err := rr.Resolve(ctx, tag); err == nil {
				got = desc.Digest
			}
			if got == want {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		return got
	}
	if got := held(profile.KeepTag(img.manifests[1].Digest), img.manifests[1].Digest); got != img.manifests[1].Digest {
		t.Fatalf("the arm64 manifest's keep-tag names %q in the backend", got)
	}
	if got := held("1.0", ""); got != "" {
		t.Fatalf("1.0 is in the backend before its index is complete: %s", got)
	}
	pullPlatform(t, base, "1.0", img, 0)
	if got := held("1.0", img.index.Digest); got != img.index.Digest {
		t.Errorf("1.0 names %q in the backend once both platforms are held, want the index %s", got, img.index.Digest)
	}
	before := count.blobGETs()
	pullPlatform(t, base, "1.0", img, 0)
	pullPlatform(t, base, "1.0", img, 1)
	if n := count.blobGETs() - before; n != 0 {
		t.Errorf("the second pull made %d upstream blob requests, want none", n)
	}
	if n := count.blobGETs(); n != 4 {
		t.Errorf("%d upstream blob GETs in all, want one per blob, 4", n)
	}

	// #876: kube-apiserver shares its base layer with kube-controller-manager, which the backend
	// holds; it is mounted into kube-apiserver, not fetched.
	up := strings.TrimSuffix(path, "/img")
	shared := [][]byte{[]byte(strings.Repeat("base "+randomHex(t)+"\n", 2000))}
	kcm := seedSharedImage(t, seed, up+"/kube-controller-manager", "v1.37.1", shared)
	kas := seedSharedImage(t, seed, up+"/kube-apiserver", "v1.37.1", shared)
	pullPlatform(t, srv.URL+"/v2/oci/hub/"+up+"/kube-controller-manager", "v1.37.1", kcm, 0)
	pullPlatform(t, srv.URL+"/v2/oci/hub/"+up+"/kube-apiserver", "v1.37.1", kas, 0)
	if n := count.blobGET("/v2/" + up + "/kube-apiserver/blobs/" + kas.blobs[0][1].Digest.String()); n != 0 {
		t.Errorf("the upstream was asked %d times for the layer kube-apiserver shares, want none: the backend mounts it", n)
	}

	t.Run("prefetch", func(t *testing.T) {
		ociPrefetched(t, st, backend, seed, core, repo, up, count)
	})

	// #876 the way containerd pulls, on a backend that mounts and on one that does not.
	for _, mounts := range []bool{true, false} {
		t.Run(fmt.Sprintf("completed, mounts %t", mounts), func(t *testing.T) {
			ociCompleted(t, st, backend, seed, proxy.NewOCIUpstream("up", client, false), count, mounts)
		})
	}
}

// ociCompleted: a client that holds kube-controller-manager's layers gets kube-apiserver's
// manifest and asks only for its config and its own layer. The backend then holds
// kube-apiserver whole under its keep-tag: the GET of the manifest mounts the shared layer, or,
// when the backend does not mount, the GET of the client's layer starts its fetch in the
// background; an offline repo then serves every layer of it (docs/spec/proxy-and-group.md
// §11.5, step 5).
func ociCompleted(t *testing.T, st index.Store, backend, seed provider.Provider, upstream *proxy.OCIUpstream, count *counter, mounts bool) {
	ctx := t.Context()
	name := fmt.Sprintf("k8s-%t", mounts)
	repo := index.Repo{Format: "oci", Name: name, Registry: "it", Prefix: "oci/" + name, Kind: index.KindProxy,
		Upstream: json.RawMessage(`{"name": "up"}`)}
	must(t, st.CreateRepo(ctx, &repo))
	reg := backend
	if !mounts {
		reg = noMount{backend}
	}
	providers := provider.Map{"it": reg}
	log := slog.New(slog.NewTextHandler(&logBuffer{}, nil))
	pipeline := ingest.New(ingest.Options{Providers: providers, Index: st, Log: log})
	serve := func(upstreams map[string]*proxy.OCIUpstream) string {
		core := proxy.New(proxy.Options{Index: st, Providers: providers, Mirror: pipeline, Log: log, Context: ctx, OCIUpstreams: upstreams,
			CompleteQuiet: 100 * time.Millisecond})
		srv := httptest.NewServer(New(Options{Index: st, Providers: providers, Log: log, Tokens: keys.NewEphemeralTokens(), Ingest: pipeline,
			OCIProxy: core}))
		t.Cleanup(srv.Close)
		return srv.URL + "/v2/oci/" + name + "/"
	}
	online, offline := serve(map[string]*proxy.OCIUpstream{"up": upstream}), serve(nil)

	up := "it-upstream-" + randomHex(t)
	shared := [][]byte{[]byte(strings.Repeat("base "+randomHex(t)+"\n", 2000))}
	kcm := seedSharedImage(t, seed, up+"/kube-controller-manager", "v1.37.1", shared)
	kas := seedSharedImage(t, seed, up+"/kube-apiserver", "v1.37.1", shared)
	pullPlatform(t, online+up+"/kube-controller-manager", "v1.37.1", kcm, 0)
	blobs := kas.blobs[0]
	pullBlobs(t, online+up+"/kube-apiserver", "v1.37.1", kas, 0, blobs[0], blobs[len(blobs)-1])
	m := kas.manifests[0].Digest
	rr, err := backend.Repository(ctx, "oci/"+name+"/"+up+"/kube-apiserver")
	must(t, err)
	var got digest.Digest
	for range 100 {
		if desc, err := rr.Resolve(ctx, profile.KeepTag(m)); err == nil {
			got = desc.Digest
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got != m {
		t.Fatalf("kube-apiserver's keep-tag names %q in the backend after 10 s, want its manifest", got)
	}
	want := 0
	if !mounts {
		want = 1
	}
	if n := count.blobGET("/v2/" + up + "/kube-apiserver/blobs/" + blobs[1].Digest.String()); n != want {
		t.Errorf("the upstream was asked %d times for the layer kube-apiserver shares, want %d", n, want)
	}
	pullPlatform(t, offline+up+"/kube-apiserver", "v1.37.1", kas, 0)
}

// ociPrefetched is #886: a prefetch of an image of two platforms holds both, the index and the tag
// in the backend, its configs mounted from another path; a second run is all held and asks the upstream for no blob; then, with the
// upstream gone, a server that has no upstream for the repo serves a pull of each platform whole
// (docs/spec/proxy-and-group.md §11.10).
func ociPrefetched(t *testing.T, st index.Store, backend, seed provider.Provider, core *proxy.Core, repo index.Repo, up string, count *counter) {
	ctx := t.Context()
	path := up + "/prefetched"
	img := seedImage(t, seed, path, "2.0", "amd64", "arm64")
	ref := []string{path + ":2.0"}
	results, err := core.PrefetchOCI(ctx, repo, ref, []string{"linux/amd64", "linux/arm64"})
	must(t, err)
	// The configs are those of the image at img, which the backend holds: they are mounted, held,
	// and have no result of their own.
	if len(results) != 5 {
		t.Fatalf("%d results, want 5 (the index, two manifests, their layers): %+v", len(results), results)
	}
	for _, r := range results {
		if r.Status != "fetched" {
			t.Errorf("%s %s %s: %s %d %s, want fetched", r.Kind, r.Platform, r.Digest, r.Status, r.HTTPStatus, r.Message)
		}
	}
	rr, err := backend.Repository(ctx, "oci/hub/"+path)
	must(t, err)
	if desc, err := rr.Resolve(ctx, "2.0"); err != nil || desc.Digest != img.index.Digest {
		t.Errorf("2.0 in the backend: %v, %v; want the index %s", desc.Digest, err, img.index.Digest)
	}
	before := count.blobGETs()
	results, err = core.PrefetchOCI(ctx, repo, ref, []string{"linux/amd64", "linux/arm64"})
	must(t, err)
	if len(results) != 3 || results[0].Status != "held" || results[1].Status != "held" || results[2].Status != "held" {
		t.Errorf("the second run: %+v, want the index and two manifests held", results)
	}
	if n := count.blobGETs() - before; n != 0 {
		t.Errorf("the second run made %d upstream blob requests, want none", n)
	}

	log := slog.New(slog.NewTextHandler(&logBuffer{}, nil))
	providers := provider.Map{"it": backend}
	pipeline := ingest.New(ingest.Options{Providers: providers, Index: st, Log: log})
	gone := proxy.New(proxy.Options{Index: st, Providers: providers, Mirror: pipeline, Log: log})
	srv := httptest.NewServer(New(Options{Index: st, Providers: providers, Log: log, Tokens: keys.NewEphemeralTokens(), Ingest: pipeline,
		OCIProxy: gone}))
	t.Cleanup(srv.Close)
	before = count.blobGETs()
	for i := range img.manifests {
		pullPlatform(t, srv.URL+"/v2/oci/hub/"+path, "2.0", img, i)
	}
	if n := count.blobGETs() - before; n != 0 {
		t.Errorf("the pulls without an upstream made %d upstream blob requests", n)
	}
}

// noMount is a backend that opens an upload session instead of a mount (202), as Nexus's
// docker format and JFrog do.
type noMount struct{ provider.Provider }

func (n noMount) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := n.Provider.Repository(ctx, name)
	return noMountRepository{rr}, err
}

type noMountRepository struct{ provider.Repository }

func (r noMountRepository) MountBlob(ctx context.Context, _ digest.Digest, _ string) (bool, provider.Upload, error) {
	u, err := r.StartBlobUpload(ctx)
	return false, u, err
}

// blobGET is how many GETs of the blob at urlPath the counter passed on.
func (c *counter) blobGET(urlPath string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blobs[urlPath]
}

// seedSharedImage pushes to the repository path of p an index of one platform, linux/arm64,
// whose manifest lists a config, the layers shared and a layer of its own, in that order.
func seedSharedImage(t *testing.T, p provider.Provider, path, tag string, shared [][]byte) itImage {
	t.Helper()
	ctx := t.Context()
	rr, err := p.Repository(ctx, path)
	must(t, err)
	push := func(mediaType string, b []byte) ocispec.Descriptor {
		desc := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(b), Size: int64(len(b))}
		must(t, rr.Push(ctx, desc, bytes.NewReader(b)))
		return desc
	}
	blobs := []ocispec.Descriptor{push(ocispec.MediaTypeImageConfig, []byte(`{"architecture":"arm64","os":"linux","image":"`+path+`"}`))}
	for _, b := range shared {
		blobs = append(blobs, push(ocispec.MediaTypeImageLayerGzip, b))
	}
	blobs = append(blobs, push(ocispec.MediaTypeImageLayerGzip, []byte(strings.Repeat(path+" "+randomHex(t)+"\n", 2000))))
	raw, err := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		Config: blobs[0], Layers: blobs[1:]})
	must(t, err)
	m := push(ocispec.MediaTypeImageManifest, raw)
	child := m
	child.Platform = &ocispec.Platform{OS: "linux", Architecture: "arm64"}
	raw, err = json.Marshal(ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{child}})
	must(t, err)
	img := itImage{index: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromBytes(raw), Size: int64(len(raw))},
		manifests: []ocispec.Descriptor{m}, blobs: [][]ocispec.Descriptor{blobs}}
	must(t, rr.PushReference(ctx, img.index, bytes.NewReader(raw), tag))
	return img
}

// itImage is an index of one manifest per platform and their blobs.
type itImage struct {
	index     ocispec.Descriptor
	manifests []ocispec.Descriptor
	blobs     [][]ocispec.Descriptor
}

// seedImage pushes an image of platforms to the repository path of p, the index under tag.
func seedImage(t *testing.T, p provider.Provider, path, tag string, platforms ...string) itImage {
	t.Helper()
	ctx := t.Context()
	rr, err := p.Repository(ctx, path)
	must(t, err)
	push := func(mediaType string, b []byte) ocispec.Descriptor {
		desc := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(b), Size: int64(len(b))}
		must(t, rr.Push(ctx, desc, bytes.NewReader(b)))
		return desc
	}
	var img itImage
	var children []ocispec.Descriptor
	for _, arch := range platforms {
		config := push(ocispec.MediaTypeImageConfig, []byte(`{"architecture":"`+arch+`","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`))
		layer := push(ocispec.MediaTypeImageLayerGzip, []byte(strings.Repeat(arch+" "+randomHex(t)+"\n", 2000)))
		raw, err := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
			Config: config, Layers: []ocispec.Descriptor{layer}})
		must(t, err)
		m := push(ocispec.MediaTypeImageManifest, raw)
		img.manifests, img.blobs = append(img.manifests, m), append(img.blobs, []ocispec.Descriptor{config, layer})
		child := m
		child.Platform = &ocispec.Platform{OS: "linux", Architecture: arch}
		children = append(children, child)
	}
	raw, err := json.Marshal(ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: children})
	must(t, err)
	img.index = ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	must(t, rr.PushReference(ctx, img.index, bytes.NewReader(raw), tag))
	return img
}

// pullPlatform pulls the platform i of img from base as docker does, checking every body
// against its digest: the tag, the platform's manifest, its config and layers.
func pullPlatform(t *testing.T, base, tag string, img itImage, i int) {
	t.Helper()
	pullBlobs(t, base, tag, img, i, img.blobs[i]...)
}

// pullBlobs is pullPlatform that asks for blobs only, as containerd asks only for the blobs it
// does not hold.
func pullBlobs(t *testing.T, base, tag string, img itImage, i int, blobs ...ocispec.Descriptor) {
	t.Helper()
	accept := strings.Join([]string{ocispec.MediaTypeImageIndex, ocispec.MediaTypeImageManifest}, ", ")
	get := func(what, url string, want digest.Digest) {
		t.Helper()
		resp, body := request(t, http.MethodGet, url, nil, 0, "Accept", accept)
		if resp.StatusCode != http.StatusOK || digest.FromBytes(body) != want {
			t.Fatalf("GET %s: %d, %d bytes of %s, want %s: %s", what, resp.StatusCode, len(body), digest.FromBytes(body), want, body)
		}
	}
	get("the tag", base+"/manifests/"+tag, img.index.Digest)
	get("the platform's manifest", base+"/manifests/"+img.manifests[i].Digest.String(), img.manifests[i].Digest)
	for _, b := range blobs {
		get("a blob", base+"/blobs/"+b.Digest.String(), b.Digest)
	}
}

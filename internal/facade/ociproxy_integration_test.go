//go:build integration

package facade

import (
	"bytes"
	"cmp"
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
	held := func(tag string) digest.Digest {
		rr, err := backend.Repository(ctx, "oci/hub/"+path)
		must(t, err)
		desc, err := rr.Resolve(ctx, tag)
		if err != nil {
			return ""
		}
		return desc.Digest
	}
	if got := held(profile.KeepTag(img.manifests[1].Digest)); got != img.manifests[1].Digest {
		t.Fatalf("the arm64 manifest's keep-tag names %q in the backend", got)
	}
	if got := held("1.0"); got != "" {
		t.Fatalf("1.0 is in the backend before its index is complete: %s", got)
	}
	pullPlatform(t, base, "1.0", img, 0)
	if got := held("1.0"); got != img.index.Digest {
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
// against its digest: the tag, the platform's manifest, its config and layer.
func pullPlatform(t *testing.T, base, tag string, img itImage, i int) {
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
	for _, b := range img.blobs[i] {
		get("a blob", base+"/blobs/"+b.Digest.String(), b.Digest)
	}
}

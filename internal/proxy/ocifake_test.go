package proxy_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// hub is a registry of the distribution API with Docker Hub's ways (docs/spec/proxy-and-group.md
// §11.7): anonymous bearer tokens from a realm on its own host, a 401 after the token for a
// repository it does not have, the ratelimit-* headers on every answer, a 429 with Retry-After
// when tooMany is set, and multi-arch indexes. It logs its requests to /v2/ that carry the
// token: the challenges of the token flow are not reads.
type hub struct {
	srv       *httptest.Server
	mu        sync.Mutex
	docs      map[string]map[string]hubDoc // repository → tag or digest → manifest or index
	blobs     map[string]map[digest.Digest][]byte
	requests  []string // "HEAD /v2/library/nginx/manifests/1.27"
	tooMany   atomic.Bool
	corrupted map[digest.Digest]bool // manifests answered with other bytes
}

type hubDoc struct {
	mediaType string
	raw       []byte
}

func newHub(t *testing.T) *hub {
	t.Helper()
	h := &hub{docs: map[string]map[string]hubDoc{}, blobs: map[string]map[digest.Digest][]byte{}, corrupted: map[digest.Digest]bool{}}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "anon"})
		return
	}
	w.Header().Set("ratelimit-limit", "100;w=21600")
	w.Header().Set("ratelimit-remaining", "90;w=21600")
	if r.Header.Get("Authorization") != "Bearer anon" {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="hub"`, h.srv.URL+"/token"))
		hubError(w, http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}
	h.mu.Lock()
	h.requests = append(h.requests, r.Method+" "+r.URL.Path)
	h.mu.Unlock()
	if h.tooMany.Load() {
		w.Header().Set("Retry-After", "7")
		hubError(w, http.StatusTooManyRequests, "TOOMANYREQUESTS")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	if i := strings.LastIndex(rest, "/manifests/"); i > 0 {
		h.serveDoc(w, r, rest[:i], rest[i+len("/manifests/"):])
		return
	}
	if i := strings.LastIndex(rest, "/blobs/"); i > 0 {
		h.serveBlob(w, r, rest[:i], digest.Digest(rest[i+len("/blobs/"):]))
		return
	}
	http.NotFound(w, r)
}

func (h *hub) serveDoc(w http.ResponseWriter, r *http.Request, repo, ref string) {
	h.mu.Lock()
	docs, known := h.docs[repo]
	doc, ok := docs[ref]
	corrupt := h.corrupted[digest.FromBytes(doc.raw)]
	h.mu.Unlock()
	switch {
	case !known: // Docker Hub: a token without pull, then 401 for the read
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="hub",error="insufficient_scope"`, h.srv.URL+"/token"))
		hubError(w, http.StatusUnauthorized, "UNAUTHORIZED")
		return
	case !ok:
		hubError(w, http.StatusNotFound, "MANIFEST_UNKNOWN")
		return
	}
	raw := doc.raw
	if corrupt && r.Method == http.MethodGet {
		raw = append([]byte(" "), raw[1:]...)
	}
	w.Header().Set("Content-Type", doc.mediaType)
	w.Header().Set("Docker-Content-Digest", digest.FromBytes(doc.raw).String())
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(raw)
	}
}

func (h *hub) serveBlob(w http.ResponseWriter, r *http.Request, repo string, d digest.Digest) {
	h.mu.Lock()
	b, ok := h.blobs[repo][d]
	h.mu.Unlock()
	if !ok {
		hubError(w, http.StatusNotFound, "BLOB_UNKNOWN")
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Header().Set("Docker-Content-Digest", d.String())
	if r.Method == http.MethodGet {
		_, _ = w.Write(b)
	}
}

func hubError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": code}}})
}

// count returns how many requests of the log start with prefix.
func (h *hub) count(prefix string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// put stores a manifest or an index at repo under its digest and the tags.
func (h *hub) put(repo, mediaType string, raw []byte, tags ...string) ocispec.Descriptor {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.docs[repo] == nil {
		h.docs[repo] = map[string]hubDoc{}
	}
	d := digest.FromBytes(raw)
	for _, ref := range append(tags, d.String()) {
		h.docs[repo][ref] = hubDoc{mediaType: mediaType, raw: raw}
	}
	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(raw))}
}

// blob stores a blob at repo.
func (h *hub) blob(repo, mediaType string, b []byte) ocispec.Descriptor {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.blobs[repo] == nil {
		h.blobs[repo] = map[digest.Digest][]byte{}
	}
	d := digest.FromBytes(b)
	h.blobs[repo][d] = b
	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(b))}
}

// image is an index of one manifest per platform, each of a config and one layer, stored at
// repo under tag; the index and its manifests in the order of platforms.
type image struct {
	index     ocispec.Descriptor
	manifests []ocispec.Descriptor
	blobs     [][]ocispec.Descriptor // per manifest: config, layer
}

func (h *hub) image(t *testing.T, repo, tag string, platforms ...string) image {
	t.Helper()
	var img image
	var children []ocispec.Descriptor
	for _, arch := range platforms {
		config := h.blob(repo, ocispec.MediaTypeImageConfig, []byte(`{"architecture":"`+arch+`","tag":"`+tag+`"}`))
		layer := h.blob(repo, ocispec.MediaTypeImageLayerGzip, []byte(strings.Repeat(arch+tag, 100)))
		raw, err := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
			Config: config, Layers: []ocispec.Descriptor{layer}})
		must(t, err)
		m := h.put(repo, ocispec.MediaTypeImageManifest, raw)
		img.manifests, img.blobs = append(img.manifests, m), append(img.blobs, []ocispec.Descriptor{config, layer})
		child := m
		child.Platform = &ocispec.Platform{OS: "linux", Architecture: arch}
		children = append(children, child)
	}
	raw, err := json.Marshal(ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: children})
	must(t, err)
	img.index = h.put(repo, ocispec.MediaTypeImageIndex, raw, tag)
	return img
}

// ociEnv is an oci proxy repo, oci/hub, of the upstream hub on a fake backend, with a clock.
type ociEnv struct {
	st   index.Store
	reg  *providertest.Fake
	hub  *hub
	repo index.Repo
	pipe *ingest.Pipeline
	up   *proxy.OCIUpstream
	now  atomic.Int64
}

func newOCIEnv(t *testing.T, settings string) *ociEnv {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:ociproxy-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	e := &ociEnv{st: st, reg: &providertest.Fake{RegistryName: "local"}, hub: newHub(t)}
	e.now.Store(time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC).UnixNano())
	e.repo = index.Repo{Format: "oci", Name: "hub", Registry: "local", Prefix: "oci/hub", Kind: index.KindProxy, Upstream: json.RawMessage(settings)}
	must(t, st.CreateRepo(ctx, &e.repo))
	client, err := generic.NewUpstream(config.Upstream{Name: "hub", Format: config.FormatOCI, URL: e.hub.srv.URL, Retries: -1}, generic.Credential{},
		slog.New(slog.DiscardHandler))
	must(t, err)
	e.up = proxy.NewOCIUpstream("hub", client, true)
	e.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": e.reg}, Index: st, Log: slog.New(slog.DiscardHandler)})
	return e
}

func (e *ociEnv) clock() time.Time        { return time.Unix(0, e.now.Load()).UTC() }
func (e *ociEnv) advance(d time.Duration) { e.now.Add(int64(d)) }

// core is a Core of the env on replica, with o's changes.
func (e *ociEnv) core(replica string, o proxy.Options) *proxy.Core {
	o.Index, o.Providers, o.Now, o.Replica = e.st, provider.Map{"local": e.reg}, e.clock, replica
	o.OCIUpstreams = map[string]*proxy.OCIUpstream{"hub": e.up}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	o.WaitFirst, o.WaitLongest = time.Millisecond, 5*time.Millisecond
	return proxy.New(o)
}

// row returns the proxy_meta row of kind at path, nil when there is none.
func (e *ociEnv) row(t *testing.T, path, kind string) *index.ProxyMeta {
	t.Helper()
	r, err := e.st.GetProxyMeta(t.Context(), e.repo.ID, path, kind)
	if err != nil {
		return nil
	}
	return r
}

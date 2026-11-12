package proxy_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// await waits up to 5 s for cond, and fails the test naming what without it.
func await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not after 5 s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fetches is the count of result of lajstrom_proxy_fetches_total for the upstream of ociEnv.
func fetches(result string) float64 {
	return testutil.ToFloat64(metrics.ProxyFetches.WithLabelValues("hub", result))
}

// keepTagged waits for the keep-tag of d at path in the backend.
func (e *ociEnv) keepTagged(t *testing.T, path string, d digest.Digest) {
	t.Helper()
	await(t, "the keep-tag of "+d.String()+" at "+path, func() bool { return e.tagged(t, path, profile.KeepTag(d)) == d })
}

// asked is a client's GET of the blob d at path through c, as the facade reports it: it begins
// and ends at once.
func (e *ociEnv) asked(t *testing.T, c *proxy.Core, path string, d ocispec.Descriptor) {
	c.OCIPulling(t.Context(), e.repo, path, d.Digest)()
}

// own is the layer of img's platform i of its own, the last.
func own(img image, i int) ocispec.Descriptor {
	return img.blobs[i][len(img.blobs[i])-1]
}

// TestOCICompleteShared is #876: a client that holds kube-controller-manager's layers gets
// kube-apiserver's manifest and only its config and its own layer; the GET of the manifest
// mounts the layers the two share, with no request to the upstream, and kube-apiserver is held
// whole under its keep-tag; an offline repo then serves every layer of it to a node with an
// empty store (docs/spec/proxy-and-group.md §11.5, step 5).
func TestOCICompleteShared(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	a := e.hub.sharedImage(t, kcm, "v1.37.1", k8sShared, "arm64")
	b := e.hub.sharedImage(t, kas, "v1.37.1", k8sShared, "arm64")
	c := e.core("a", proxy.Options{CompleteQuiet: -1})
	pull(t, e, c, kcm, "v1.37.1", a, 0)
	pullOwn(t, e, c, kas, "v1.37.1", b, 0)
	mounted := fetches("mounted")
	c.OCIComplete(t.Context(), e.repo, kas, b.manifests[0])
	e.keepTagged(t, kas, b.manifests[0].Digest)
	await(t, "v1.37.1, its index of one platform, in the backend", func() bool { return e.tagged(t, kas, "v1.37.1") == b.index.Digest })
	if n := e.blobGETs(kas, b.blobs[0][1:3]...); n != 0 {
		t.Errorf("the upstream was asked %d times for the layers kube-apiserver shares, want none", n)
	}
	if n := fetches("mounted") - mounted; n != 2 {
		t.Errorf("lajstrom_proxy_fetches_total{result=\"mounted\"} rose by %v, want 2: the shared layers", n)
	}
	off := e.repo
	off.Upstream = json.RawMessage(`{"name": "hub", "offline": true}`)
	asked := e.hub.count("")
	_, _, err := c.OCIManifest(t.Context(), off, kas, "v1.37.1")
	must(t, err)
	for _, bl := range b.blobs[0] {
		if _, _, err := c.OCIBlob(t.Context(), off, kas, bl.Digest, true, false); err != nil {
			t.Fatalf("offline, OCIBlob %s: %v", bl.Digest, err)
		}
	}
	if n := e.hub.count("") - asked; n != 0 {
		t.Errorf("the offline repo sent the upstream %d requests", n)
	}
}

// TestOCICompleteNotPulled: a GET of a manifest and of its config that no GET of a layer follows,
// as Renovate reads an image's labels, mounts what the backend holds at another path and fetches
// nothing else.
func TestOCICompleteNotPulled(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	a := e.hub.sharedImage(t, kcm, "v1.37.1", k8sShared, "arm64")
	b := e.hub.sharedImage(t, kas, "v1.37.1", k8sShared, "arm64")
	c := e.core("a", proxy.Options{CompleteQuiet: -1})
	pull(t, e, c, kcm, "v1.37.1", a, 0)
	mounted, completed := fetches("mounted"), fetches("completed")
	_, _, err := c.OCIManifest(t.Context(), e.repo, kas, "v1.37.1")
	must(t, err)
	m, _, err := c.OCIManifest(t.Context(), e.repo, kas, b.manifests[0].Digest.String())
	must(t, err)
	c.OCIComplete(t.Context(), e.repo, kas, m)
	config := b.blobs[0][0]
	e.asked(t, c, kas, config)
	_, _, err = c.OCIBlob(t.Context(), e.repo, kas, config.Digest, true, false)
	must(t, err)
	await(t, "the shared layers mounted", func() bool { return fetches("mounted")-mounted == 2 })
	time.Sleep(200 * time.Millisecond) // a fetch, were one started, would run now
	if n := e.blobGETs(kas, b.blobs[0][1:]...); n != 0 {
		t.Errorf("the upstream was asked %d times for layers, want none: no client asked for one", n)
	}
	if n := fetches("completed") - completed; n != 0 {
		t.Errorf("the completion fetched %v blobs, want none", n)
	}
	if got := e.tagged(t, kas, profile.KeepTag(m.Digest)); got != "" {
		t.Errorf("kube-apiserver is in the backend without its own layer: %s", got)
	}
}

// TestOCICompleteLayerAsked: a GET of the manifest and of one layer completes the rest: the
// blobs no client asked for and the backend does not hold anywhere are fetched.
func TestOCICompleteLayerAsked(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	b := e.hub.sharedImage(t, kas, "v1.37.1", k8sShared, "arm64")
	c := e.core("a", proxy.Options{CompleteQuiet: -1})
	m, _, err := c.OCIManifest(t.Context(), e.repo, kas, b.manifests[0].Digest.String())
	must(t, err)
	c.OCIComplete(t.Context(), e.repo, kas, m)
	completed := fetches("completed")
	layer := own(b, 0)
	_, _, err = c.OCIBlob(t.Context(), e.repo, kas, layer.Digest, true, false)
	must(t, err)
	e.asked(t, c, kas, layer)
	e.keepTagged(t, kas, m.Digest)
	for _, bl := range b.blobs[0] {
		if n := e.blobGETs(kas, bl); n != 1 {
			t.Errorf("the upstream was asked %d times for %s, want once", n, bl.Digest)
		}
	}
	// counted once each fetch's flight ended, after the push the last one made
	await(t, `lajstrom_proxy_fetches_total{result="completed"} up by 3: the config and the shared layers`, func() bool {
		return fetches("completed")-completed == 3
	})
}

// TestOCICompleteAmbiguousLayer: a GET of a layer that two manifests cached at the path list
// does not tell which one a client pulls, and starts neither; a GET of a layer of one's own
// completes that one alone.
func TestOCICompleteAmbiguousLayer(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	const path = "library/app"
	shared := e.hub.blob(path, ocispec.MediaTypeImageLayerGzip, []byte(strings.Repeat("base ", 200)))
	var ms [2]ocispec.Descriptor
	var owns [2]ocispec.Descriptor
	for i, v := range []string{"1.0", "2.0"} {
		config := e.hub.blob(path, ocispec.MediaTypeImageConfig, []byte(`{"version":"`+v+`"}`))
		owns[i] = e.hub.blob(path, ocispec.MediaTypeImageLayerGzip, []byte(strings.Repeat("app "+v, 100)))
		raw, err := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
			Config: config, Layers: []ocispec.Descriptor{shared, owns[i]}})
		must(t, err)
		ms[i] = e.hub.put(path, ocispec.MediaTypeImageManifest, raw, v)
	}
	c := e.core("a", proxy.Options{CompleteQuiet: -1})
	for _, m := range ms {
		_, _, err := c.OCIManifest(t.Context(), e.repo, path, m.Digest.String())
		must(t, err)
	}
	e.asked(t, c, path, shared)
	time.Sleep(200 * time.Millisecond) // a fetch, were one started, would run now
	if n := e.hub.count("GET /v2/" + path + "/blobs/"); n != 0 {
		t.Fatalf("a GET of the layer both versions list fetched %d blobs, want none", n)
	}
	e.asked(t, c, path, owns[0])
	e.keepTagged(t, path, ms[0].Digest)
	if n := e.blobGETs(path, owns[1]); n != 0 {
		t.Errorf("the layer of 2.0's own was fetched %d times, want none: no client pulled 2.0", n)
	}
	if got := e.tagged(t, path, profile.KeepTag(ms[1].Digest)); got != "" {
		t.Errorf("2.0 is in the backend: %s", got)
	}
}

// ksched is the third image of the control plane's static pods.
const ksched = "k8s/kube-scheduler"

// sharedFetched counts the upstream's GETs of the layer d across the three paths.
func (e *ociEnv) sharedFetched(d ocispec.Descriptor) int {
	return e.blobGETs(kcm, d) + e.blobGETs(kas, d) + e.blobGETs(ksched, d)
}

// TestOCICompleteSharedLater is the second Talos boot of #876: the clients of kube-apiserver and
// kube-scheduler ask for a layer while kube-controller-manager's shared layers are not held
// yet; their completions wait for the paths to be quiet, and kube-controller-manager's fetches
// the shared layers meanwhile. When the wait ends they mount the shared layers from
// kube-controller-manager instead of fetching them again.
func TestOCICompleteSharedLater(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	imgs := map[string]image{}
	for _, path := range []string{kcm, kas, ksched} {
		imgs[path] = e.hub.sharedImage(t, path, "v1.37.1", k8sShared, "arm64")
	}
	mounted := fetches("mounted")
	waiting := e.core("a", proxy.Options{CompleteQuiet: 20 * time.Millisecond})
	var pulled []func()
	for _, path := range []string{kas, ksched} {
		m, _, err := waiting.OCIManifest(t.Context(), e.repo, path, imgs[path].manifests[0].Digest.String())
		must(t, err)
		waiting.OCIComplete(t.Context(), e.repo, path, m)
		// the client's GET of its own layer runs until kube-controller-manager is held
		pulled = append(pulled, waiting.OCIPulling(t.Context(), e.repo, path, own(imgs[path], 0).Digest))
	}
	c := e.core("b", proxy.Options{CompleteQuiet: -1})
	m, _, err := c.OCIManifest(t.Context(), e.repo, kcm, imgs[kcm].manifests[0].Digest.String())
	must(t, err)
	c.OCIComplete(t.Context(), e.repo, kcm, m)
	e.asked(t, c, kcm, own(imgs[kcm], 0))
	e.keepTagged(t, kcm, m.Digest)
	for _, done := range pulled {
		done() // the paths' last requests end: the waits run out
	}
	for _, path := range []string{kas, ksched} {
		e.keepTagged(t, path, imgs[path].manifests[0].Digest)
	}
	for _, d := range imgs[kas].blobs[0][1:3] {
		if n := e.sharedFetched(d); n != 1 {
			t.Errorf("the shared layer %s was fetched %d times, want once", d.Digest, n)
		}
	}
	if n := fetches("mounted") - mounted; n != 4 {
		t.Errorf("lajstrom_proxy_fetches_total{result=\"mounted\"} rose by %v, want 4: two layers at two paths", n)
	}
}

// TestOCICompleteSharedConcurrent: completions at three paths that share layers, on two
// replicas at once, fetch each shared layer once, under the repo's claim of its digest; the
// others wait and mount it.
func TestOCICompleteSharedConcurrent(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	imgs := map[string]image{}
	for _, path := range []string{kcm, kas, ksched} {
		imgs[path] = e.hub.sharedImage(t, path, "v1.37.1", k8sShared, "arm64")
	}
	gated := imgs[kcm].blobs[0][1]
	gate := make(chan struct{})
	e.hub.mu.Lock()
	e.hub.gates[gated.Digest] = gate
	e.hub.mu.Unlock()
	cores := []*proxy.Core{e.core("a", proxy.Options{CompleteQuiet: -1}), e.core("b", proxy.Options{CompleteQuiet: -1})}
	mounted := fetches("mounted")
	for i, path := range []string{kcm, kas, ksched} {
		m, _, err := cores[i%2].OCIManifest(t.Context(), e.repo, path, imgs[path].manifests[0].Digest.String())
		must(t, err)
		cores[i%2].OCIComplete(t.Context(), e.repo, path, m)
		e.asked(t, cores[i%2], path, own(imgs[path], 0))
	}
	await(t, "a GET of the gated layer", func() bool { return e.sharedFetched(gated) > 0 })
	time.Sleep(200 * time.Millisecond) // the other completions reach the layer meanwhile
	close(gate)
	for _, path := range []string{kcm, kas, ksched} {
		e.keepTagged(t, path, imgs[path].manifests[0].Digest)
	}
	for _, d := range imgs[kcm].blobs[0][1:3] {
		if n := e.sharedFetched(d); n != 1 {
			t.Errorf("the shared layer %s was fetched %d times, want once", d.Digest, n)
		}
	}
	if n := fetches("mounted") - mounted; n != 4 {
		t.Errorf("lajstrom_proxy_fetches_total{result=\"mounted\"} rose by %v, want 4: two layers at two paths", n)
	}
}

// TestOCICompleteFetches: on a backend that does not mount, the client's GET of a layer starts
// the fetch of the shared layers in the background, each once.
func TestOCICompleteFetches(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	a := e.hub.sharedImage(t, kcm, "v1.37.1", k8sShared, "arm64")
	b := e.hub.sharedImage(t, kas, "v1.37.1", k8sShared, "arm64")
	c := e.coreOn(noMount{e.reg}, "a", proxy.Options{CompleteQuiet: -1})
	pull(t, e, c, kcm, "v1.37.1", a, 0)
	pullOwn(t, e, c, kas, "v1.37.1", b, 0)
	c.OCIComplete(t.Context(), e.repo, kas, b.manifests[0])
	e.asked(t, c, kas, own(b, 0))
	e.keepTagged(t, kas, b.manifests[0].Digest)
	for _, bl := range b.blobs[0] {
		if n := e.blobGETs(kas, bl); n != 1 {
			t.Errorf("the upstream was asked %d times for %s, want once", n, bl.Digest)
		}
	}
	if n := e.reg.OpenUploads(); n != 0 {
		t.Errorf("%d upload sessions left open", n)
	}
}

// TestOCICompletePlatform: the completion of a platform leaves the other platforms of its index
// alone, and a GET of an index completes nothing.
func TestOCICompletePlatform(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64", "arm64")
	c := e.core("a", proxy.Options{CompleteQuiet: -1})
	idx, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27")
	must(t, err)
	arm, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[1].Digest.String())
	must(t, err)
	c.OCIComplete(t.Context(), e.repo, "library/nginx", idx)
	c.OCIComplete(t.Context(), e.repo, "library/nginx", arm)
	e.asked(t, c, "library/nginx", own(img, 1))
	e.keepTagged(t, "library/nginx", arm.Digest)
	if n := e.blobGETs("library/nginx", img.blobs[0]...); n != 0 {
		t.Errorf("the upstream was asked %d times for the blobs of amd64, which no client got", n)
	}
	if got := e.tagged(t, "library/nginx", profile.KeepTag(img.index.Digest)); got != "" {
		t.Errorf("the index is in the backend with a platform missing: %s", got)
	}
}

// TestOCICompleteOnce: completions of one manifest on two replicas fetch each blob once (INV-31).
func TestOCICompleteOnce(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	cores := []*proxy.Core{e.core("a", proxy.Options{CompleteQuiet: -1}), e.core("b", proxy.Options{CompleteQuiet: -1})}
	m, _, err := cores[0].OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() { e.asked(t, cores[i%2], "library/nginx", own(img, 0)) })
	}
	wg.Wait()
	e.keepTagged(t, "library/nginx", m.Digest)
	for _, bl := range img.blobs[0] {
		if n := e.blobGETs("library/nginx", bl); n != 1 {
			t.Errorf("the upstream was asked %d times for %s, want once", n, bl.Digest)
		}
	}
}

// TestOCICompleteBound: a blob above maxFileSize is left out of the completion, with no
// proxy.reject, and the manifest stays incomplete; the blobs within the bound are fetched.
func TestOCICompleteBound(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.core("a", proxy.Options{CompleteQuiet: -1})
	m, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	repo := e.repo
	repo.Policy = []byte(`{"maxFileSize": 100}`) // the config's 35 bytes, not the layer's 900
	config, layer := img.blobs[0][0], img.blobs[0][1]
	c.OCIComplete(t.Context(), repo, "library/nginx", m)
	c.OCIPulling(t.Context(), repo, "library/nginx", layer.Digest)()
	await(t, "the config held", func() bool { return e.row(t, "library/nginx", "oci.held:"+config.Digest.String()) != nil })
	if n := e.blobGETs("library/nginx", layer); n != 0 {
		t.Errorf("the layer above maxFileSize was fetched %d times", n)
	}
	if got := e.tagged(t, "library/nginx", profile.KeepTag(m.Digest)); got != "" {
		t.Errorf("the manifest is in the backend without its layer: %s", got)
	}
	if evs, err := e.st.ListAudit(t.Context(), index.AuditFilter{Action: "proxy.reject"}); err != nil || len(evs) != 0 {
		t.Errorf("proxy.reject rows = %+v, %v", evs, err)
	}
}

// TestOCICompleteWaits: the completion fetches nothing while a client of the replica pulls a
// blob of the path, nor for CompleteQuiet after; then it does.
func TestOCICompleteWaits(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.core("a", proxy.Options{CompleteQuiet: 50 * time.Millisecond})
	m, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	c.OCIComplete(t.Context(), e.repo, "library/nginx", m)
	done := c.OCIPulling(t.Context(), e.repo, "library/nginx", own(img, 0).Digest)
	time.Sleep(300 * time.Millisecond)
	if n := e.blobGETs("library/nginx", img.blobs[0]...); n != 0 {
		t.Fatalf("the completion fetched %d blobs while a client pulled", n)
	}
	done()
	e.keepTagged(t, "library/nginx", m.Digest)
}

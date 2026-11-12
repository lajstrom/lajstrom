package proxy_test

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
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

// keepTagged waits for the keep-tag of d at path in the backend.
func (e *ociEnv) keepTagged(t *testing.T, path string, d digest.Digest) {
	t.Helper()
	await(t, "the keep-tag of "+d.String()+" at "+path, func() bool { return e.tagged(t, path, profile.KeepTag(d)) == d })
}

// TestOCICompleteShared is #876: a client that holds kube-controller-manager's layers gets
// kube-apiserver's manifest and only its own layer; the completion of that manifest mounts the
// layers the two share, with no request to the upstream, and kube-apiserver is held whole
// under its keep-tag; an offline repo then serves every layer of it to a node with an empty
// store (docs/spec/proxy-and-group.md §11.5, step 5).
func TestOCICompleteShared(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	a := e.hub.sharedImage(t, kcm, "v1.37.1", k8sShared, "arm64")
	b := e.hub.sharedImage(t, kas, "v1.37.1", k8sShared, "arm64")
	c := e.core("a", proxy.Options{CompleteQuiet: -1})
	pull(t, e, c, kcm, "v1.37.1", a, 0)
	pullOwn(t, e, c, kas, "v1.37.1", b, 0)
	c.OCIComplete(t.Context(), e.repo, kas, b.manifests[0])
	e.keepTagged(t, kas, b.manifests[0].Digest)
	await(t, "v1.37.1, its index of one platform, in the backend", func() bool { return e.tagged(t, kas, "v1.37.1") == b.index.Digest })
	if n := e.blobGETs(kas, b.blobs[0][1:3]...); n != 0 {
		t.Errorf("the upstream was asked %d times for the layers kube-apiserver shares, want none", n)
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

// TestOCICompleteFetches: on a backend that does not mount, the completion fetches the shared
// layers in the background, each once.
func TestOCICompleteFetches(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	a := e.hub.sharedImage(t, kcm, "v1.37.1", k8sShared, "arm64")
	b := e.hub.sharedImage(t, kas, "v1.37.1", k8sShared, "arm64")
	c := e.coreOn(noMount{e.reg}, "a", proxy.Options{CompleteQuiet: -1})
	pull(t, e, c, kcm, "v1.37.1", a, 0)
	pullOwn(t, e, c, kas, "v1.37.1", b, 0)
	c.OCIComplete(t.Context(), e.repo, kas, b.manifests[0])
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

// TestOCICompletePlatform: the completion of a platform's manifest leaves the other platforms of
// its index alone, and a GET of an index completes nothing.
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
		wg.Go(func() { cores[i%2].OCIComplete(t.Context(), e.repo, "library/nginx", m) })
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
	c.OCIComplete(t.Context(), repo, "library/nginx", m)
	config, layer := img.blobs[0][0], img.blobs[0][1]
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
	done := c.OCIPulling(e.repo, "library/nginx")
	c.OCIComplete(t.Context(), e.repo, "library/nginx", m)
	time.Sleep(300 * time.Millisecond)
	if n := e.blobGETs("library/nginx", img.blobs[0]...); n != 0 {
		t.Fatalf("the completion fetched %d blobs while a client pulled", n)
	}
	done()
	e.keepTagged(t, "library/nginx", m.Digest)
}

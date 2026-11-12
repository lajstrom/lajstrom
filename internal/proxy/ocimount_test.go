package proxy_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// The images of #876: kube-apiserver and kube-controller-manager of one release share most of
// their layers.
const (
	kcm = "k8s/kube-controller-manager"
	kas = "k8s/kube-apiserver"
)

var k8sShared = [][]byte{[]byte(strings.Repeat("distroless base ", 200)), []byte(strings.Repeat("go runtime ", 200))}

// pullOwn pulls the platform i of img through c as containerd does when it holds the layers img
// shares with another image: the tag, the platform's manifest, its config and its last layer.
func pullOwn(t *testing.T, e *ociEnv, c *proxy.Core, path, tag string, img image, i int) {
	t.Helper()
	ctx := t.Context()
	_, _, err := c.OCIManifest(ctx, e.repo, path, tag)
	must(t, err)
	_, _, err = c.OCIManifest(ctx, e.repo, path, img.manifests[i].Digest.String())
	must(t, err)
	blobs := img.blobs[i]
	for _, b := range []ocispec.Descriptor{blobs[0], blobs[len(blobs)-1]} {
		if _, _, err := c.OCIBlob(ctx, e.repo, path, b.Digest, true, false); err != nil {
			t.Fatalf("OCIBlob %s: %v", b.Digest, err)
		}
	}
}

// blobGETs counts the upstream's GETs of blobs at path.
func (e *ociEnv) blobGETs(path string, blobs ...ocispec.Descriptor) int {
	n := 0
	for _, b := range blobs {
		n += e.hub.count("GET /v2/" + path + "/blobs/" + b.Digest.String())
	}
	return n
}

// holders returns the paths of the repo's oci.holders row of desc.
func (e *ociEnv) holders(t *testing.T, desc ocispec.Descriptor) []string {
	t.Helper()
	row := e.row(t, "", "oci.holders:"+desc.Digest.String())
	if row == nil {
		return nil
	}
	var h struct {
		Paths []string `json:"paths"`
	}
	must(t, json.Unmarshal(row.Content, &h))
	return h.Paths
}

// TestOCIBlobMount: a blob the backend holds at another path of the repo is mounted into the
// path of a miss, with no request to the upstream; the repo's oci.holders row lists every path
// that holds it (docs/spec/proxy-and-group.md §11.5, step 2).
func TestOCIBlobMount(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	a := e.hub.sharedImage(t, kcm, "v1.37.1", k8sShared, "arm64")
	b := e.hub.sharedImage(t, kas, "v1.37.1", k8sShared, "arm64")
	c := e.core("a", proxy.Options{})
	pull(t, e, c, kcm, "v1.37.1", a, 0)
	mounted := fetches("mounted")
	pull(t, e, c, kas, "v1.37.1", b, 0)
	if n := fetches("mounted") - mounted; n != 2 {
		t.Errorf("lajstrom_proxy_fetches_total{result=\"mounted\"} rose by %v, want 2", n)
	}
	shared, own := b.blobs[0][1:3], []ocispec.Descriptor{b.blobs[0][0], b.blobs[0][3]}
	if n := e.blobGETs(kas, shared...); n != 0 {
		t.Errorf("the upstream was asked %d times for the layers kube-apiserver shares, want none: they are mounted", n)
	}
	if n := e.blobGETs(kas, own...); n != 2 {
		t.Errorf("the upstream was asked %d times for the config and the layer of kube-apiserver's own, want 2", n)
	}
	if got := e.tagged(t, kas, profile.KeepTag(b.manifests[0].Digest)); got != b.manifests[0].Digest {
		t.Errorf("kube-apiserver's keep-tag names %q, want its manifest", got)
	}
	if got := e.holders(t, shared[0]); !slices.Equal(got, []string{kcm, kas}) {
		t.Errorf("the oci.holders row of a shared layer lists %q", got)
	}
	if got := e.holders(t, own[1]); !slices.Equal(got, []string{kas}) {
		t.Errorf("the oci.holders row of kube-apiserver's own layer lists %q", got)
	}
}

// TestOCIBlobMountOffline: an offline repo mounts, since it asks the backend and not the
// upstream: a client that holds kube-controller-manager's layers got kube-apiserver's manifest
// and its own layer; offline, a node with an empty store gets every layer, and kube-apiserver is
// complete in the backend (#876).
func TestOCIBlobMountOffline(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	a := e.hub.sharedImage(t, kcm, "v1.37.1", k8sShared, "arm64")
	b := e.hub.sharedImage(t, kas, "v1.37.1", k8sShared, "arm64")
	c := e.core("a", proxy.Options{})
	pull(t, e, c, kcm, "v1.37.1", a, 0)
	pullOwn(t, e, c, kas, "v1.37.1", b, 0)
	keep := profile.KeepTag(b.manifests[0].Digest)
	if got := e.tagged(t, kas, keep); got != "" {
		t.Fatalf("kube-apiserver is complete before its shared layers are held: %s", got)
	}
	off := e.repo
	off.Upstream = json.RawMessage(`{"name": "hub", "offline": true}`)
	asked := e.hub.count("")
	for _, bl := range b.blobs[0] {
		if _, _, err := c.OCIBlob(t.Context(), off, kas, bl.Digest, true, false); err != nil {
			t.Fatalf("offline, OCIBlob %s: %v", bl.Digest, err)
		}
	}
	if n := e.hub.count("") - asked; n != 0 {
		t.Errorf("the offline repo sent the upstream %d requests", n)
	}
	if got := e.tagged(t, kas, keep); got != b.manifests[0].Digest {
		t.Errorf("offline, kube-apiserver's keep-tag names %q once every layer is held, want its manifest", got)
	}
}

// TestOCIBlobMountRefused: on a backend that opens a session instead of a mount, the blob is
// fetched from the upstream and the session cancelled; offline, it is a 404.
func TestOCIBlobMountRefused(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	a := e.hub.sharedImage(t, kcm, "v1.37.1", k8sShared, "arm64")
	b := e.hub.sharedImage(t, kas, "v1.37.1", k8sShared, "arm64")
	c := e.coreOn(noMount{e.reg}, "a", proxy.Options{})
	pull(t, e, c, kcm, "v1.37.1", a, 0)
	pullOwn(t, e, c, kas, "v1.37.1", b, 0)
	off := e.repo
	off.Upstream = json.RawMessage(`{"name": "hub", "offline": true}`)
	shared := b.blobs[0][1]
	if _, _, err := c.OCIBlob(t.Context(), off, kas, shared.Digest, true, false); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("offline, a blob the backend does not mount: %v, want index.ErrNotFound", err)
	}
	pull(t, e, c, kas, "v1.37.1", b, 0)
	if n := e.blobGETs(kas, b.blobs[0][1:3]...); n != 2 {
		t.Errorf("the upstream was asked %d times for the shared layers, want one each", n)
	}
	if n := e.reg.OpenUploads(); n != 0 {
		t.Errorf("%d upload sessions left open", n)
	}
	if got := e.tagged(t, kas, profile.KeepTag(b.manifests[0].Digest)); got != b.manifests[0].Digest {
		t.Errorf("kube-apiserver's keep-tag names %q, want its manifest", got)
	}
}

package facade

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
)

// longName is base and components of at most 61 bytes, n bytes in all.
func longName(base string, n int) string {
	rest := n - len(base) - 1
	p := strings.Repeat("a", (rest-1)%61+1)
	for len(p) < rest {
		p = strings.Repeat("a", 60) + "/" + p
	}
	return base + "/" + p
}

// TestUploadNameLength: a /v2 blob upload or manifest push into a name longer than the registry
// takes below its prefix, 255 bytes less the prefix's length (the generic provider's
// MaxNameLength), is 400 NAME_INVALID naming the limit and not the prefix, a managed and an oci
// repo alike, and nothing reaches the registry; a name of exactly the limit uploads (#661).
func TestUploadNameLength(t *testing.T) {
	e := newEnv(t, envOptions{})
	const limit = 255 - len("lajstrom/")
	e.prov.MaxNameLength = limit
	b := []byte("a few bytes")
	d := digest.FromBytes(b).String()
	body, _ := manifest(t, []byte("layer"), "long")
	raw, oci := longName("raw/assets", limit+1), longName("oci/images", limit+1)
	for _, req := range []struct{ method, target, name string }{
		{http.MethodPost, "/v2/" + raw + "/blobs/uploads/", raw},
		{http.MethodPost, "/v2/" + raw + "/blobs/uploads/?digest=" + d, raw},
		{http.MethodPut, "/v2/" + oci + "/manifests/v1", oci},
	} {
		in := bytes.NewReader(b)
		if req.method == http.MethodPut {
			in = bytes.NewReader(body)
		}
		msg := wantError(t, req.method+" "+req.target, e.do(e.h, req.method, req.target, in, "Content-Type", ocispecManifest),
			http.StatusBadRequest, codeNameInvalid)
		if want := fmt.Sprintf("%s is %d bytes long, and the registry takes at most %d", req.name, limit+1, limit); !strings.Contains(msg, want) {
			t.Errorf("%s %s: message %q does not contain %q", req.method, req.target, msg, want)
		}
		if strings.Contains(msg, "lajstrom/") {
			t.Errorf("%s %s: message %q names the prefix", req.method, req.target, msg)
		}
	}
	if n := e.prov.OpenUploads(); n != 0 || len(e.prov.Pushes()) != 0 || len(e.prov.PushReferences()) != 0 {
		t.Errorf("%d backend sessions open, %d blobs and %d manifests pushed for a name the registry does not take", n,
			len(e.prov.Pushes()), len(e.prov.PushReferences()))
	}
	wantStatus(t, "a name of the limit", e.do(e.h, http.MethodPost, "/v2/"+longName("raw/assets", limit)+"/blobs/uploads/?digest="+d,
		bytes.NewReader(b)), http.StatusCreated)
	wantStatus(t, "an oci name of the limit", e.do(e.h, http.MethodPut, "/v2/"+longName("oci/images", limit)+"/manifests/v1",
		bytes.NewReader(body), "Content-Type", ocispecManifest), http.StatusCreated)
}

package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// maxBlobRegistry is the fake with a capabilities.maxBlobSize.
type maxBlobRegistry struct {
	*providertest.Fake
	max int64
}

func (r maxBlobRegistry) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	c, err := r.Fake.Capabilities(ctx)
	c.MaxBlobSize = r.max
	return c, err
}

// TestUploadMaxBlobSize: the registry's capabilities.maxBlobSize bounds a /v2 blob upload when
// it is below the repo's maxFileSize, and on an oci repo, which maxFileSize does not bound; the
// 413 names the bound that refused, for a declared size and for a body cut where it passes it
// (#246).
func TestUploadMaxBlobSize(t *testing.T) {
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider { return maxBlobRegistry{Fake: f, max: 8} }})
	e.raw.Policy = json.RawMessage(`{"maxFileSize":10}`)
	must(t, e.st.UpdateRepo(t.Context(), &e.raw))
	const raw = "/v2/raw/assets/docs/big.txt/blobs/uploads/"
	nine, eight := []byte("nine byte"), []byte("8 bytes!")
	for _, base := range []string{raw, uploads} {
		post := func(b []byte, length string) *httptest.ResponseRecorder {
			return e.do(e.h, http.MethodPost, base+"?digest="+digest.FromBytes(b).String(), bytes.NewReader(b), "Content-Length", length)
		}
		for _, length := range []string{"9", "-1"} {
			msg := wantError(t, base+" POST of 9 bytes, length "+length, post(nine, length), http.StatusRequestEntityTooLarge, codeSizeInvalid)
			if !strings.Contains(msg, "the maxBlobSize of registry local, 8 bytes") {
				t.Errorf("%s: message %q does not name the registry's bound", base, msg)
			}
		}
		wantStatus(t, base+" POST of 8 bytes", post(eight, "8"), http.StatusCreated)
	}
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d backend sessions open", n)
	}

	e.raw.Policy = json.RawMessage(`{"maxFileSize":4}`)
	must(t, e.st.UpdateRepo(t.Context(), &e.raw))
	msg := wantError(t, "maxFileSize below maxBlobSize", e.do(e.h, http.MethodPost, raw+"?digest="+digest.FromBytes(eight).String(),
		bytes.NewReader(eight)), http.StatusRequestEntityTooLarge, codeSizeInvalid)
	if !strings.Contains(msg, "the maxFileSize of raw/assets, 4 bytes") {
		t.Errorf("message %q does not name the repo's bound", msg)
	}
}

// TestUploadNameDepth: a /v2 blob upload or manifest push into a name deeper than the
// registry's maxNameDepth is 400 NAME_INVALID naming the limit, a session, a monolithic upload,
// a mount and a manifest of a managed or an oci repo alike, and nothing reaches the registry; a
// name within it uploads (#599).
func TestUploadNameDepth(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.prov.MaxNameDepth = 4
	b := []byte("a few bytes")
	d := digest.FromBytes(b).String()
	body, _ := manifest(t, []byte("layer"), "deep")
	const deep = "/v2/raw/assets/ci/build/app.tar/blobs/uploads/"
	for _, req := range []struct{ method, target, name string }{
		{http.MethodPost, deep, "raw/assets/ci/build/app.tar"},
		{http.MethodPost, deep + "?digest=" + d, "raw/assets/ci/build/app.tar"},
		{http.MethodPost, deep + "?mount=" + d + "&from=raw/assets/x", "raw/assets/ci/build/app.tar"},
		{http.MethodPut, "/v2/raw/assets/ci/build/app.tar/manifests/1.0.0", "raw/assets/ci/build/app.tar"},
		{http.MethodPut, "/v2/oci/images/team/app/x/manifests/v1", "oci/images/team/app/x"},
	} {
		in := bytes.NewReader(b)
		if req.method == http.MethodPut {
			in = bytes.NewReader(body)
		}
		msg := wantError(t, req.method+" "+req.target, e.do(e.h, req.method, req.target, in, "Content-Type", ocispecManifest),
			http.StatusBadRequest, codeNameInvalid)
		if want := req.name + " has 5 components, and the registry takes at most 4 below its prefix"; !strings.Contains(msg, want) {
			t.Errorf("%s %s: message %q does not contain %q", req.method, req.target, msg, want)
		}
	}
	if n := e.prov.OpenUploads(); n != 0 || len(e.prov.Pushes()) != 0 || len(e.prov.PushReferences()) != 0 {
		t.Errorf("%d backend sessions open, %d blobs and %d manifests pushed for a name the registry does not take", n,
			len(e.prov.Pushes()), len(e.prov.PushReferences()))
	}
	wantStatus(t, "a name of 4 components", e.do(e.h, http.MethodPost, "/v2/raw/assets/ci/app.tar/blobs/uploads/?digest="+d,
		bytes.NewReader(b)), http.StatusCreated)
	wantStatus(t, "an oci name of 4 components", e.do(e.h, http.MethodPut, "/v2/oci/images/team/app/manifests/v1",
		bytes.NewReader(body), "Content-Type", ocispecManifest), http.StatusCreated)
}

// TestUploadDefaultMaxFileSize: a repo whose policy sets no maxFileSize is bounded by its
// format's default on /v2 too, 8 GiB for raw and 16 GiB for oci (#246): a blob that declares one
// byte more is 413 before anything streams.
func TestUploadDefaultMaxFileSize(t *testing.T) {
	e := newEnv(t, envOptions{})
	b := []byte("a few bytes")
	for _, tt := range []struct{ base, length, bound string }{
		{"/v2/raw/assets/docs/big.iso/blobs/uploads/", "8589934593", "the maxFileSize of raw/assets, 8589934592 bytes"},
		{uploads, "17179869185", "the maxFileSize of oci/images, 17179869184 bytes"},
	} {
		rr := e.do(e.h, http.MethodPost, tt.base+"?digest="+digest.FromBytes(b).String(), bytes.NewReader(b), "Content-Length", tt.length)
		msg := wantError(t, tt.base+": the default and a byte", rr, http.StatusRequestEntityTooLarge, codeSizeInvalid)
		if !strings.Contains(msg, tt.bound) {
			t.Errorf("message %q does not name the default bound %q", msg, tt.bound)
		}
	}
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d backend sessions open", n)
	}
}

package facade

import (
	"bytes"
	"context"
	"crypto/sha512"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

func TestBlobReads(t *testing.T) {
	e := newEnv(t, envOptions{})
	b := []byte("a blob to read")
	d := e.pushBlob("oci/images/app", b)
	blob := "/v2/oci/images/app/blobs/" + d.String()
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		rr := e.do(e.h, method, blob, nil)
		wantStatus(t, method, rr, http.StatusOK)
		wantHeader(t, method, rr, "Content-Length", fmt.Sprint(len(b)))
		wantHeader(t, method, rr, "Docker-Content-Digest", d.String())
		wantHeader(t, method, rr, "Content-Type", octetStream)
		wantHeader(t, method, rr, "Accept-Ranges", "bytes")
		want := map[string]string{http.MethodHead: "", http.MethodGet: string(b)}[method]
		if rr.Body.String() != want {
			t.Errorf("%s body %q, want %q", method, rr.Body, want)
		}
	}
	rr := e.do(e.h, http.MethodGet, blob, nil, "Range", "bytes=2-5")
	wantStatus(t, "GET with a Range", rr, http.StatusOK)
	if rr.Body.String() != string(b) {
		t.Errorf("GET with a Range: body %q, want the whole blob (#27)", rr.Body)
	}
	wantError(t, "HEAD of a blob that does not exist", e.do(e.h, http.MethodHead, "/v2/oci/images/app/blobs/"+zeroDigest.String(), nil),
		http.StatusNotFound, codeBlobUnknown)
	wantError(t, "GET of a blob of another repository", e.do(e.h, http.MethodGet, "/v2/oci/images/other/blobs/"+d.String(), nil),
		http.StatusNotFound, codeBlobUnknown)

	raw := e.pushBlob("raw/assets/docs/file.txt", []byte("raw bytes"))
	rr = e.do(e.h, http.MethodGet, "/v2/raw/assets/docs/file.txt/blobs/"+raw.String(), nil)
	wantStatus(t, "GET of a raw repo's blob", rr, http.StatusOK)

	// A provider whose reader does not tell the size: no Content-Length, the bytes stream.
	e = newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider { return unseekable{f} }})
	d = e.pushBlob("oci/images/app", b)
	rr = e.do(e.h, http.MethodGet, "/v2/oci/images/app/blobs/"+d.String(), nil)
	wantHeader(t, "unsized", rr, "Content-Length", "")
	if rr.Body.String() != string(b) {
		t.Errorf("unsized body %q", rr.Body)
	}
}

// unseekable is the fake whose readers are not io.Seekers.
type unseekable struct{ *providertest.Fake }

func (u unseekable) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := u.Fake.Repository(ctx, name)
	return unseekableRepository{rr}, err
}

type unseekableRepository struct{ provider.Repository }

func (r unseekableRepository) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	rc, err := r.Repository.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(rc), nil
}

func TestManifestPassThrough(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	body, d := manifest(t, []byte("layer"), "first")
	put := func(ref string, b []byte, hdr ...string) *httptest.ResponseRecorder {
		hdr = append([]string{"Content-Type", ocispecManifest}, hdr...)
		return e.do(e.h, http.MethodPut, "/v2/oci/images/app/manifests/"+ref, bytes.NewReader(b), hdr...)
	}
	rr := put("v1", body)
	wantStatus(t, "PUT by tag", rr, http.StatusCreated)
	wantHeader(t, "PUT by tag", rr, "Location", "/v2/oci/images/app/manifests/"+d.String())
	wantHeader(t, "PUT by tag", rr, "Docker-Content-Digest", d.String())
	if refs := e.prov.PushReferences(); len(refs) != 1 || refs[0].Name != "oci/images/app" || refs[0].Tag != "v1" || refs[0].Digest != d {
		t.Errorf("PushReferences = %+v", refs)
	}
	ocis, err := e.st.ListOCIRepos(ctx, e.oci.ID)
	must(t, err)
	if len(ocis) != 1 || ocis[0].Path != "oci/images/app" {
		t.Errorf("oci_repos = %+v, want oci/images/app", ocis)
	}

	for _, ref := range []string{"v1", d.String()} {
		for _, method := range []string{http.MethodHead, http.MethodGet} {
			// The Accept of a client that lists another type changes nothing.
			rr := e.do(e.h, method, "/v2/oci/images/app/manifests/"+ref, nil, "Accept", "application/vnd.docker.distribution.manifest.v2+json")
			what := method + " " + ref
			wantStatus(t, what, rr, http.StatusOK)
			wantHeader(t, what, rr, "Content-Type", ocispecManifest)
			wantHeader(t, what, rr, "Docker-Content-Digest", d.String())
			wantHeader(t, what, rr, "Content-Length", fmt.Sprint(len(body)))
			if method == http.MethodGet && !bytes.Equal(rr.Body.Bytes(), body) {
				t.Errorf("%s: body %q", what, rr.Body)
			}
		}
	}
	wantError(t, "GET of a tag that does not exist", e.do(e.h, http.MethodGet, "/v2/oci/images/app/manifests/v9", nil),
		http.StatusNotFound, codeManifestUnknown)
	wantError(t, "HEAD of a digest that does not exist", e.do(e.h, http.MethodHead, "/v2/oci/images/app/manifests/"+zeroDigest.String(), nil),
		http.StatusNotFound, codeManifestUnknown)

	// By digest, in sha256 and sha512.
	body2, d2 := manifest(t, []byte("layer"), "second")
	wantStatus(t, "PUT by digest", put(d2.String(), body2), http.StatusCreated)
	if pushes := e.prov.Pushes(); len(pushes) != 1 || pushes[0].Digest != d2 {
		t.Errorf("Pushes = %+v", pushes)
	}
	sum := sha512.Sum512(body2)
	d512 := digest.NewDigestFromBytes(digest.SHA512, sum[:])
	rr = put(d512.String(), body2)
	wantStatus(t, "PUT by a sha512 digest", rr, http.StatusCreated)
	wantHeader(t, "PUT by a sha512 digest", rr, "Docker-Content-Digest", d512.String())
	wantError(t, "PUT by another digest", put(d.String(), body2), http.StatusBadRequest, codeDigestInvalid)

	// What the pass-through refuses.
	wantError(t, "not a manifest type", put("v2", body, "Content-Type", "application/json"), http.StatusBadRequest, codeManifestInvalid)
	wantError(t, "not JSON", put("v2", []byte("{oops")), http.StatusBadRequest, codeManifestInvalid)
	wantError(t, "mediaType against Content-Type", put("v2", body, "Content-Type", ocispec.MediaTypeImageIndex), http.StatusBadRequest, codeManifestInvalid)
	wantError(t, "no Content-Length", put("v2", body, "Content-Length", "-1"), http.StatusLengthRequired, codeManifestInvalid)
	wantError(t, "too large", put("v2", body, "Content-Length", fmt.Sprint(defaultMaxManifestSize+1)), http.StatusRequestEntityTooLarge, codeManifestInvalid)
	wantError(t, "body shorter than Content-Length", put("v2", body, "Content-Length", fmt.Sprint(len(body)+5)), http.StatusBadRequest, codeManifestInvalid)

	e.prov.FailPushReference = fmt.Errorf("the tag is immutable: %w", provider.ErrConflict)
	wantError(t, "a tag the backend will not move", put("v1", body2), http.StatusConflict, codeTagInvalid)
	e.prov.FailPushReference = fmt.Errorf("the backend is down")
	wantError(t, "a backend that fails", put("v3", body2), http.StatusInternalServerError, codeUnknown)
}

// TestManagedManifestRead: a manifest of a repo Lajstrom manages is read from the backend too.
func TestManagedManifestRead(t *testing.T) {
	e := newEnv(t, envOptions{})
	body, d := manifest(t, []byte("raw file"), "raw")
	rr, err := e.prov.Repository(t.Context(), "raw/assets/hello")
	must(t, err)
	must(t, rr.PushReference(t.Context(), ocispec.Descriptor{MediaType: ocispecManifest, Digest: d, Size: int64(len(body))}, bytes.NewReader(body), "1"))
	res := e.do(e.h, http.MethodGet, "/v2/raw/assets/hello/manifests/1", nil)
	wantStatus(t, "GET", res, http.StatusOK)
	if !strings.Contains(res.Body.String(), `"raw"`) {
		t.Errorf("body %q", res.Body)
	}
}

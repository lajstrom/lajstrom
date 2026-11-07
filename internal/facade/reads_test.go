package facade

import (
	"bytes"
	"context"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestBlobReads checks HEAD and GET of a blob through format.ServeBlob: one StatBlob or one
// OpenBlob, the headers, the client's Range passed to the backend and its 206 and 416, the 304
// of If-None-Match without a request, BLOB_UNKNOWN for a blob the backend does not have.
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
		wantHeader(t, method, rr, "ETag", `"`+d.String()+`"`)
		wantHeader(t, method, rr, apiVersionHeader, "registry/2.0")
		want := map[string]string{http.MethodHead: "", http.MethodGet: string(b)}[method]
		if rr.Body.String() != want {
			t.Errorf("%s body %q, want %q", method, rr.Body, want)
		}
	}
	if o, s := e.prov.OpenBlobs(), e.prov.StatBlobs(); len(o) != 1 || len(s) != 1 || o[0] != (providertest.BlobRead{Name: "oci/images/app", Digest: d}) {
		t.Errorf("HEAD and GET: OpenBlobs %+v, StatBlobs %+v; want one each", o, s)
	}

	rr := e.do(e.h, http.MethodGet, blob, nil, "Range", "bytes=2-5")
	wantStatus(t, "GET with a Range", rr, http.StatusPartialContent)
	wantHeader(t, "GET with a Range", rr, "Content-Range", "bytes 2-5/14")
	wantHeader(t, "GET with a Range", rr, "Content-Length", "4")
	if o := e.prov.OpenBlobs(); rr.Body.String() != "blob" || o[len(o)-1].Range != "bytes=2-5" {
		t.Errorf("GET with a Range: body %q, the backend got %+v", rr.Body, o[len(o)-1])
	}
	rr = e.do(e.h, http.MethodGet, blob, nil, "Range", "bytes=14-")
	wantStatus(t, "GET with a range past the end", rr, http.StatusRequestedRangeNotSatisfiable)
	wantHeader(t, "GET with a range past the end", rr, "Content-Range", "bytes */14")

	calls := len(e.prov.OpenBlobs()) + len(e.prov.StatBlobs())
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		wantStatus(t, method+" with If-None-Match", e.do(e.h, method, blob, nil, "If-None-Match", `"`+d.String()+`"`), http.StatusNotModified)
	}
	if n := len(e.prov.OpenBlobs()) + len(e.prov.StatBlobs()); n != calls {
		t.Errorf("a 304 asked the backend: %d calls, want %d", n, calls)
	}

	wantError(t, "HEAD of a blob that does not exist", e.do(e.h, http.MethodHead, "/v2/oci/images/app/blobs/"+zeroDigest.String(), nil),
		http.StatusNotFound, codeBlobUnknown)
	msg := wantError(t, "GET of a blob of another repository", e.do(e.h, http.MethodGet, "/v2/oci/images/other/blobs/"+d.String(), nil),
		http.StatusNotFound, codeBlobUnknown)
	if !strings.HasPrefix(msg, "blob unknown to registry") {
		t.Errorf("message %q", msg)
	}

	raw := e.pushBlob("raw/assets/docs/file.txt", []byte("raw bytes"))
	rr = e.do(e.h, http.MethodGet, "/v2/raw/assets/docs/file.txt/blobs/"+raw.String(), nil)
	wantStatus(t, "GET of a raw repo's blob", rr, http.StatusOK)
}

// TestBlobRedirect is the policy switch of #27 on the facade: GET is a 307 to the URL the
// provider gives, with Docker-Content-Digest, while HEAD answers the headers; with
// policy.blobRedirect never on the repo, or capabilities.blobRedirect false on the registry,
// GET streams.
func TestBlobRedirect(t *testing.T) {
	e := newEnv(t, envOptions{})
	b := []byte("a redirected blob")
	d := e.pushBlob("oci/images/app", b)
	blob := "/v2/oci/images/app/blobs/" + d.String()
	e.prov.RedirectURL = func(name string, d digest.Digest) (string, bool) {
		return "https://objects.test/" + name + "/" + d.Encoded() + "?sig=1", true
	}
	rr := e.do(e.h, http.MethodGet, blob, nil, "Range", "bytes=0-3")
	wantStatus(t, "GET", rr, http.StatusTemporaryRedirect)
	wantHeader(t, "GET", rr, "Location", "https://objects.test/oci/images/app/"+d.Encoded()+"?sig=1")
	wantHeader(t, "GET", rr, "Docker-Content-Digest", d.String())
	rr = e.do(e.h, http.MethodHead, blob, nil)
	wantStatus(t, "HEAD", rr, http.StatusOK)
	wantHeader(t, "HEAD", rr, "Content-Length", fmt.Sprint(len(b)))

	stream := func(what string) {
		t.Helper()
		rr := e.do(e.h, http.MethodGet, blob, nil)
		wantStatus(t, what, rr, http.StatusOK)
		wantHeader(t, what, rr, "Location", "")
		if rr.Body.String() != string(b) {
			t.Errorf("%s: body %q", what, rr.Body)
		}
	}
	setPolicy := func(policy string) {
		t.Helper()
		repo := e.oci
		repo.Policy = []byte(policy)
		must(t, e.st.UpdateRepo(t.Context(), &repo))
	}
	setPolicy(`{"blobRedirect": "never"}`)
	stream("GET with policy.blobRedirect never")
	setPolicy(`{"blobRedirect": "auto"}`)
	wantStatus(t, "GET with policy.blobRedirect auto", e.do(e.h, http.MethodGet, blob, nil), http.StatusTemporaryRedirect)
	e.prov.NoBlobRedirect = true
	stream("GET from a registry with capabilities.blobRedirect false")
}

// TestBlobReadFailures checks a backend error before the response (502 UNKNOWN, the cause
// logged and not answered) and a stream that breaks after the headers (logged with the request
// id).
func TestBlobReadFailures(t *testing.T) {
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider { return &brokenStream{Fake: f} }})
	b := []byte("0123456789")
	d := e.pushBlob("oci/images/app", b)
	blob := "/v2/oci/images/app/blobs/" + d.String()
	rr := e.do(e.h, http.MethodGet, blob, nil)
	wantStatus(t, "a stream that breaks", rr, http.StatusOK)
	if rr.Body.String() != "0" || !strings.Contains(e.logs.String(), "facade: the blob stream failed after the response started") {
		t.Errorf("body %q; log:\n%s", rr.Body, e.logs)
	}
	msg := wantError(t, "a backend that refuses Lajstrom", e.do(e.h, http.MethodHead, blob, nil), http.StatusBadGateway, codeUnknown)
	if strings.Contains(msg, "backend.example") || !strings.Contains(e.logs.String(), "backend.example") {
		t.Errorf("message %q; the cause must be logged, not answered:\n%s", msg, e.logs)
	}
}

// brokenStream is the fake whose blobs break after their first byte and whose StatBlob fails
// as a backend that rejects Lajstrom's credentials.
type brokenStream struct{ *providertest.Fake }

func (p *brokenStream) OpenBlob(ctx context.Context, name string, d digest.Digest, rangeHeader string, redirect bool) (provider.Blob, error) {
	b, err := p.Fake.OpenBlob(ctx, name, d, rangeHeader, redirect)
	if err != nil || b.Body == nil {
		return b, err
	}
	b.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(io.LimitReader(b.Body, 1), iotest.ErrReader(errors.New("connection reset"))), b.Body}
	return b, nil
}

func (p *brokenStream) StatBlob(context.Context, string, digest.Digest) (provider.Blob, error) {
	return provider.Blob{}, fmt.Errorf("HEAD https://backend.example/v2/x: %w", provider.ErrUnauthorized)
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

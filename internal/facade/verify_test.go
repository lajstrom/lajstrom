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
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// laxRegistry is the fake as a backend that checks no digest, as JFrog JCR (#368): the close of
// a session takes the last chunk and stores the bytes under whatever digest it is given, which
// stored records with their size.
type laxRegistry struct {
	*providertest.Fake
	stored sync.Map
}

func (f *laxRegistry) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.Fake.Repository(ctx, name)
	return laxRepository{Repository: rr, f: f}, err
}

type laxRepository struct {
	provider.Repository
	f *laxRegistry
}

func (r laxRepository) FinishBlobUpload(ctx context.Context, u provider.Upload, d digest.Digest, rd io.Reader, size int64) (ocispec.Descriptor, error) {
	u, err := r.AppendBlobChunk(ctx, u, rd, size)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if err := r.CancelBlobUpload(ctx, u); err != nil { // the fake's session ends: the bytes are "stored"
		return ocispec.Descriptor{}, err
	}
	r.f.stored.Store(d, u.Offset)
	return ocispec.Descriptor{MediaType: octetStream, Digest: d, Size: u.Offset}, nil
}

func newLaxEnv(t *testing.T) (*env, *laxRegistry) {
	t.Helper()
	var lax *laxRegistry
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider {
		lax = &laxRegistry{Fake: f}
		return lax
	}})
	return e, lax
}

// storedUnder reports whether the lax backend stored a blob under d.
func storedUnder(lax *laxRegistry, d digest.Digest) bool {
	_, ok := lax.stored.Load(d)
	return ok
}

// wantNoUpload fails the test when the upload id (none for "") has a row or the backend a session.
func (e *env) wantNoUpload(what, id string) {
	e.t.Helper()
	if n := e.prov.OpenUploads(); n != 0 {
		e.t.Errorf("%s: %d backend sessions open", what, n)
	}
	if _, err := e.st.GetSession(e.t.Context(), e.oci.ID, uploadKind+id); id != "" && err == nil {
		e.t.Errorf("%s: the session's row is still there", what)
	}
}

// TestUploadVerifiesDigest: on a backend that checks no digest, every way of uploading a blob is
// 400 DIGEST_INVALID with a digest its bytes do not have, the backend stores nothing under it,
// and no session is left; with the right digest each is 201.
func TestUploadVerifiesDigest(t *testing.T) {
	e, lax := newLaxEnv(t)
	blob := []byte("the bytes of a blob, in chunks or not")
	chunks := func(last string, hdr ...string) func(digest.Digest) (*httptest.ResponseRecorder, string) {
		return func(d digest.Digest) (*httptest.ResponseRecorder, string) {
			id := e.start(e.h)
			wantStatus(t, "chunk", e.do(e.h, http.MethodPatch, uploads+id, bytes.NewReader(blob[:5])), http.StatusAccepted)
			wantStatus(t, "chunk", e.do(e.h, http.MethodPatch, uploads+id, bytes.NewReader(blob[5:9]), "Content-Length", "-1"), http.StatusAccepted)
			body := io.Reader(strings.NewReader(last))
			if last == "" { // the rest as a chunk too
				wantStatus(t, "chunk", e.do(e.h, http.MethodPatch, uploads+id, bytes.NewReader(blob[9:])), http.StatusAccepted)
				body = nil
			}
			return e.do(e.h, http.MethodPut, uploads+id+"?digest="+d.String(), body, hdr...), id
		}
	}
	rest := string(blob[9:])
	for _, tt := range []struct {
		name string
		send func(digest.Digest) (*httptest.ResponseRecorder, string)
	}{
		{"monolithic POST", func(d digest.Digest) (*httptest.ResponseRecorder, string) {
			return e.do(e.h, http.MethodPost, uploads+"?digest="+d.String(), bytes.NewReader(blob)), ""
		}},
		{"streamed monolithic POST", func(d digest.Digest) (*httptest.ResponseRecorder, string) {
			return e.do(e.h, http.MethodPost, uploads+"?digest="+d.String(), bytes.NewReader(blob), "Content-Length", "-1"), ""
		}},
		{"POST, PUT", func(d digest.Digest) (*httptest.ResponseRecorder, string) {
			id := e.start(e.h)
			return e.do(e.h, http.MethodPut, uploads+id+"?digest="+d.String(), bytes.NewReader(blob)), id
		}},
		{"chunks, empty PUT", chunks("")},
		{"chunks, last chunk in the PUT", chunks(rest, "Content-Range", fmt.Sprintf("9-%d", len(blob)-1))},
		{"chunks, streamed last chunk", chunks(rest, "Content-Length", "-1")},
	} {
		bad := digest.FromString(tt.name)
		rr, id := tt.send(bad)
		wantError(t, tt.name+", wrong digest", rr, http.StatusBadRequest, codeDigestInvalid)
		if storedUnder(lax, bad) {
			t.Errorf("%s: the backend stored the bytes under the wrong digest", tt.name)
		}
		e.wantNoUpload(tt.name, id)
		rr, _ = tt.send(digest.FromBytes(blob))
		wantStatus(t, tt.name+", right digest", rr, http.StatusCreated)
	}

	// A chunked upload is checked in sha256; sha512 goes in one request.
	sum := sha512.Sum512(blob)
	d512 := digest.NewDigestFromBytes(digest.SHA512, sum[:])
	rr, id := chunks(rest)(d512)
	wantError(t, "chunks closed with sha512", rr, http.StatusBadRequest, codeDigestInvalid)
	e.wantNoUpload("chunks closed with sha512", id)
	wantStatus(t, "sha512 in one request", e.do(e.h, http.MethodPost, uploads+"?digest="+d512.String(), bytes.NewReader(blob)), http.StatusCreated)
}

// TestUploadVerifiedAcrossReplicas: the PATCHes of a session go to two replicas, each continuing
// the hash from the row, and the close checks the whole blob: its digest is 201, the digest of
// the bytes after the first chunk, the hash a replica without the row's would have, is 400.
func TestUploadVerifiedAcrossReplicas(t *testing.T) {
	e, lax := newLaxEnv(t)
	a, b := e.h, e.replica()
	chunks := []string{"on A, ", "on B, ", "and on B again, ", "closed on A"}
	whole := strings.Join(chunks, "")
	for _, d := range []digest.Digest{digest.FromString(whole[len(chunks[0]):]), digest.FromString(whole)} {
		id := e.start(a)
		off := 0
		for i, c := range chunks[:3] {
			h := map[bool]http.Handler{true: b, false: a}[i > 0]
			rr := e.do(h, http.MethodPatch, uploads+id, strings.NewReader(c), "Content-Range", fmt.Sprintf("%d-%d", off, off+len(c)-1))
			wantStatus(t, fmt.Sprintf("chunk %d", i), rr, http.StatusAccepted)
			off += len(c)
		}
		rr := e.do(a, http.MethodPut, uploads+id+"?digest="+d.String(), strings.NewReader(chunks[3]),
			"Content-Range", fmt.Sprintf("%d-%d", off, len(whole)-1))
		if d == digest.FromString(whole) {
			wantStatus(t, "the whole blob's digest", rr, http.StatusCreated)
			continue
		}
		wantError(t, "the digest without the first chunk", rr, http.StatusBadRequest, codeDigestInvalid)
		if storedUnder(lax, d) {
			t.Error("the backend stored the bytes under the digest without the first chunk")
		}
		e.wantNoUpload("the digest without the first chunk", id)
	}
}

// TestVerifierShortReads: the verifier holds one byte back whatever the size of the reads, and
// passes the blob on whole only when it has the digest.
func TestVerifierShortReads(t *testing.T) {
	blob := []byte("held back, one byte at a time")
	for _, size := range []int{1, 2, 7, 64} {
		for _, want := range []digest.Digest{digest.FromBytes(blob), digest.FromString("other")} {
			v := &verifier{r: bytes.NewReader(blob), h: digest.SHA256.Hash(), want: want}
			var got []byte
			buf := make([]byte, size)
			var err error
			for err == nil {
				var n int
				n, err = v.Read(buf)
				got = append(got, buf[:n]...)
			}
			if want == digest.FromBytes(blob) && (!errors.Is(err, io.EOF) || !bytes.Equal(got, blob)) {
				t.Errorf("reads of %d: %q, %v; want the blob and EOF", size, got, err)
			}
			if want != digest.FromBytes(blob) && (!errors.Is(err, errNotDigest) || len(got) != len(blob)-1) {
				t.Errorf("reads of %d, wrong digest: %d bytes, %v; want all but one and errNotDigest", size, len(got), err)
			}
		}
	}
}

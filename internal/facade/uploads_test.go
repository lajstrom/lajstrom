package facade

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

const uploads = "/v2/oci/images/app/blobs/uploads/"

// wantBlob fails the test when the fake does not hold b in the OCI repository name.
func (e *env) wantBlob(name string, b []byte) {
	e.t.Helper()
	rr, _ := e.prov.Repository(e.t.Context(), name)
	rc, err := rr.Fetch(e.t.Context(), ocispec.Descriptor{Digest: digest.FromBytes(b), Size: -1})
	if err != nil {
		e.t.Errorf("blob %s of %s: %v", digest.FromBytes(b), name, err)
		return
	}
	defer func() { _ = rc.Close() }()
	if got, _ := io.ReadAll(rc); !bytes.Equal(got, b) {
		e.t.Errorf("blob of %s is %q, want %q", name, got, b)
	}
}

// wantCreated fails the test when rr is not the 201 of the blob d of oci/images/app.
func wantCreated(t *testing.T, what string, rr *httptest.ResponseRecorder, d digest.Digest) {
	t.Helper()
	wantStatus(t, what, rr, http.StatusCreated)
	wantHeader(t, what, rr, "Location", "/v2/oci/images/app/blobs/"+d.String())
	wantHeader(t, what, rr, "Docker-Content-Digest", d.String())
}

// start opens a session on h and returns its upload id.
func (e *env) start(h http.Handler) string {
	e.t.Helper()
	rr := e.do(h, http.MethodPost, uploads, nil)
	wantStatus(e.t, "POST", rr, http.StatusAccepted)
	return uploadID(e.t, rr)
}

func TestUploadMonolithicPost(t *testing.T) {
	e := newEnv(t, envOptions{})
	b := []byte("one request")
	d := digest.FromBytes(b)
	rr := e.do(e.h, http.MethodPost, uploads+"?digest="+d.String(), bytes.NewReader(b), "Content-Type", octetStream)
	wantCreated(t, "monolithic POST", rr, d)
	e.wantBlob("oci/images/app", b)
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d backend sessions open", n)
	}

	rr = e.do(e.h, http.MethodPost, uploads+"?digest="+digest.FromString("other").String(), bytes.NewReader(b))
	wantError(t, "monolithic POST with the wrong digest", rr, http.StatusBadRequest, codeDigestInvalid)
	rr = e.do(e.h, http.MethodPost, uploads+"?digest=sha256:nothex", bytes.NewReader(b))
	wantError(t, "monolithic POST with an invalid digest", rr, http.StatusBadRequest, codeDigestInvalid)
	empty := digest.FromBytes(nil)
	wantCreated(t, "monolithic POST of the empty blob", e.do(e.h, http.MethodPost, uploads+"?digest="+empty.String(), nil), empty)
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d backend sessions open after the failures", n)
	}
}

func TestUploadPostPut(t *testing.T) {
	e := newEnv(t, envOptions{})
	rr := e.do(e.h, http.MethodPost, uploads, nil)
	wantStatus(t, "POST", rr, http.StatusAccepted)
	id := uploadID(t, rr)
	if !validUploadID(id) {
		t.Errorf("upload id %q is not 32 hex digits", id)
	}
	wantHeader(t, "POST", rr, "Location", uploads+id)
	wantHeader(t, "POST", rr, "Range", "0-0")
	wantHeader(t, "POST", rr, "Docker-Upload-UUID", id)
	wantHeader(t, "POST", rr, "OCI-Chunk-Min-Length", "")
	if strings.Contains(rr.Header().Get("Location"), "upload-") {
		t.Error("the backend's upload id reached the client")
	}
	b := []byte("post then put")
	d := digest.FromBytes(b)
	rr = e.do(e.h, http.MethodPut, uploads+id+"?digest="+d.String(), bytes.NewReader(b), "Content-Type", octetStream)
	wantCreated(t, "PUT", rr, d)
	e.wantBlob("oci/images/app", b)
	wantError(t, "GET after the PUT", e.do(e.h, http.MethodGet, uploads+id, nil), http.StatusNotFound, codeBlobUploadUnknown)
	wantError(t, "second PUT", e.do(e.h, http.MethodPut, uploads+id+"?digest="+d.String(), nil), http.StatusNotFound, codeBlobUploadUnknown)
}

func TestUploadChunked(t *testing.T) {
	e := newEnv(t, envOptions{})
	id := e.start(e.h)
	rr := e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("abc"), "Content-Range", "0-2", "Content-Type", octetStream)
	wantStatus(t, "first chunk", rr, http.StatusAccepted)
	wantHeader(t, "first chunk", rr, "Range", "0-2")
	wantHeader(t, "first chunk", rr, "Location", uploads+id)
	wantHeader(t, "first chunk", rr, "Docker-Upload-UUID", id)
	rr = e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("defg"))
	wantHeader(t, "streamed chunk with a Content-Length", rr, "Range", "0-6")
	rr = e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("hi"), "Content-Length", "-1")
	wantHeader(t, "chunked chunk", rr, "Range", "0-8")
	rr = e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("jk"), "Content-Length", "-1", "Content-Range", "9-10")
	wantHeader(t, "chunked chunk with a Content-Range", rr, "Range", "0-10")
	wantStatus(t, "empty chunk", e.do(e.h, http.MethodPatch, uploads+id, nil), http.StatusAccepted)

	rr = e.do(e.h, http.MethodGet, uploads+id, nil)
	wantStatus(t, "status", rr, http.StatusNoContent)
	wantHeader(t, "status", rr, "Range", "0-10")
	wantHeader(t, "status", rr, "Location", uploads+id)

	// A chunk that does not start at the end of the upload, and a Content-Range that is not one.
	rr = e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("abc"), "Content-Range", "0-2")
	wantError(t, "chunk again", rr, http.StatusRequestedRangeNotSatisfiable, codeBlobUploadInvalid)
	wantHeader(t, "chunk again", rr, "Range", "0-10")
	wantHeader(t, "chunk again", rr, "Location", uploads+id)
	wantError(t, "malformed Content-Range", e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("x"), "Content-Range", "11"),
		http.StatusBadRequest, codeBlobUploadInvalid)
	wantError(t, "Content-Range against Content-Length", e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("xy"), "Content-Range", "11-11"),
		http.StatusBadRequest, codeSizeInvalid)
	wantError(t, "final chunk out of order", e.do(e.h, http.MethodPut, uploads+id+"?digest="+digest.FromString("x").String(),
		strings.NewReader("x"), "Content-Range", "3-3"), http.StatusRequestedRangeNotSatisfiable, codeBlobUploadInvalid)

	// A digest that is not one keeps the session; the last chunk in the PUT closes it.
	wantError(t, "PUT without a digest", e.do(e.h, http.MethodPut, uploads+id, nil), http.StatusBadRequest, codeDigestInvalid)
	b := []byte("abcdefghijklm")
	rr = e.do(e.h, http.MethodPut, uploads+id+"?digest="+digest.FromBytes(b).String(), strings.NewReader("lm"), "Content-Range", "11-12")
	wantCreated(t, "PUT with the last chunk", rr, digest.FromBytes(b))
	e.wantBlob("oci/images/app", b)
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d backend sessions open", n)
	}
}

// TestUploadDigestInvalid: a digest the backend refuses ends the session, here and there.
func TestUploadDigestInvalid(t *testing.T) {
	e := newEnv(t, envOptions{})
	id := e.start(e.h)
	wantStatus(t, "chunk", e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("abc")), http.StatusAccepted)
	rr := e.do(e.h, http.MethodPut, uploads+id+"?digest="+digest.FromString("other").String(), nil)
	wantError(t, "wrong digest", rr, http.StatusBadRequest, codeDigestInvalid)
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d backend sessions open after the wrong digest, want it cancelled", n)
	}
	wantError(t, "GET after the wrong digest", e.do(e.h, http.MethodGet, uploads+id, nil), http.StatusNotFound, codeBlobUploadUnknown)

	// sha512 in one request: hashed in its own algorithm.
	id = e.start(e.h)
	b := []byte("sha512 content")
	sum := sha512.Sum512(b)
	d := digest.NewDigestFromBytes(digest.SHA512, sum[:])
	rr = e.do(e.h, http.MethodPut, uploads+id+"?digest="+d.String(), bytes.NewReader(b))
	wantStatus(t, "sha512", rr, http.StatusCreated)
}

func TestUploadCancel(t *testing.T) {
	e := newEnv(t, envOptions{})
	id := e.start(e.h)
	wantStatus(t, "chunk", e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("abc")), http.StatusAccepted)
	wantStatus(t, "DELETE", e.do(e.h, http.MethodDelete, uploads+id, nil), http.StatusNoContent)
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d backend sessions open after the cancel", n)
	}
	wantError(t, "GET after the cancel", e.do(e.h, http.MethodGet, uploads+id, nil), http.StatusNotFound, codeBlobUploadUnknown)
	wantError(t, "second DELETE", e.do(e.h, http.MethodDelete, uploads+id, nil), http.StatusNotFound, codeBlobUploadUnknown)

	// A backend session that is gone: the row goes too, and a cancel of it is fine.
	id = e.start(e.h)
	ctx := t.Context()
	rr, _ := e.prov.Repository(ctx, "oci/images/app")
	var st uploadState
	s, err := e.st.GetSession(ctx, e.oci.ID, uploadKind+id)
	must(t, err)
	must(t, json.Unmarshal(s.Files, &st))
	must(t, rr.CancelBlobUpload(ctx, st.upload()))
	wantError(t, "chunk to a session the backend dropped", e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("x")),
		http.StatusNotFound, codeBlobUploadUnknown)
	if _, err := e.st.GetSession(ctx, e.oci.ID, uploadKind+id); err == nil {
		t.Error("the row of a session the backend dropped is still there")
	}
	id = e.start(e.h)
	s, _ = e.st.GetSession(ctx, e.oci.ID, uploadKind+id)
	must(t, json.Unmarshal(s.Files, &st))
	must(t, rr.CancelBlobUpload(ctx, st.upload()))
	wantStatus(t, "DELETE of a session the backend dropped", e.do(e.h, http.MethodDelete, uploads+id, nil), http.StatusNoContent)
}

// TestUploadUnknown covers the ids that are not a session of this repo, name and principal,
// and the expiry.
func TestUploadUnknown(t *testing.T) {
	e := newEnv(t, envOptions{})
	id := e.start(e.h)
	for _, tt := range []struct{ name, target string }{
		{"malformed id", uploads + "upload-1"},
		{"upper-case id", uploads + strings.ToUpper(id)},
		{"no row", uploads + strings.Repeat("ab", 16)},
		{"another name", "/v2/oci/images/other/blobs/uploads/" + id},
		{"another repo", "/v2/raw/assets/app/blobs/uploads/" + id},
	} {
		wantError(t, tt.name, e.do(e.h, http.MethodPatch, tt.target, strings.NewReader("x")), http.StatusNotFound, codeBlobUploadUnknown)
	}
	wantError(t, "another principal", e.do(e.h, http.MethodGet, uploads+id, nil, principalHeader, "mallory"),
		http.StatusNotFound, codeBlobUploadUnknown)
	wantStatus(t, "the owner", e.do(e.h, http.MethodGet, uploads+id, nil), http.StatusNoContent)

	// A request slides the expiry; an hour without one ends the session.
	e.clock.advance(uploadTTL - time.Minute)
	wantStatus(t, "chunk before the expiry", e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("x")), http.StatusAccepted)
	e.clock.advance(uploadTTL - time.Minute)
	wantStatus(t, "status before the expiry", e.do(e.h, http.MethodGet, uploads+id, nil), http.StatusNoContent)
	e.clock.advance(uploadTTL + time.Second)
	wantError(t, "chunk after the expiry", e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("y")), http.StatusNotFound, codeBlobUploadUnknown)
	// The next POST expires the row in the index.
	e.start(e.h)
	if _, err := e.st.GetSession(t.Context(), e.oci.ID, uploadKind+id); err == nil {
		t.Error("the expired session is still open after a POST")
	}
}

func TestUploadMount(t *testing.T) {
	z := authorizer{"dev": {"oci/images": {auth.ActionRead, auth.ActionWrite}, "raw/assets": {auth.ActionWrite}}}
	e := newEnv(t, envOptions{authz: z})
	b := []byte("mount me")
	d := e.pushBlob("oci/images/base", b)
	user := []string{principalHeader, "dev"}
	rr := e.do(e.h, http.MethodPost, uploads+"?mount="+d.String()+"&from=oci/images/base", nil, user...)
	wantCreated(t, "mount from the same repo", rr, d)
	e.wantBlob("oci/images/app", b)

	for _, tt := range []struct{ name, query string }{
		{"absent blob", "?mount=" + digest.FromString("absent").String() + "&from=oci/images/base"},
		{"no from", "?mount=" + d.String()},
		{"from not a name", "?mount=" + d.String() + "&from=Bad//name"},
		{"from a repo that does not exist", "?mount=" + d.String() + "&from=oci/none/base"},
	} {
		rr := e.do(e.h, http.MethodPost, "/v2/oci/images/other/blobs/uploads/"+tt.query, nil, user...)
		wantStatus(t, tt.name, rr, http.StatusAccepted)
		if !strings.HasPrefix(rr.Header().Get("Location"), "/v2/oci/images/other/blobs/uploads/") {
			t.Errorf("%s: Location %q", tt.name, rr.Header().Get("Location"))
		}
	}
	wantError(t, "mount of an invalid digest", e.do(e.h, http.MethodPost, uploads+"?mount=sha256:x&from=oci/images/base", nil, user...),
		http.StatusBadRequest, codeDigestInvalid)

	// From another repo: only when the principal may read it, else a session and no mount.
	d2 := e.pushBlob("raw/assets/secret", []byte("secret"))
	rr = e.do(e.h, http.MethodPost, uploads+"?mount="+d2.String()+"&from=raw/assets/secret", nil, user...)
	wantStatus(t, "mount from a repo the principal may not read", rr, http.StatusAccepted)
	app, _ := e.prov.Repository(t.Context(), "oci/images/app")
	if ok, _ := app.Exists(t.Context(), ocispec.Descriptor{Digest: d2}); ok {
		t.Error("the blob of a repo the principal may not read was mounted")
	}
	z["dev"]["raw/assets"] = append(z["dev"]["raw/assets"], auth.ActionRead)
	rr = e.do(e.h, http.MethodPost, uploads+"?mount="+d2.String()+"&from=raw/assets/secret", nil, user...)
	wantCreated(t, "mount from a repo the principal may read", rr, d2)
}

// TestUploadBrokenBody: a body that breaks off is 400 and cancels the monolithic session. A
// chunk that breaks off leaves bytes in the backend that the hash did not take (the fake keeps
// them): the close is then 400 DIGEST_INVALID and a further chunk 404, and either ends the
// session (#368).
func TestUploadBrokenBody(t *testing.T) {
	e := newEnv(t, envOptions{})
	b := bytes.Repeat([]byte("x"), 1000)
	d := digest.FromBytes(b)
	rr := e.do(e.h, http.MethodPost, uploads+"?digest="+d.String(), brokenAfter(b, 300), "Content-Length", "1000")
	wantError(t, "monolithic POST that breaks off", rr, http.StatusBadRequest, codeBlobUploadInvalid)
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d backend sessions open", n)
	}
	id := e.start(e.h)
	rr = e.do(e.h, http.MethodPatch, uploads+id, brokenAfter(b, 300), "Content-Length", "1000")
	wantError(t, "chunk that breaks off", rr, http.StatusBadRequest, codeBlobUploadInvalid)
	rr = e.do(e.h, http.MethodGet, uploads+id, nil)
	wantHeader(t, "status after a broken chunk", rr, "Range", "0-299")
	rr = e.do(e.h, http.MethodPatch, uploads+id, bytes.NewReader(b[300:]), "Content-Range", "300-999")
	wantError(t, "chunk after it", rr, http.StatusNotFound, codeBlobUploadUnknown)
	e.wantNoUpload("chunk after a broken chunk", id)

	id = e.start(e.h)
	e.do(e.h, http.MethodPatch, uploads+id, brokenAfter(b, 300), "Content-Length", "1000")
	rr = e.do(e.h, http.MethodPut, uploads+id+"?digest="+d.String(), bytes.NewReader(b[300:]))
	wantError(t, "close after a broken chunk", rr, http.StatusBadRequest, codeDigestInvalid)
	e.wantNoUpload("close after a broken chunk", id)
}

// brokenAfter is a body of b that fails after n bytes, as a client that goes away.
func brokenAfter(b []byte, n int) io.Reader {
	return io.MultiReader(bytes.NewReader(b[:n]), errReader{})
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

// failingStore is the store whose session writes fail.
type failingStore struct{ index.Store }

func (failingStore) UpsertSession(context.Context, *index.Session) error {
	return errors.New("database is locked")
}

// TestUploadRowNotWritten: a session whose row cannot be written is cancelled in the backend,
// so that no backend session is orphaned, and the answer is a 500 without the cause.
func TestUploadRowNotWritten(t *testing.T) {
	e := newEnv(t, envOptions{})
	h := New(Options{Index: failingStore{e.st}, Providers: provider.Map{"local": e.prov}, Tokens: e.tokens, Log: slog.New(slog.NewTextHandler(e.logs, nil)),
		Now: e.clock.Now})
	rr := e.do(h, http.MethodPost, uploads, nil)
	if msg := wantError(t, "POST", rr, http.StatusInternalServerError, codeUnknown); strings.Contains(msg, "locked") {
		t.Errorf("message %q shows the cause", msg)
	}
	if n := e.prov.OpenUploads(); n != 0 {
		t.Errorf("%d backend sessions open, want the one of the failed POST cancelled", n)
	}
	id := e.start(e.h)
	wantError(t, "PATCH whose row cannot be written", e.do(h, http.MethodPatch, uploads+id, strings.NewReader("abc")),
		http.StatusInternalServerError, codeUnknown)
	rr = e.do(e.h, http.MethodGet, uploads+id, nil)
	wantHeader(t, "status after it: the chunk did not stream without its Recheck in the row", rr, "Range", "0-0")
}

// TestUploadMaxFileSize: a /v2 blob upload to a repo with maxFileSize 10 (U-3 of the
// threat-model pass of #63), a raw repo and an oci one alike (#246), is 413 SIZE_INVALID as soon
// as a declared size would carry the blob past it, and where a body of unknown length passes it,
// counted with the bytes the session's earlier PATCHes sent; a session so refused ends. Ten bytes
// pass, in one request or in chunks.
func TestUploadMaxFileSize(t *testing.T) {
	e := newEnv(t, envOptions{})
	for _, r := range []*index.Repo{&e.raw, &e.oci} {
		r.Policy = json.RawMessage(`{"maxFileSize":10}`)
		must(t, e.st.UpdateRepo(t.Context(), r))
	}
	eleven, ten := []byte("eleven byte"), []byte("ten bytes!")
	noBackendSessions := func(what string) {
		t.Helper()
		if n := e.prov.OpenUploads(); n != 0 {
			t.Errorf("%s: %d backend sessions open", what, n)
		}
	}
	for _, base := range []string{"/v2/raw/assets/docs/big.txt/blobs/uploads/", uploads} {
		post := func(b []byte, length string) *httptest.ResponseRecorder {
			return e.do(e.h, http.MethodPost, base+"?digest="+digest.FromBytes(b).String(), bytes.NewReader(b), "Content-Length", length)
		}
		wantError(t, base+" monolithic POST of 11 bytes", post(eleven, "11"), http.StatusRequestEntityTooLarge, codeSizeInvalid)
		wantError(t, base+" monolithic POST of 11 bytes, chunked", post(eleven, "-1"), http.StatusRequestEntityTooLarge, codeSizeInvalid)
		noBackendSessions(base + " monolithic POST")
		wantStatus(t, base+" monolithic POST of 10 bytes", post(ten, "10"), http.StatusCreated)

		start := func() string {
			t.Helper()
			rr := e.do(e.h, http.MethodPost, base, nil)
			wantStatus(t, "POST", rr, http.StatusAccepted)
			return uploadID(t, rr)
		}
		for _, tt := range []struct{ name, method, first, length string }{
			{"PATCH of 5 more", http.MethodPatch, "6", "5"},
			{"PATCH of 5 more, chunked", http.MethodPatch, "6", "-1"},
			{"PATCH of 5 more, both chunked", http.MethodPatch, "-1", "-1"},
			{"PUT of 5 more", http.MethodPut, "6", "5"},
			{"PUT of 5 more, chunked", http.MethodPut, "6", "-1"},
			{"PUT of 5 more, both chunked", http.MethodPut, "-1", "-1"},
		} {
			name := base + " " + tt.name
			id := start()
			wantStatus(t, name+": the first 6", e.do(e.h, http.MethodPatch, base+id, bytes.NewReader(eleven[:6]), "Content-Length", tt.first),
				http.StatusAccepted)
			target := base + id
			if tt.method == http.MethodPut {
				target += "?digest=" + digest.FromBytes(eleven).String()
			}
			rr := e.do(e.h, tt.method, target, bytes.NewReader(eleven[6:]), "Content-Length", tt.length)
			wantError(t, name, rr, http.StatusRequestEntityTooLarge, codeSizeInvalid)
			wantError(t, name+": the session after it", e.do(e.h, http.MethodGet, base+id, nil), http.StatusNotFound, codeBlobUploadUnknown)
			noBackendSessions(name)
		}

		id := start()
		wantStatus(t, base+" PATCH of 6, chunked", e.do(e.h, http.MethodPatch, base+id, bytes.NewReader(ten[:6]), "Content-Length", "-1"),
			http.StatusAccepted)
		rr := e.do(e.h, http.MethodPut, base+id+"?digest="+digest.FromBytes(ten).String(), bytes.NewReader(ten[6:]))
		wantStatus(t, base+" PUT of the last 4", rr, http.StatusCreated)
	}
}

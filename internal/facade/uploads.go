package facade

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/content"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Blob uploads across replicas. The client's upload id is the key of a sessions row of the
// index, not a value held in memory: the row holds the backend's session (provider.Upload:
// its location, its id, its offset) and the OCI name, and any replica that gets the next
// request loads it, calls the provider with it and writes it back. A row rather than an id
// signed with a server key, because there is no shared key before #41; one UpsertSession per
// chunk is cheap at the chunk sizes clients use. Every body streams from the client to the
// backend: nothing is spooled.

const (
	// uploadTTL is how long an upload session lives after its last request. The backends keep
	// theirs longer; an expired row is BLOB_UPLOAD_UNKNOWN, and the backend drops its session
	// on its own.
	uploadTTL = time.Hour
	// uploadKind is the kind of a blob upload in sessions.key, before the id.
	uploadKind = "blob-upload:"
	// cleanupTimeout bounds the cancel of a backend session after a failed request, which runs
	// when the client has gone.
	cleanupTimeout = 10 * time.Second
)

// uploadState is the files column of an upload session's row: the backend's session and the
// OCI name it belongs to, as the client addresses it.
type uploadState struct {
	Name     string `json:"name"`
	Location string `json:"location"`
	ID       string `json:"id"`
	Offset   int64  `json:"offset"`
}

func (s uploadState) upload() provider.Upload {
	return provider.Upload{Location: s.Location, ID: s.ID, Offset: s.Offset}
}

// startUpload answers POST …/blobs/uploads/: with ?mount= a mount, with ?digest= a monolithic
// upload of the body, else a new session (202). It also expires the sessions of every kind
// whose time is up, which nothing else does before the reconcile loop of #30.
func (h *handler) startUpload(w http.ResponseWriter, r *http.Request, t *target, _ string) error {
	ctx := r.Context()
	if err := h.authorize(ctx, auth.ActionWrite, t.repo); err != nil {
		return err
	}
	if n, err := h.o.Index.ExpireSessions(ctx, h.o.Now()); err != nil {
		h.o.Log.WarnContext(ctx, "facade: expire sessions", "error", err.Error())
	} else if n > 0 {
		h.o.Log.DebugContext(ctx, "facade: sessions expired", "count", n)
	}
	rr, err := h.backend(ctx, t)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	switch {
	case q.Has("mount"):
		return h.mount(w, r, t, rr, q.Get("mount"), q.Get("from"))
	case q.Has("digest"):
		return h.monolithic(w, r, t, rr, q.Get("digest"))
	}
	u, err := rr.StartBlobUpload(ctx)
	if err != nil {
		return &apiError{code: codeBlobUploadInvalid, err: err}
	}
	return h.openSession(w, r, t, rr, u)
}

// monolithic answers POST …/blobs/uploads/?digest=: the body is the whole blob, streamed into
// a backend session that is started and finished in this request. A failure cancels the
// backend session; no row is written, so a retry, on any replica, starts afresh.
func (h *handler) monolithic(w http.ResponseWriter, r *http.Request, t *target, rr provider.Repository, raw string) error {
	ctx := r.Context()
	d, err := parseDigest(raw)
	if err != nil {
		return err
	}
	u, err := rr.StartBlobUpload(ctx)
	if err != nil {
		return &apiError{code: codeBlobUploadInvalid, err: err}
	}
	body, size := requestBody(r)
	if _, err := rr.FinishBlobUpload(ctx, u, d, body.reader(), size); err != nil {
		h.cancelBackend(ctx, rr, u)
		return finishError(err, body)
	}
	answerBlob(w, t, d)
	return nil
}

// mount answers POST …/blobs/uploads/?mount=<digest>&from=<name>. The source is translated to
// its OCI name as the target is; it may be in another repo only when the principal may read
// that repo and the repo is on the same registry. A source that is not allowed, and a
// request without from, get a plain session, which is what the spec asks of a registry that
// cannot mount: passing a mount without from to the backend would let it take the blob from a
// repo the principal may not read.
func (h *handler) mount(w http.ResponseWriter, r *http.Request, t *target, rr provider.Repository, raw, from string) error {
	ctx := r.Context()
	d, err := parseDigest(raw)
	if err != nil {
		return err
	}
	if src, ok := h.mountSource(ctx, t, from); ok {
		mounted, u, err := rr.MountBlob(ctx, d, src)
		switch {
		case err != nil:
			return &apiError{code: codeBlobUploadInvalid, err: err}
		case mounted:
			answerBlob(w, t, d)
			return nil
		}
		return h.openSession(w, r, t, rr, u)
	}
	u, err := rr.StartBlobUpload(ctx)
	if err != nil {
		return &apiError{code: codeBlobUploadInvalid, err: err}
	}
	return h.openSession(w, r, t, rr, u)
}

// mountSource returns the OCI name of the mount source from, and false when there is none to
// mount from: from is empty, not a name, not a repo, on another registry, or not readable.
func (h *handler) mountSource(ctx context.Context, t *target, from string) (string, bool) {
	if from == "" {
		return "", false
	}
	src, err := h.resolve(ctx, from)
	if err != nil || src.repo.Registry != t.repo.Registry {
		return "", false
	}
	if src.repo.ID != t.repo.ID && h.authorize(ctx, auth.ActionRead, src.repo) != nil {
		return "", false
	}
	return src.oci, true
}

// openSession writes the row of a new backend session u and answers 202 with its Location. A
// row that cannot be written cancels the backend session.
func (h *handler) openSession(w http.ResponseWriter, r *http.Request, t *target, rr provider.Repository, u provider.Upload) error {
	ctx := r.Context()
	id, err := newUploadID()
	if err != nil {
		h.cancelBackend(ctx, rr, u)
		return err
	}
	s := &index.Session{RepoID: t.repo.ID, Key: uploadKind + id, Principal: auth.PrincipalFrom(ctx).String(), State: "open"}
	if err := h.saveSession(ctx, s, t, u); err != nil {
		h.cancelBackend(ctx, rr, u)
		return err
	}
	answerSession(w, t, id, u, http.StatusAccepted)
	return nil
}

// patchUpload answers PATCH …/blobs/uploads/<id>: the body streamed to the backend session as
// one chunk, 202 with the new Range. A Content-Range must start at the session's offset (416
// with the Range otherwise) and span Content-Length bytes.
func (h *handler) patchUpload(w http.ResponseWriter, r *http.Request, t *target, id string) error {
	ctx := r.Context()
	s, st, rr, err := h.continueSession(r, t, id)
	if err != nil {
		return err
	}
	size, err := chunkSize(w, r, t, id, st.Offset)
	if err != nil {
		return err
	}
	body, _ := requestBody(r)
	u, err := rr.AppendBlobChunk(ctx, st.upload(), body.reader(), size)
	if err != nil {
		return h.sessionError(ctx, s, err, body)
	}
	if err := h.saveSession(ctx, s, t, u); err != nil {
		return err
	}
	answerSession(w, t, id, u, http.StatusAccepted)
	return nil
}

// putUpload answers PUT …/blobs/uploads/<id>?digest=: the last chunk, if any, and the close of
// the backend session; 201 with the blob's Location, and the row is deleted. A digest the
// backend refuses ends the session (the spec's rule after an error): the backend session is
// cancelled, the row deleted, and the answer is 400 DIGEST_INVALID.
func (h *handler) putUpload(w http.ResponseWriter, r *http.Request, t *target, id string) error {
	ctx := r.Context()
	d, err := parseDigest(r.URL.Query().Get("digest"))
	if err != nil {
		return err
	}
	s, st, rr, err := h.continueSession(r, t, id)
	if err != nil {
		return err
	}
	size, err := chunkSize(w, r, t, id, st.Offset)
	if err != nil {
		return err
	}
	body, _ := requestBody(r)
	if size == 0 {
		body = nil
	}
	if _, err := rr.FinishBlobUpload(ctx, st.upload(), d, body.reader(), size); err != nil {
		if errors.Is(err, content.ErrMismatchedDigest) {
			h.cancelBackend(ctx, rr, st.upload())
			h.dropSession(ctx, s)
			return finishError(err, body)
		}
		return h.sessionError(ctx, s, err, body)
	}
	h.dropSession(ctx, s)
	answerBlob(w, t, d)
	return nil
}

// uploadStatus answers GET …/blobs/uploads/<id>: 204 with the Range of the backend session,
// which is written back to the row.
func (h *handler) uploadStatus(w http.ResponseWriter, r *http.Request, t *target, id string) error {
	ctx := r.Context()
	s, st, rr, err := h.continueSession(r, t, id)
	if err != nil {
		return err
	}
	u, err := rr.BlobUploadStatus(ctx, st.upload())
	if err != nil {
		return h.sessionError(ctx, s, err, nil)
	}
	if err := h.saveSession(ctx, s, t, u); err != nil {
		return err
	}
	answerSession(w, t, id, u, http.StatusNoContent)
	return nil
}

// cancelUpload answers DELETE …/blobs/uploads/<id>: the backend session is cancelled (one the
// backend does not know any more is fine) and the row deleted; 204.
func (h *handler) cancelUpload(w http.ResponseWriter, r *http.Request, t *target, id string) error {
	ctx := r.Context()
	s, st, rr, err := h.continueSession(r, t, id)
	if err != nil {
		return err
	}
	if err := rr.CancelBlobUpload(ctx, st.upload()); err != nil && !errors.Is(err, provider.ErrNotFound) {
		return &apiError{code: codeBlobUploadInvalid, err: err}
	}
	h.dropSession(ctx, s)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// errUploadUnknown is the answer for an upload id without an open row of this repo and name
// and principal, or with an expired one.
var errUploadUnknown = &apiError{status: http.StatusNotFound, code: codeBlobUploadUnknown, msg: "blob upload unknown to registry"}

// continueSession authorizes the write and loads the row of upload id: absent, expired, of
// another repo, name or principal is BLOB_UPLOAD_UNKNOWN.
func (h *handler) continueSession(r *http.Request, t *target, id string) (*index.Session, uploadState, provider.Repository, error) {
	ctx := r.Context()
	var st uploadState
	if err := h.authorize(ctx, auth.ActionWrite, t.repo); err != nil {
		return nil, st, nil, err
	}
	if !validUploadID(id) {
		return nil, st, nil, errUploadUnknown
	}
	s, err := h.o.Index.GetSession(ctx, t.repo.ID, uploadKind+id)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil, st, nil, errUploadUnknown
	case err != nil:
		return nil, st, nil, fmt.Errorf("facade: upload session %s: %w", id, err)
	case !s.ExpiresAt.After(h.o.Now()), s.Principal != auth.PrincipalFrom(ctx).String():
		return nil, st, nil, errUploadUnknown
	}
	if err := json.Unmarshal(s.Files, &st); err != nil {
		return nil, st, nil, fmt.Errorf("facade: upload session %s: %w", id, err)
	}
	if st.Name != t.name {
		return nil, st, nil, errUploadUnknown
	}
	rr, err := h.backend(ctx, t)
	if err != nil {
		return nil, st, nil, err
	}
	return s, st, rr, nil
}

// saveSession writes the row s with the backend session u, and moves its expiry uploadTTL
// from now.
func (h *handler) saveSession(ctx context.Context, s *index.Session, t *target, u provider.Upload) error {
	files, err := json.Marshal(uploadState{Name: t.name, Location: u.Location, ID: u.ID, Offset: u.Offset})
	if err != nil {
		return fmt.Errorf("facade: upload session: %w", err)
	}
	s.Files, s.ExpiresAt = files, h.o.Now().Add(uploadTTL)
	if err := h.o.Index.UpsertSession(ctx, s); err != nil {
		return fmt.Errorf("facade: upload session: %w", err)
	}
	return nil
}

// dropSession deletes the row s. A failure is logged: the row expires.
func (h *handler) dropSession(ctx context.Context, s *index.Session) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := h.o.Index.DeleteSession(ctx, s.ID); err != nil && !errors.Is(err, index.ErrNotFound) {
		h.o.Log.WarnContext(ctx, "facade: delete upload session", "session", s.ID, "error", err.Error())
	}
}

// sessionError is the answer to a provider error on an existing session: a backend session
// that is gone deletes the row (BLOB_UPLOAD_UNKNOWN); a client body that broke off is 400; the
// rest keeps the row, which a GET of the session brings back in line with the backend.
func (h *handler) sessionError(ctx context.Context, s *index.Session, err error, body *bodyReader) error {
	if errors.Is(err, provider.ErrNotFound) {
		h.dropSession(ctx, s)
		return &apiError{status: http.StatusNotFound, code: codeBlobUploadUnknown, msg: "blob upload unknown to registry", err: err}
	}
	return finishError(err, body)
}

// finishError is the answer to a failed chunk or finish: a digest the backend refused is
// DIGEST_INVALID, a client body that broke off BLOB_UPLOAD_INVALID (400), anything else
// BLOB_UPLOAD_INVALID with the status of the cause.
func finishError(err error, body *bodyReader) error {
	switch {
	case errors.Is(err, content.ErrMismatchedDigest):
		return &apiError{status: http.StatusBadRequest, code: codeDigestInvalid, msg: "provided digest did not match uploaded content", err: err}
	case body != nil && body.err != nil:
		return &apiError{status: http.StatusBadRequest, code: codeBlobUploadInvalid,
			msg: fmt.Sprintf("the request body broke off after %d bytes", body.n), err: err}
	}
	return &apiError{code: codeBlobUploadInvalid, err: err}
}

// cancelBackend cancels the backend session u after a failed request. It runs when the client
// may have gone, so it has a context of its own. A failure is logged: the backend expires its
// sessions.
func (h *handler) cancelBackend(ctx context.Context, rr provider.Repository, u provider.Upload) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := rr.CancelBlobUpload(ctx, u); err != nil && !errors.Is(err, provider.ErrNotFound) {
		h.o.Log.WarnContext(ctx, "facade: cancel backend upload session", "error", err.Error())
	}
}

// chunkSize returns the size of the request's chunk: Content-Length, or the span of
// Content-Range for a body of unknown length; -1 when neither tells. A Content-Range must be
// <first>-<last> with first at offset (416 with the session's Range and Location otherwise)
// and agree with Content-Length (SIZE_INVALID).
func chunkSize(w http.ResponseWriter, r *http.Request, t *target, id string, offset int64) (int64, error) {
	size := r.ContentLength
	cr := r.Header.Get("Content-Range")
	if cr == "" {
		return size, nil
	}
	first, last, ok := parseContentRange(cr)
	switch {
	case !ok:
		return 0, &apiError{status: http.StatusBadRequest, code: codeBlobUploadInvalid,
			msg: fmt.Sprintf("Content-Range %q is not <first>-<last>", cr)}
	case first != offset:
		w.Header().Set("Location", uploadLocation(t, id))
		w.Header().Set("Range", rangeOf(offset))
		w.Header().Set("Docker-Upload-UUID", id)
		return 0, &apiError{status: http.StatusRequestedRangeNotSatisfiable, code: codeBlobUploadInvalid,
			msg: fmt.Sprintf("Content-Range %q does not start at the end of the upload, byte %d", cr, offset)}
	case size >= 0 && last-first+1 != size:
		return 0, &apiError{status: http.StatusBadRequest, code: codeSizeInvalid,
			msg: fmt.Sprintf("Content-Range %q does not span Content-Length %d", cr, size)}
	}
	return last - first + 1, nil
}

// parseContentRange parses <first>-<last> (also bytes <first>-<last>/<total>, as some clients
// send it), inclusive, last not before first.
func parseContentRange(s string) (first, last int64, ok bool) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "bytes"), " ")
	s, _, _ = strings.Cut(s, "/")
	a, b, found := strings.Cut(s, "-")
	if !found {
		return 0, 0, false
	}
	first, err1 := strconv.ParseInt(a, 10, 64)
	last, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || first < 0 || last < first {
		return 0, 0, false
	}
	return first, last, true
}

// answerSession answers status for the upload id with the facade's Location for it, the Range
// of the bytes the backend holds and Docker-Upload-UUID. The backend's location never reaches
// the client. OCI-Chunk-Min-Length is not sent: provider.Upload does not carry the backend's.
func answerSession(w http.ResponseWriter, t *target, id string, u provider.Upload, status int) {
	w.Header().Set("Location", uploadLocation(t, id))
	w.Header().Set("Range", rangeOf(u.Offset))
	w.Header().Set("Docker-Upload-UUID", id)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

// answerBlob answers 201 for the blob d: its Location and Docker-Content-Digest.
func answerBlob(w http.ResponseWriter, t *target, d digest.Digest) {
	w.Header().Set("Location", "/v2/"+t.name+"/blobs/"+d.String())
	w.Header().Set("Docker-Content-Digest", d.String())
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// uploadLocation is the facade's URL of the upload id.
func uploadLocation(t *target, id string) string { return "/v2/" + t.name + "/blobs/uploads/" + id }

// rangeOf is the Range header of a session of offset bytes: 0-<offset-1>, and 0-0 when empty
// (the spec's form; a client reads the offset from Range only after a chunk).
func rangeOf(offset int64) string {
	if offset <= 0 {
		return "0-0"
	}
	return "0-" + strconv.FormatInt(offset-1, 10)
}

// newUploadID returns a random 128-bit id, hex.
func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("facade: upload id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// validUploadID reports whether id has the form of newUploadID's.
func validUploadID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

// parseDigest parses the digest of a query or a path: DIGEST_INVALID when it is not one of an
// available algorithm.
func parseDigest(s string) (digest.Digest, error) {
	d, err := digest.Parse(s)
	if err != nil {
		return "", &apiError{status: http.StatusBadRequest, code: codeDigestInvalid, msg: fmt.Sprintf("invalid digest %q", s)}
	}
	return d, nil
}

// requestBody returns the body of r to stream to the backend and its size: nil and 0 for an
// empty body, -1 for one of unknown length (chunked encoding). The bodyReader records how
// the client's body ended.
func requestBody(r *http.Request) (*bodyReader, int64) {
	if r.ContentLength == 0 || r.Body == nil || r.Body == http.NoBody {
		return nil, 0
	}
	return &bodyReader{r: r.Body}, r.ContentLength
}

// bodyReader is a request body that counts its bytes and keeps the error that ended it early,
// to tell a client that broke off from a backend that failed.
type bodyReader struct {
	r   io.Reader
	n   int64
	err error
}

// reader is b as an io.Reader: nil for a nil b, never an interface holding a nil pointer.
func (b *bodyReader) reader() io.Reader {
	if b == nil {
		return nil
	}
	return b
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n += int64(n)
	if err != nil && !errors.Is(err, io.EOF) && b.err == nil {
		b.err = err
	}
	return n, err
}

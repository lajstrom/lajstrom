package facade

import (
	"context"
	"crypto/rand"
	"encoding"
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
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Blob uploads across replicas. The client's upload id is the key of a sessions row of the
// index, not a value held in memory: the row holds the backend's session (provider.Upload:
// its location, its id, its offset) and the OCI name, and any replica that gets the next
// request loads it, calls the provider with it and writes it back. A row rather than an id
// signed with a server key, because there is no shared key before #41; the two UpsertSession of
// a chunk (Recheck before it streams, the new state after) are cheap at the chunk sizes clients
// use. Every body streams from the client to the backend; nothing is spooled here, and the
// provider spools only a chunk of unknown length that a registry with
// uploads.spoolUnknownLength refuses streamed (#425), as limitBlob cuts it. The row also keeps
// the hash of the bytes that passed (verify.go).

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

// uploadState is the files column of an upload session's row: the backend's session, the OCI
// name it belongs to, as the client addresses it, and the hash of the bytes that went through
// the facade into it.
type uploadState struct {
	Name     string `json:"name"`
	Location string `json:"location"`
	ID       string `json:"id"`
	Offset   int64  `json:"offset"`
	// SHA256 is the state of the sha256 of the first Hashed bytes, as crypto/sha256 marshals
	// it: nil before the first byte.
	SHA256 []byte `json:"sha256,omitempty"`
	Hashed int64  `json:"hashed"`
	// Recheck is set while a chunk streams, and stays when it fails: the backend may then hold
	// bytes the hash did not see, which closingHash asks it before the close.
	Recheck bool `json:"recheck,omitempty"`
}

func (s uploadState) upload() provider.Upload {
	return provider.Upload{Location: s.Location, ID: s.ID, Offset: s.Offset}
}

// startUpload answers POST …/blobs/uploads/: with ?mount= a mount, with ?digest= a monolithic
// upload of the body, else a new session (202). It also expires the sessions of every kind
// whose time is up, which nothing else does before the reconcile loop of #30.
func (h *handler) startUpload(w http.ResponseWriter, r *http.Request, t *target, _ string) error {
	ctx := r.Context()
	if err := h.authorize(ctx, auth.ActionWrite, t); err != nil {
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
	if err := h.ensureRepository(ctx, t); err != nil {
		return err
	}
	return h.openSession(w, r, t, rr, "", startSession(rr))
}

// startSession is the start of openSession for a new backend session of rr.
func startSession(rr provider.Repository) func(context.Context) (bool, provider.Upload, error) {
	return func(ctx context.Context) (bool, provider.Upload, error) {
		u, err := rr.StartBlobUpload(ctx)
		return false, u, err
	}
}

// ensureRepository creates the backend's repository of t where the backend does not create
// one on push (ECR without a creation template, capabilities.createOnPush: false), before the
// first request that writes to it: a blob upload or a mount (#356). Elsewhere it is a no-op,
// and the provider remembers a repository it created or found, so a push asks the backend
// once per repository and while, as ingest's publishes do.
func (h *handler) ensureRepository(ctx context.Context, t *target) error {
	p, err := h.registry(t)
	if err != nil {
		return err
	}
	if err := p.EnsureRepository(ctx, t.oci); err != nil {
		return &apiError{code: codeBlobUploadInvalid, err: fmt.Errorf("facade: ensure repository %s: %w", t.name, err)}
	}
	return nil
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
	if err := h.limitBlob(w, r, t, 0, r.ContentLength); err != nil {
		return err
	}
	if err := h.ensureRepository(ctx, t); err != nil {
		return err
	}
	u, err := rr.StartBlobUpload(ctx)
	if err != nil {
		return &apiError{code: codeBlobUploadInvalid, err: err}
	}
	body, size := requestBody(r)
	if err := finishChecked(ctx, rr, u, d, d.Algorithm().Hash(), body, size); err != nil {
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
// repo the principal may not read. With a bearer, a source the token does not name is 401 with
// both repositories in the challenge and insufficient_scope, so that the client gets a token
// that names it, as docker does for a mount from another repository.
func (h *handler) mount(w http.ResponseWriter, r *http.Request, t *target, rr provider.Repository, raw, from string) error {
	ctx := r.Context()
	d, err := parseDigest(raw)
	if err != nil {
		return err
	}
	src, ok, err := h.mountSource(ctx, t, from)
	if err != nil {
		return err
	}
	if err := h.ensureRepository(ctx, t); err != nil {
		return err
	}
	if ok {
		return h.openSession(w, r, t, rr, d, func(ctx context.Context) (bool, provider.Upload, error) {
			return rr.MountBlob(ctx, d, src)
		})
	}
	return h.openSession(w, r, t, rr, "", startSession(rr))
}

// mountSource returns the OCI name of the mount source from, and false when there is none to
// mount from: from is empty, not a name, not a repo, on another registry, or not readable:
// in another repo the principal may not read, or, with a bearer, another repository the token
// names without pull. A bearer that does not name another repository at all is the error of
// namedByToken, 401 with insufficient_scope, before the repo is looked up: whether from
// exists is not in the answer.
func (h *handler) mountSource(ctx context.Context, t *target, from string) (string, bool, error) {
	if from == "" {
		return "", false, nil
	}
	g, bearer := grantFrom(ctx)
	if bearer && from != t.name {
		if !token.ValidName(from) { // no token names it: never a source, whatever resolve accepts
			return "", false, nil
		}
		if _, named := g.Repository(from); !named {
			return "", false, h.namedByToken(ctx, g, from)
		}
		if !g.Allows(from, token.ActionPull) {
			return "", false, nil
		}
	}
	src, err := h.resolve(ctx, from)
	if err != nil || src.repo.Registry != t.repo.Registry {
		return "", false, nil
	}
	if bearer {
		return src.oci, true, nil
	}
	if src.repo.ID != t.repo.ID && h.authorize(ctx, auth.ActionRead, src) != nil {
		return "", false, nil
	}
	return src.oci, true, nil
}

// openSession opens an upload session of t. It counts the new upload id among the principal's
// live sessions first (admitUpload: 429 at a bound), then start opens the backend's session, or
// mounts the blob d (not empty), which is answered 201 and opens nothing: at a bound a mount is
// still tried, and only a session it answers instead is cancelled and refused. The row of the
// session is written and the answer is 202 with its Location; a row that cannot be written
// cancels the backend session.
func (h *handler) openSession(w http.ResponseWriter, r *http.Request, t *target, rr provider.Repository, d digest.Digest,
	start func(context.Context) (bool, provider.Upload, error)) error {
	ctx := r.Context()
	id, err := newUploadID()
	if err != nil {
		return err
	}
	principal := auth.PrincipalFrom(ctx).String()
	full := h.admitUpload(ctx, t, principal, id)
	if full != nil && d == "" {
		return full
	}
	mounted, u, err := start(ctx)
	switch {
	case err != nil:
		h.uploads.release(principal, id)
		return &apiError{code: codeBlobUploadInvalid, err: err}
	case mounted:
		h.uploads.release(principal, id)
		answerBlob(w, t, d)
		return nil
	case full != nil:
		h.cancelBackend(ctx, rr, u)
		return full
	}
	s := &index.Session{RepoID: t.repo.ID, Key: uploadKind + id, Principal: principal, State: "open"}
	if err := h.saveSession(ctx, s, uploadState{Name: t.name, Location: u.Location, ID: u.ID, Offset: u.Offset}); err != nil {
		h.uploads.release(principal, id)
		h.cancelBackend(ctx, rr, u)
		return err
	}
	h.uploads.end(principal, id, h.o.Now())
	answerSession(w, t, id, u, http.StatusAccepted)
	return nil
}

// patchUpload answers PATCH …/blobs/uploads/<id>: the body streamed to the backend session as
// one chunk, and hashed, 202 with the new Range. A Content-Range must start at the session's
// offset (416 with the Range otherwise) and span Content-Length bytes. The row says Recheck
// while the chunk streams; a backend that then holds more than what passed ends the session.
func (h *handler) patchUpload(w http.ResponseWriter, r *http.Request, t *target, id string) error {
	ctx := r.Context()
	s, st, rr, err := h.continueSession(r, t, id)
	if err != nil {
		return err
	}
	defer h.requestEnded(s)
	size, err := chunkSize(w, r, t, id, st.Offset)
	if err != nil {
		return err
	}
	if err := h.limitBlob(w, r, t, st.Offset, size); err != nil {
		h.endUpload(ctx, s, rr, st)
		return err
	}
	body, _ := requestBody(r)
	hs, err := st.sha256()
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		st.Recheck, rd = true, io.TeeReader(body, hs)
		if err := h.saveSession(ctx, s, st); err != nil {
			return err
		}
	}
	u, err := rr.AppendBlobChunk(ctx, st.upload(), rd, size)
	if err != nil {
		if cutAtLimit(body) {
			h.endUpload(ctx, s, rr, st)
		}
		return h.sessionError(ctx, s, err, body)
	}
	if body != nil {
		if u.Offset != st.Hashed+body.n {
			h.endUpload(ctx, s, rr, st)
			return &apiError{status: http.StatusNotFound, code: codeBlobUploadUnknown, msg: errUnchecked.Error(), err: errUnchecked}
		}
		if st.SHA256, err = hs.(encoding.BinaryMarshaler).MarshalBinary(); err != nil {
			return fmt.Errorf("facade: upload session: sha256 state: %w", err)
		}
		st.Hashed, st.Recheck = u.Offset, false
	}
	st.Location, st.ID, st.Offset = u.Location, u.ID, u.Offset
	if err := h.saveSession(ctx, s, st); err != nil {
		return err
	}
	answerSession(w, t, id, u, http.StatusAccepted)
	return nil
}

// putUpload answers PUT …/blobs/uploads/<id>?digest=: the last chunk, if any, and the close of
// the backend session; 201 with the blob's Location, and the row is deleted. Bytes without the
// digest (finishChecked), a session that cannot be checked (closingHash) and a digest the
// backend refuses end the session (the spec's rule after an error): the backend session is
// cancelled, the row deleted, and the answer is 400 DIGEST_INVALID. So does any failure once
// the last chunk has begun to stream, since the backend may hold part of it.
func (h *handler) putUpload(w http.ResponseWriter, r *http.Request, t *target, id string) error {
	ctx := r.Context()
	s, st, rr, err := h.continueSession(r, t, id) // authorizes before the digest: rbac.md §3
	if err != nil {
		return err
	}
	defer h.requestEnded(s)
	d, err := parseDigest(r.URL.Query().Get("digest"))
	if err != nil {
		return err
	}
	size, err := chunkSize(w, r, t, id, st.Offset)
	if err != nil {
		return err
	}
	if err := h.limitBlob(w, r, t, st.Offset, size); err != nil {
		h.endUpload(ctx, s, rr, st)
		return err
	}
	body, _ := requestBody(r)
	if size == 0 {
		body = nil
	}
	hs, err := closingHash(ctx, rr, st, d)
	var ae *apiError
	switch {
	case errors.As(err, &ae): // a session that cannot be checked
		h.endUpload(ctx, s, rr, st)
		return err
	case err != nil:
		return h.sessionError(ctx, s, err, nil)
	}
	if err := finishChecked(ctx, rr, st.upload(), d, hs, body, size); err != nil {
		if ends(err, body) {
			h.endUpload(ctx, s, rr, st)
			return finishError(err, body)
		}
		return h.sessionError(ctx, s, err, body)
	}
	h.dropSession(ctx, s)
	answerBlob(w, t, d)
	return nil
}

// ends reports whether the failed close of an upload ends its session: a digest refused, here or
// by the backend, a body cut at the repo's maxFileSize, or a last chunk of which the backend may
// hold a part. A session the backend no longer knows is sessionError's.
func ends(err error, body *bodyReader) bool {
	switch {
	case errors.Is(err, errNotDigest), errors.Is(err, content.ErrMismatchedDigest), cutAtLimit(body):
		return true
	}
	return body != nil && body.n > 0 && !errors.Is(err, provider.ErrNotFound)
}

// uploadStatus answers GET …/blobs/uploads/<id>: 204 with the Range of the backend session,
// which is written back to the row.
func (h *handler) uploadStatus(w http.ResponseWriter, r *http.Request, t *target, id string) error {
	ctx := r.Context()
	s, st, rr, err := h.continueSession(r, t, id)
	if err != nil {
		return err
	}
	defer h.requestEnded(s)
	u, err := rr.BlobUploadStatus(ctx, st.upload())
	if err != nil {
		return h.sessionError(ctx, s, err, nil)
	}
	st.Location, st.ID, st.Offset = u.Location, u.ID, u.Offset
	if err := h.saveSession(ctx, s, st); err != nil {
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
	defer h.requestEnded(s)
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
// another repo, name or principal is BLOB_UPLOAD_UNKNOWN. The session then counts as live, with
// a request running, until the caller's requestEnded.
func (h *handler) continueSession(r *http.Request, t *target, id string) (*index.Session, uploadState, provider.Repository, error) {
	ctx := r.Context()
	var st uploadState
	if err := h.authorize(ctx, auth.ActionWrite, t); err != nil {
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
	h.uploads.begin(s.Principal, s.RepoID, id, h.o.Now())
	return s, st, rr, nil
}

// requestEnded records the end of a request on the upload session s that continueSession began.
func (h *handler) requestEnded(s *index.Session) {
	h.uploads.end(s.Principal, strings.TrimPrefix(s.Key, uploadKind), h.o.Now())
}

// saveSession writes the row s with the upload st, and moves its expiry uploadTTL from now.
func (h *handler) saveSession(ctx context.Context, s *index.Session, st uploadState) error {
	files, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("facade: upload session: %w", err)
	}
	s.Files, s.ExpiresAt = files, h.o.Now().Add(uploadTTL)
	if err := h.o.Index.UpsertSession(ctx, s); err != nil {
		return fmt.Errorf("facade: upload session: %w", err)
	}
	return nil
}

// dropSession deletes the row s, and the session no longer counts among the principal's live
// ones. A failure is logged: the row expires.
func (h *handler) dropSession(ctx context.Context, s *index.Session) {
	h.uploads.release(s.Principal, strings.TrimPrefix(s.Key, uploadKind))
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
// DIGEST_INVALID, a body cut at the repo's maxFileSize (limitBlob) 413 SIZE_INVALID, a client
// body that broke off BLOB_UPLOAD_INVALID (400, or 408 when it stalled: brokeOff), anything else
// BLOB_UPLOAD_INVALID with the status of the cause.
func finishError(err error, body *bodyReader) error {
	switch {
	case errors.Is(err, errNotDigest), errors.Is(err, content.ErrMismatchedDigest):
		return &apiError{status: http.StatusBadRequest, code: codeDigestInvalid, msg: "provided digest did not match uploaded content", err: err}
	case cutAtLimit(body):
		msg := "the blob is larger than the bound of the upload: the upload ends"
		var cut *blobTooLarge
		if errors.As(body.err, &cut) {
			msg = cut.msg
		}
		return &apiError{status: http.StatusRequestEntityTooLarge, code: codeSizeInvalid, msg: msg, err: err}
	case body != nil && body.err != nil:
		status, how := brokeOff(body.err)
		return &apiError{status: status, code: codeBlobUploadInvalid,
			msg: fmt.Sprintf("the request body %s after %d bytes", how, body.n), err: err}
	}
	return &apiError{code: codeBlobUploadInvalid, err: err}
}

// brokeOff returns the status of a client body that ended early with err, and how it ended:
// 408 "stalled" when server.stallTimeout cut it (format.StatusOf, #576), else 400 "broke off".
func brokeOff(err error) (int, string) {
	if format.StatusOf(err) == http.StatusRequestTimeout {
		return http.StatusRequestTimeout, "stalled"
	}
	return http.StatusBadRequest, "broke off"
}

// limitBlob bounds what a request may add to an upload of t that holds offset bytes (U-3 of the
// threat model, #246) by blobLimit: when offset and the declared size, the request's
// Content-Length or Content-Range, pass it, the answer is 413 SIZE_INVALID naming the bound
// before anything streams; a body of unknown length (size -1) is cut where it would pass it
// (http.MaxBytesReader in a tooLargeBody; finishError answers 413). Without a bound the request
// is left as it is.
func (h *handler) limitBlob(w http.ResponseWriter, r *http.Request, t *target, offset, size int64) error {
	limit, bound, err := h.blobLimit(r.Context(), t)
	if err != nil || limit < 0 {
		return err
	}
	msg := fmt.Sprintf("the blob is larger than %s, %d bytes: the upload ends", bound, limit)
	switch {
	case offset+max(size, 0) > limit:
		return &apiError{status: http.StatusRequestEntityTooLarge, code: codeSizeInvalid, msg: msg}
	case size < 0 && r.Body != nil:
		r.Body = tooLargeBody{ReadCloser: http.MaxBytesReader(w, r.Body, limit-offset), msg: msg}
	}
	return nil
}

// blobLimit returns the bound of a blob upload of t and what it is: the repo's maxFileSize
// (format.MaxFileSize, an oci repo's too), or the registry's capabilities.maxBlobSize when that
// is smaller; -1 when there is none (manifest-profile.md §5). A registry whose capabilities
// cannot be read bounds nothing here: its own answer will.
func (h *handler) blobLimit(ctx context.Context, t *target) (int64, string, error) {
	limit, err := format.MaxFileSize(t.repo)
	if err != nil {
		return 0, "", fmt.Errorf("facade: %w", err)
	}
	bound := fmt.Sprintf("the maxFileSize of %s/%s", t.repo.Format, t.repo.Name)
	p, err := h.registry(t)
	if err != nil {
		return 0, "", err
	}
	if caps, err := p.Capabilities(ctx); err == nil && caps.MaxBlobSize > 0 && (limit < 0 || caps.MaxBlobSize < limit) {
		limit, bound = caps.MaxBlobSize, "the maxBlobSize of registry "+t.repo.Registry
	}
	return limit, bound, nil
}

// tooLargeBody is a body that limitBlob cut at its bound: its error past the bound names it, in
// a *blobTooLarge that wraps the *http.MaxBytesError.
type tooLargeBody struct {
	io.ReadCloser
	msg string
}

func (b tooLargeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		err = &blobTooLarge{msg: b.msg, err: err}
	}
	return n, err
}

// blobTooLarge is the error of a tooLargeBody past its bound; msg names the bound.
type blobTooLarge struct {
	msg string
	err error
}

func (e *blobTooLarge) Error() string { return e.msg + ": " + e.err.Error() }
func (e *blobTooLarge) Unwrap() error { return e.err }

// cutAtLimit reports whether body was cut at its bound (limitBlob).
func cutAtLimit(body *bodyReader) bool {
	var tooLarge *http.MaxBytesError
	return body != nil && errors.As(body.err, &tooLarge)
}

// endUpload ends the upload session s after a request that can never fit its bound:
// the backend session st is cancelled and the row dropped.
func (h *handler) endUpload(ctx context.Context, s *index.Session, rr provider.Repository, st uploadState) {
	h.cancelBackend(ctx, rr, st.upload())
	h.dropSession(ctx, s)
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

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n += int64(n)
	if err != nil && !errors.Is(err, io.EOF) && b.err == nil {
		b.err = err
	}
	return n, err
}

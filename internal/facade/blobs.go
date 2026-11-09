package facade

import (
	"errors"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/server"
)

// octetStream is the Content-Type of a blob.
const octetStream = "application/octet-stream"

// getBlob answers HEAD and GET …/blobs/<digest> through format.ServeBlob, one request to the
// backend: HEAD the backend's HEAD, GET with the client's Range passed through, a 307 to the
// backend's storage when the provider gives a URL and the repo policy's blobRedirect allows
// it, otherwise the backend's answer streamed, with Content-Type application/octet-stream,
// Docker-Content-Digest, ETag (the digest), Content-Length and, from the backend,
// Accept-Ranges and Content-Range. A blob the backend does not have is BLOB_UNKNOWN; a stream
// that breaks after the headers is logged.
func (h *handler) getBlob(w http.ResponseWriter, r *http.Request, t *target, arg string) error {
	ctx := r.Context()
	if err := h.authorize(ctx, auth.ActionRead, t); err != nil { // before the digest: rbac.md §3
		return err
	}
	d, err := parseDigest(arg)
	if err != nil {
		return err
	}
	p, err := h.registry(t)
	if err != nil {
		return err
	}
	err = format.ServeBlob(w, r, p, t.oci, d, format.ServeBlobOptions{
		ContentType: octetStream,
		Redirect:    format.BlobRedirect(t.repo),
		Fail:        func(w http.ResponseWriter, err error) { h.fail(w, r, blobError(err)) },
	})
	if err != nil {
		h.o.Log.WarnContext(ctx, "facade: the blob stream failed after the response started", "name", t.name,
			"digest", d.String(), "error", err.Error(), "requestId", server.RequestID(ctx))
	}
	return nil
}

// blobError is the answer to an error of a blob read: BLOB_UNKNOWN, with the status of the
// cause (404 for one the backend does not have).
func blobError(err error) error {
	if errors.Is(err, provider.ErrNotFound) {
		return &apiError{code: codeBlobUnknown, msg: "blob unknown to registry", err: err}
	}
	return &apiError{code: codeBlobUnknown, err: err}
}

// deleteBlob answers DELETE …/blobs/<digest>: 405. A blob no manifest uses any more is the
// backend's garbage collection's (docs/spec/http-routing.md §2).
func (h *handler) deleteBlob(_ http.ResponseWriter, r *http.Request, t *target, _ string) error {
	if err := h.authorize(r.Context(), auth.ActionDelete, t); err != nil {
		return err
	}
	return &apiError{status: http.StatusMethodNotAllowed, code: codeUnsupported,
		msg: "blobs are not deleted through Lajstrom: delete the manifests, and the registry's garbage collection removes the blobs they used"}
}

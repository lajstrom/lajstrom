package facade

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/server"
)

// octetStream is the Content-Type of a blob.
const octetStream = "application/octet-stream"

// getBlob answers HEAD and GET …/blobs/<digest>: the blob streamed from the backend, with
// Content-Length, Docker-Content-Digest, Content-Type application/octet-stream and
// Accept-Ranges. The descriptor has no size (a client asks by digest alone), so the provider
// fetches by the digest and its reader tells the size when it is an io.Seeker, which the
// providers' readers of unsized blobs are; without one the answer has no Content-Length.
// HEAD fetches too, and reads nothing.
//
// A Range header is not passed on: the answer is the whole blob with 200, which HTTP allows.
// Ranges and the 307 to Provider.BlobRedirectURL come with #27.
func (h *handler) getBlob(w http.ResponseWriter, r *http.Request, t *target, arg string) error {
	ctx := r.Context()
	d, err := parseDigest(arg)
	if err != nil {
		return err
	}
	if err := h.authorize(ctx, auth.ActionRead, t.repo); err != nil {
		return err
	}
	rr, err := h.backend(ctx, t)
	if err != nil {
		return err
	}
	rc, err := rr.Fetch(ctx, ocispec.Descriptor{MediaType: octetStream, Digest: d, Size: -1})
	if errors.Is(err, provider.ErrNotFound) {
		return &apiError{code: codeBlobUnknown, msg: "blob unknown to registry", err: err}
	}
	if err != nil {
		return &apiError{code: codeBlobUnknown, err: err}
	}
	defer func() { _ = rc.Close() }()
	size, err := sizeOf(rc)
	if err != nil {
		return fmt.Errorf("facade: blob %s@%s: %w", t.name, d, err)
	}
	hdr := w.Header()
	hdr.Set("Content-Type", octetStream)
	hdr.Set("Docker-Content-Digest", d.String())
	hdr.Set("Accept-Ranges", "bytes")
	if size >= 0 {
		hdr.Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return nil
	}
	if n, err := io.Copy(w, rc); err != nil && ctx.Err() == nil {
		h.o.Log.WarnContext(ctx, "facade: the blob stream failed after the response started", "name", t.name,
			"digest", d.String(), "bytes", n, "error", err.Error(), "requestId", server.RequestID(ctx))
	}
	return nil
}

// sizeOf returns the size of a blob reader that is an io.Seeker, which it leaves at the start,
// and -1 for any other reader.
func sizeOf(rc io.Reader) (int64, error) {
	s, ok := rc.(io.Seeker)
	if !ok {
		return -1, nil
	}
	size, err := s.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := s.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return size, nil
}

// deleteBlob answers DELETE …/blobs/<digest>: 405 until #61.
func (h *handler) deleteBlob(_ http.ResponseWriter, r *http.Request, t *target, _ string) error {
	if err := h.authorize(r.Context(), auth.ActionDelete, t.repo); err != nil {
		return err
	}
	return &apiError{status: http.StatusMethodNotAllowed, code: codeUnsupported, msg: "deleting blobs comes with #61"}
}

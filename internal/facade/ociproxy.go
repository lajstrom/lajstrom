package facade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// OCIProxy serves the reads of oci proxy repos (ADR 0018, docs/spec/proxy-and-group.md §11):
// *proxy.Core. It is the facade's own interface, as Ingester is.
type OCIProxy interface {
	// OCIPath is the path under which repo keeps what a client names path: library/<path> for
	// a one-component path of Docker Hub.
	OCIPath(repo index.Repo, path string) string
	// OCIManifest returns the manifest ref names at path: its descriptor, and its bytes when the
	// backend does not hold it (nil: read it from the backend by the digest).
	OCIManifest(ctx context.Context, repo index.Repo, path, ref string) (ocispec.Descriptor, []byte, error)
	// OCIBlob returns the blob d at path as the manifest that lists it gives it, and with fetch
	// makes the backend hold it; with stream, rc is the verified tee of the copy when this
	// request holds the blob's claim, nil otherwise (read the blob from the backend).
	OCIBlob(ctx context.Context, repo index.Repo, path string, d digest.Digest, fetch, stream bool) (ocispec.Descriptor, io.ReadCloser, error)
}

// ociProxy reports whether t is an oci proxy repo that the facade asks Options.OCIProxy about.
func (h *handler) ociProxy(t *target) bool {
	return h.o.OCIProxy != nil && t.repo.Format == ociFormat && t.repo.Kind == index.KindProxy
}

// getProxyManifest answers HEAD and GET …/manifests/<ref> of an oci proxy repo from the core: the
// manifest's media type, digest and length, and its bytes from the cache or, when the backend
// holds it, from the backend, checked against the digest as they stream.
func (h *handler) getProxyManifest(w http.ResponseWriter, r *http.Request, t *target, ref string) error {
	ctx := r.Context()
	desc, raw, err := h.o.OCIProxy.OCIManifest(ctx, t.repo, t.path, ref)
	if err != nil {
		return proxyError(codeManifestUnknown, err)
	}
	hdr := w.Header()
	hdr.Set("Content-Type", desc.MediaType)
	hdr.Set("Docker-Content-Digest", desc.Digest.String())
	hdr.Set("Content-Length", strconv.FormatInt(desc.Size, 10))
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusOK)
		return nil
	}
	if raw != nil {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return nil
	}
	rr, err := h.backend(ctx, t)
	if err != nil {
		return err
	}
	rc, err := rr.Fetch(ctx, desc)
	if err != nil {
		return manifestUnknown(err)
	}
	defer func() { _ = rc.Close() }()
	w.WriteHeader(http.StatusOK)
	h.streamManifest(ctx, w, t, desc, rc)
	return nil
}

// getProxyBlob answers HEAD and GET of a blob of an oci proxy repo that the backend does not
// hold (ServeBlob answered 404): a HEAD from what the manifest that lists it says; a GET without
// Range by the request that holds the blob's claim from the verified tee of the copy (§11.8),
// any other GET once the core made the backend hold it, served from the backend.
func (h *handler) getProxyBlob(w http.ResponseWriter, r *http.Request, t *target, d digest.Digest, serve func() error) error {
	get := r.Method == http.MethodGet
	desc, rc, err := h.o.OCIProxy.OCIBlob(r.Context(), t.repo, t.path, d, get, get && r.Header.Get("Range") == "")
	if err != nil {
		return proxyError(codeBlobUnknown, err)
	}
	if rc != nil {
		h.teeBlob(w, r, t, desc, rc)
		return nil
	}
	if get {
		return serve()
	}
	hdr := w.Header()
	hdr.Set("Content-Type", octetStream)
	hdr.Set("Docker-Content-Digest", d.String())
	hdr.Set("ETag", `"`+d.String()+`"`)
	hdr.Set("Content-Length", strconv.FormatInt(desc.Size, 10))
	w.WriteHeader(http.StatusOK)
	return nil
}

// teeBlob copies the verified tee rc of the blob desc to the client: the headers of a whole blob,
// then the bytes as they go into the backend. The tee holds the last byte until the backend
// committed the blob; an error of it aborts the response, so that the client never completes a
// blob the backend does not hold (§11.8, rule 2).
func (h *handler) teeBlob(w http.ResponseWriter, r *http.Request, t *target, desc ocispec.Descriptor, rc io.ReadCloser) {
	defer func() { _ = rc.Close() }()
	hdr := w.Header()
	hdr.Set("Content-Type", octetStream)
	hdr.Set("Docker-Content-Digest", desc.Digest.String())
	hdr.Set("ETag", `"`+desc.Digest.String()+`"`)
	hdr.Set("Content-Length", strconv.FormatInt(desc.Size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil && r.Context().Err() == nil {
		h.logProxy(r.Context(), t, desc.Digest, err)
		panic(fmt.Errorf("facade: the tee of %s@%s, the response is aborted: %w", t.name, desc.Digest, http.ErrAbortHandler))
	}
}

// proxyError is the answer to an error of the core on a read of an oci proxy repo, with code for
// what it read: 404 code for what the upstream does not have, 403 DENIED for a path outside the
// lists, 400 NAME_INVALID for a name the backend does not take, 503 TOOMANYREQUESTS with the
// upstream's Retry-After for its 429; the other statuses are format.StatusOf's (502, 504).
func proxyError(code string, err error) error {
	var busy *format.UnavailableError
	switch {
	case errors.Is(err, index.ErrNotFound):
		return &apiError{status: http.StatusNotFound, code: code, err: err}
	case errors.Is(err, format.ErrForbidden):
		return &apiError{status: http.StatusForbidden, code: codeDenied, err: err}
	case errors.Is(err, format.ErrInvalid):
		return &apiError{status: http.StatusBadRequest, code: codeNameInvalid, err: err}
	case errors.As(err, &busy):
		return &apiError{status: http.StatusServiceUnavailable, code: codeTooManyRequests, retryAfter: busy.RetryAfter, told: true,
			msg: fmt.Sprintf("the upstream of the repo asks to wait %s", busy.RetryAfter.Round(1e9)), err: err}
	}
	return &apiError{code: code, err: err}
}

// refuseProxyPush refuses the push of a manifest that is not a referrer to an oci proxy repo,
// 405 UNSUPPORTED: what it holds is its upstream's (http-routing.md §6).
func refuseProxyPush(t *target) error {
	return &apiError{status: http.StatusMethodNotAllowed, code: codeUnsupported,
		msg: fmt.Sprintf("%s/%s is a proxy of its upstream: only a referrer may be pushed to it", t.repo.Format, t.repo.Name)}
}

// logProxy logs a read of an oci proxy repo that failed after its response started.
func (h *handler) logProxy(ctx context.Context, t *target, d digest.Digest, err error) {
	h.o.Log.WarnContext(ctx, "facade: the blob stream failed after the response started", "name", t.name, "digest", d.String(),
		"error", err.Error(), "requestId", server.RequestID(ctx))
}

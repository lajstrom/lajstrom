package facade

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/server"
)

// manifestMediaTypes are the media types a manifest PUT passes through: those oras-go sends
// to the manifests endpoint, so the provider stores the manifest as one.
var manifestMediaTypes = map[string]bool{
	ocispec.MediaTypeImageManifest:                              true,
	ocispec.MediaTypeImageIndex:                                 true,
	"application/vnd.oci.artifact.manifest.v1+json":             true,
	"application/vnd.docker.distribution.manifest.v2+json":      true,
	"application/vnd.docker.distribution.manifest.list.v2+json": true,
}

// tagGrammar is the grammar of a tag (distribution spec, "Pulling manifests").
var tagGrammar = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

// parseReference returns the digest of ref when it is a digest, and "" for a tag; a reference
// that is neither is DIGEST_INVALID (it has a colon) or TAG_INVALID.
func parseReference(ref string) (digest.Digest, error) {
	if strings.Contains(ref, ":") {
		return parseDigest(ref)
	}
	if !tagGrammar.MatchString(ref) {
		return "", &apiError{status: http.StatusBadRequest, code: codeTagInvalid, msg: fmt.Sprintf("invalid tag %q", ref)}
	}
	return "", nil
}

// getManifest answers HEAD and GET …/manifests/<ref>: the backend resolves the reference with
// oras-go's manifest media types (OCI and Docker manifests and indexes), and the answer has
// the manifest's own Content-Type, Docker-Content-Digest and Content-Length. The client's
// Accept converts nothing: a manifest of a type the client did not list is returned all the
// same (the spec allows a 404 instead; docker and oras list every type they read). A tag that
// is not one names no manifest: 404 MANIFEST_UNKNOWN, as the conformance suite wants of a read;
// a digest that is not one is 400 DIGEST_INVALID.
func (h *handler) getManifest(w http.ResponseWriter, r *http.Request, t *target, ref string) error {
	ctx := r.Context()
	var ae *apiError
	if _, err := parseReference(ref); errors.As(err, &ae) && ae.code == codeTagInvalid {
		return &apiError{status: http.StatusNotFound, code: codeManifestUnknown, msg: fmt.Sprintf("manifest unknown to registry: %q is not a tag", ref)}
	} else if err != nil {
		return err
	}
	if err := h.authorize(ctx, auth.ActionRead, t.repo); err != nil {
		return err
	}
	rr, err := h.backend(ctx, t)
	if err != nil {
		return err
	}
	desc, err := rr.Resolve(ctx, ref)
	if err != nil {
		return manifestUnknown(err)
	}
	var rc io.ReadCloser
	if r.Method == http.MethodGet {
		if rc, err = rr.Fetch(ctx, desc); err != nil {
			return manifestUnknown(err)
		}
		defer func() { _ = rc.Close() }()
	}
	hdr := w.Header()
	hdr.Set("Content-Type", desc.MediaType)
	hdr.Set("Docker-Content-Digest", desc.Digest.String())
	hdr.Set("Content-Length", strconv.FormatInt(desc.Size, 10))
	w.WriteHeader(http.StatusOK)
	if rc == nil {
		return nil
	}
	if _, err := io.Copy(w, rc); err != nil && ctx.Err() == nil {
		h.o.Log.WarnContext(ctx, "facade: the manifest stream failed after the response started", "name", t.name,
			"digest", desc.Digest.String(), "error", err.Error(), "requestId", server.RequestID(ctx))
	}
	return nil
}

// manifestUnknown is the answer to a failed resolve or fetch of a manifest.
func manifestUnknown(err error) error {
	if errors.Is(err, provider.ErrNotFound) {
		return &apiError{code: codeManifestUnknown, msg: "manifest unknown to registry", err: err}
	}
	return &apiError{code: codeManifestUnknown, err: err}
}

// putManifest answers PUT …/manifests/<ref>. The body (Content-Length required, at most
// Options.MaxManifestSize) is read and checked to be a manifest of one of manifestMediaTypes
// whose mediaType field, when it has one, is the Content-Type. On a repo of format oci the
// manifest then passes through: it is pushed with its tag (PushReference) or by its digest,
// which must be the reference's, oci_repos records the OCI name, and the answer is 201. Every
// other format goes through the manifest hook into ingest (putManaged).
func (h *handler) putManifest(w http.ResponseWriter, r *http.Request, t *target, ref string) error {
	ctx := r.Context()
	refDigest, err := parseReference(ref)
	if err != nil {
		return err
	}
	if err := h.authorize(ctx, auth.ActionWrite, t.repo); err != nil {
		return err
	}
	desc, body, err := readManifest(r, refDigest, h.o.MaxManifestSize)
	if err != nil {
		return err
	}
	if t.repo.Format != ociFormat {
		return h.putManaged(w, r, t, ref, refDigest, desc, body)
	}
	rr, err := h.backend(ctx, t)
	if err != nil {
		return err
	}
	if refDigest != "" {
		err = rr.Push(ctx, desc, bytes.NewReader(body))
	} else {
		err = rr.PushReference(ctx, desc, bytes.NewReader(body), ref)
	}
	switch {
	case errors.Is(err, provider.ErrConflict):
		return &apiError{code: codeTagInvalid, err: err}
	case err != nil:
		return &apiError{code: codeManifestInvalid, err: err}
	}
	if err := h.o.Index.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: t.repo.ID, Path: t.oci, SeenAt: h.o.Now()}); err != nil {
		// The registry has the manifest and is the truth; reindex records the repository.
		h.o.Log.ErrorContext(ctx, "facade: record the OCI repository", "name", t.name, "error", err.Error(),
			"requestId", server.RequestID(ctx))
	}
	w.Header().Set("Location", "/v2/"+t.name+"/manifests/"+desc.Digest.String())
	w.Header().Set("Docker-Content-Digest", desc.Digest.String())
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
	return nil
}

// readManifest reads the manifest of a PUT, at most limit bytes, and returns its descriptor
// and bytes; refDigest, when not empty, is the digest the reference names, whose algorithm
// digests the body.
func readManifest(r *http.Request, refDigest digest.Digest, limit int64) (ocispec.Descriptor, []byte, error) {
	switch {
	case r.ContentLength < 0:
		return ocispec.Descriptor{}, nil, &apiError{status: http.StatusLengthRequired, code: codeManifestInvalid, msg: "a manifest needs a Content-Length"}
	case r.ContentLength > limit:
		return ocispec.Descriptor{}, nil, &apiError{status: http.StatusRequestEntityTooLarge, code: codeManifestInvalid,
			msg: fmt.Sprintf("manifest of %d bytes, the limit is %d", r.ContentLength, limit)}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, r.ContentLength))
	if err != nil || int64(len(body)) != r.ContentLength {
		return ocispec.Descriptor{}, nil, &apiError{status: http.StatusBadRequest, code: codeManifestInvalid, msg: "the manifest body broke off", err: err}
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !manifestMediaTypes[mediaType] {
		return ocispec.Descriptor{}, nil, &apiError{status: http.StatusBadRequest, code: codeManifestInvalid,
			msg: fmt.Sprintf("Content-Type %q is not a manifest media type", r.Header.Get("Content-Type"))}
	}
	var fields struct {
		MediaType string `json:"mediaType"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return ocispec.Descriptor{}, nil, &apiError{status: http.StatusBadRequest, code: codeManifestInvalid, msg: "the manifest is not JSON", err: err}
	}
	if fields.MediaType != "" && fields.MediaType != mediaType {
		return ocispec.Descriptor{}, nil, &apiError{status: http.StatusBadRequest, code: codeManifestInvalid,
			msg: fmt.Sprintf("the manifest's mediaType %q is not its Content-Type %q", fields.MediaType, mediaType)}
	}
	alg := digest.Canonical
	if refDigest != "" {
		alg = refDigest.Algorithm()
	}
	d := alg.FromBytes(body)
	if refDigest != "" && d != refDigest {
		return ocispec.Descriptor{}, nil, &apiError{status: http.StatusBadRequest, code: codeDigestInvalid,
			msg: fmt.Sprintf("the manifest's digest is %s, not %s", d, refDigest)}
	}
	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(body))}, body, nil
}

// deleteManifest answers DELETE …/manifests/<ref>: 405 until #61.
func (h *handler) deleteManifest(_ http.ResponseWriter, r *http.Request, t *target, _ string) error {
	if err := h.authorize(r.Context(), auth.ActionDelete, t.repo); err != nil {
		return err
	}
	return &apiError{status: http.StatusMethodNotAllowed, code: codeUnsupported, msg: "deleting manifests comes with #61"}
}

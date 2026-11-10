package facade

import (
	"bytes"
	"context"
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
	"github.com/lajstrom/lajstrom/internal/format"
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
// a digest that is not one is 400 DIGEST_INVALID. The read is authorized before the reference
// is parsed, so that a principal that may not read the repo gets the answer of a repo that
// does not exist, whatever the reference (docs/spec/rbac.md §3).
func (h *handler) getManifest(w http.ResponseWriter, r *http.Request, t *target, ref string) error {
	ctx := r.Context()
	if err := h.authorize(ctx, auth.ActionRead, t); err != nil {
		return err
	}
	var ae *apiError
	if _, err := parseReference(ref); errors.As(err, &ae) && ae.code == codeTagInvalid {
		return &apiError{status: http.StatusNotFound, code: codeManifestUnknown, msg: fmt.Sprintf("manifest unknown to registry: %q is not a tag", ref)}
	} else if err != nil {
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
// which must be the reference's (passThrough; a manifest with a subject goes through ingest),
// oci_repos records the OCI name and the audit log the push of a manifest without a subject
// (recordPassThrough); the answer is 201, with OCI-Subject for a manifest with a subject: the
// facade serves the referrers API, and the provider writes the tag schema where the backend
// has none. Every other format goes through the manifest hook into ingest
// (putManaged).
func (h *handler) putManifest(w http.ResponseWriter, r *http.Request, t *target, ref string) error {
	ctx := r.Context()
	if err := h.authorize(ctx, auth.ActionWrite, t); err != nil {
		return err
	}
	refDigest, err := parseReference(ref)
	if err != nil {
		return err
	}
	desc, body, err := readManifest(r, refDigest, h.o.MaxManifestSize)
	if err != nil {
		return err
	}
	if t.repo.Format != ociFormat {
		return h.putManaged(w, r, t, ref, refDigest, desc, body)
	}
	if err := h.passThrough(r, t, ref, refDigest, desc, body); err != nil {
		return err
	}
	subject := subjectOf(body)
	tag := ref
	if refDigest != "" {
		tag = ""
	}
	h.recordPassThrough(ctx, t, tag, desc.Digest, subject == nil)
	created(w, t, desc.Digest, subject)
	return nil
}

// recordPassThrough records the manifest d that passThrough stored in t, an oci repo, in one
// transaction: oci_repos gets the OCI name, and the audit log a push row when audit is set
// (docs/spec/rbac.md §5). audit is false for a manifest with a subject, which ingest audited as
// a referrer. The row has the principal, the repo, the OCI name as the subject, the digest, the
// client address (server.ClientAddr), and the tag, "" for a push by digest, and the request id
// in the details. Every PUT writes one, the repeat of a manifest the backend has too: no index
// row tells the facade that it was there, as a version does for a managed repo. The backend has
// the manifest and is the truth, so a failure is logged: reindex records the repository, and
// the log line is what is left of the push.
func (h *handler) recordPassThrough(ctx context.Context, t *target, tag string, d digest.Digest, audit bool) {
	now := h.o.Now()
	err := h.o.Index.Tx(ctx, func(w index.Writer) error {
		if err := w.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: t.repo.ID, Path: t.oci, SeenAt: now}); err != nil {
			return err
		}
		if !audit {
			return nil
		}
		details := map[string]string{}
		if tag != "" {
			details["tag"] = tag
		}
		if id := server.RequestID(ctx); id != "" {
			details["requestId"] = id
		}
		ev := index.AuditEvent{At: now, Principal: auth.PrincipalFrom(ctx).String(), Action: "push",
			Repo: t.repo.Format + "/" + t.repo.Name, Subject: t.oci, Digest: d, IP: server.ClientAddr(ctx)}
		if len(details) > 0 {
			ev.Details, _ = json.Marshal(details) // strings always marshal
		}
		return w.AppendAudit(ctx, &ev)
	})
	if err != nil {
		h.o.Log.ErrorContext(ctx, "facade: record the OCI repository and the push in the audit log", "name", t.name,
			"digest", d.String(), "principal", auth.PrincipalFrom(ctx).String(), "error", err.Error(), "requestId", server.RequestID(ctx))
	}
}

// passThrough stores a manifest of an oci repo in the backend as it came, by digest or under the
// tag ref: a manifest with a subject through ingest (ingestReferrer), which takes the referrers
// lock where the backend has no referrers API, any other one directly.
func (h *handler) passThrough(r *http.Request, t *target, ref string, refDigest digest.Digest, desc ocispec.Descriptor, body []byte) error {
	tag := ref
	if refDigest != "" {
		tag = ""
	}
	if subject := subjectOf(body); subject != nil {
		return h.ingestReferrer(r, t, tag, desc, body, subject.Digest)
	}
	ctx := r.Context()
	rr, err := h.backend(ctx, t)
	if err != nil {
		return err
	}
	if tag == "" {
		err = rr.Push(ctx, desc, bytes.NewReader(body))
	} else {
		err = rr.PushReference(ctx, desc, bytes.NewReader(body), tag)
	}
	switch {
	case errors.Is(err, provider.ErrConflict):
		return &apiError{code: codeTagInvalid, err: err}
	case err != nil:
		return &apiError{code: codeManifestInvalid, err: err}
	}
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
		Subject   *struct {
			Digest string `json:"digest"`
		} `json:"subject"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return ocispec.Descriptor{}, nil, &apiError{status: http.StatusBadRequest, code: codeManifestInvalid, msg: "the manifest is not JSON", err: err}
	}
	// The subject's digest goes back in the OCI-Subject header of the answer: a digest only.
	if fields.Subject != nil {
		if _, err := digest.Parse(fields.Subject.Digest); err != nil {
			return ocispec.Descriptor{}, nil, &apiError{status: http.StatusBadRequest, code: codeManifestInvalid,
				msg: fmt.Sprintf("the manifest's subject has the digest %q, which is not one", fields.Subject.Digest)}
		}
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

// deleteManifest answers DELETE …/manifests/<ref> (docs/spec/http-routing.md §2): delete on
// the repo is checked before the reference, which ingest resolves and deletes with its
// referrers (Ingester.Delete): a version of a managed repo with its rows in the index, any
// other manifest as the backend has it. 202 Accepted without a body; deleteError has the
// errors.
func (h *handler) deleteManifest(w http.ResponseWriter, r *http.Request, t *target, ref string) error {
	ctx := r.Context()
	if err := h.authorize(ctx, auth.ActionDelete, t); err != nil {
		return err
	}
	if _, err := parseReference(ref); err != nil {
		return err
	}
	if h.o.Ingest == nil {
		return errors.New("facade: no ingest pipeline: Options.Ingest is required")
	}
	del := format.Deletion{Path: t.path, Reference: ref, ClientIP: server.ClientAddr(ctx)}
	if _, err := h.o.Ingest.Delete(ctx, t.repo, del, auth.PrincipalFrom(ctx)); err != nil {
		return deleteError(t, ref, err)
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

// deleteError is the error table of a manifest delete:
//
//	provider.ErrUnsupported   405 UNSUPPORTED: the backend does not delete manifests
//	index.ErrNotFound         404 MANIFEST_UNKNOWN
//	provider.ErrConflict      409 DENIED: the backend refused this one delete
//	anything else             format.StatusOf: 502 for the backend's credentials, 503 for an
//	                          open circuit breaker, else 500, logged
func deleteError(t *target, ref string, err error) error {
	switch {
	case errors.Is(err, provider.ErrUnsupported):
		return &apiError{status: http.StatusMethodNotAllowed, code: codeUnsupported, err: err,
			msg: fmt.Sprintf("the registry %q of %s/%s does not delete manifests; nothing was deleted", t.repo.Registry, t.repo.Format, t.repo.Name)}
	case errors.Is(err, index.ErrNotFound), errors.Is(err, provider.ErrNotFound):
		return &apiError{status: http.StatusNotFound, code: codeManifestUnknown, err: err,
			msg: fmt.Sprintf("manifest unknown to registry: %s in %s", ref, t.name)}
	case errors.Is(err, provider.ErrConflict):
		return &apiError{status: http.StatusConflict, code: codeDenied, err: err,
			msg: fmt.Sprintf("the registry refused to delete %s in %s: another manifest refers to it", ref, t.name)}
	}
	return &apiError{err: err}
}

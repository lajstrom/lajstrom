package facade

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// putReferrer is the manifest hook of a referrer of a managed repo (profile.IsReferrer: a
// signature, an SBOM or an attestation of a version, or cosign's legacy tag), which write on
// the repo allowed: ingest stores it as the client pushed it and indexes nothing
// (Pipeline.PutReferrer, docs/spec/manifest-profile.md §6). The facade checks the blobs first,
// for the answer's code, as for an artifact (checkLayers), and pushes nothing itself. The answer
// is that of a push, 201 with OCI-Subject when the manifest has a subject.
func (h *handler) putReferrer(w http.ResponseWriter, r *http.Request, t *target, ref string, refDigest digest.Digest,
	desc ocispec.Descriptor, body []byte, m ocispec.Manifest,
) error {
	ctx := r.Context()
	tag, subject := "", digest.Digest("")
	if refDigest == "" {
		tag = ref
	}
	if s, _, ok := profile.CosignTag(tag); ok {
		subject = s
	} else if m.Subject != nil {
		subject = m.Subject.Digest
	}
	if err := h.checkLayers(ctx, t, append([]ocispec.Descriptor{m.Config}, m.Layers...)); err != nil {
		return err
	}
	if err := h.ingestReferrer(r, t, tag, desc, body, subject); err != nil {
		return err
	}
	created(w, t, desc.Digest, m.Subject)
	return nil
}

// ingestReferrer passes a referrer of t to ingest, pushed under tag ("" for a push by digest):
// on a managed repo from putReferrer, on an oci repo from putManifest for every manifest with a
// subject. Its errors become answers here:
//
//	ingest.ErrInvalid           400 MANIFEST_INVALID, the rule that failed in the message
//	ingest.ErrConflict          409 TAG_INVALID (a backend that will not re-point a tag)
//	provider.ErrUnauthorized    502 (the backend refused Lajstrom's credentials)
//	*ingest.NotVisibleError     503 UNAVAILABLE naming what the registry does not serve yet
//	                            (notServed)
//	anything else               the status of format.StatusOf, 500 when it knows none
func (h *handler) ingestReferrer(r *http.Request, t *target, tag string, desc ocispec.Descriptor, body []byte, subject digest.Digest) error {
	ctx := r.Context()
	if h.o.Ingest == nil {
		return fmt.Errorf("facade: a referrer for %s, and Options.Ingest is nil", t.name)
	}
	_, err := h.o.Ingest.PutReferrer(ctx, t.repo, ingest.Referrer{Path: t.path, Tag: tag, Desc: desc, Manifest: body, Subject: subject},
		auth.PrincipalFrom(ctx))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ingest.ErrInvalid):
		return &apiError{status: http.StatusBadRequest, code: codeManifestInvalid, msg: validationText(err), err: err}
	case errors.Is(err, ingest.ErrConflict):
		return &apiError{status: http.StatusConflict, code: codeTagInvalid, err: err}
	case errors.Is(err, provider.ErrUnauthorized):
		return &apiError{status: http.StatusBadGateway, err: err}
	}
	var notVisible *ingest.NotVisibleError
	if errors.As(err, &notVisible) {
		return notServed(t, notVisible, err)
	}
	return &apiError{code: codeManifestInvalid, err: err}
}

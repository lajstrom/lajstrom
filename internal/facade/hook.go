package facade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// dockerManifestList is the media type of a Docker image index.
const dockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"

// unknownArtifactType is the artifactType oras gives a manifest pushed without --artifact-type.
const unknownArtifactType = "application/vnd.unknown.artifact.v1"

// putManaged is the manifest hook: a manifest pushed to a repo Lajstrom manages, every format
// but oci, is stored and indexed by ingest, the one write path (docs/spec/http-routing.md §2,
// docs/architecture.md flow A). desc and body are what readManifest read; refDigest is the
// digest of the reference, "" for a tag.
//
//  1. The manifest must be an OCI image manifest (docs/spec/manifest-profile.md §1): an image
//     index or a Docker manifest is MANIFEST_INVALID, naming what the profile wants. A
//     referrer, a signature or an SBOM of a version (profile.IsReferrer), goes to ingest's
//     referrer path unindexed (putReferrer, manifest-profile.md §6) instead of the steps below.
//     A proxy repo takes nothing else: any other manifest, valid or not, is 405 UNSUPPORTED
//     (proxyRefusal) before anything is asked of the backend or of ingest.
//  2. The package path is the <path> of the name, the tag the reference or, for a push by
//     digest, the tag of the annotations (manifestTag).
//  3. The manifest must satisfy the profile (profile.Validate), which needs no backend, else
//     MANIFEST_INVALID as ingest answers it; readManifest has bounded the layers (maxLayers).
//     Its created time must be near the server's clock, else MANIFEST_INVALID (checkCreated).
//  4. Every layer must be in the backend, else MANIFEST_BLOB_UNKNOWN (checkLayers); on a
//     registry not classified strong, a layer it does not serve is read again first.
//  5. ingest.Ingest validates the manifest, claims the tag, pushes the manifest with it and
//     indexes it; ingestError answers its errors.
//
// The answer is 201 with Location (the digest, or the tag of a push by digest),
// Docker-Content-Digest (the manifest the tag holds, also for the idempotent repeat) and, when
// the manifest has a subject, OCI-Subject.
func (h *handler) putManaged(w http.ResponseWriter, r *http.Request, t *target, ref string, refDigest digest.Digest, desc ocispec.Descriptor, body []byte) error {
	ctx := r.Context()
	if h.o.Ingest == nil {
		return fmt.Errorf("facade: a manifest for %s, and Options.Ingest is nil", t.name)
	}
	m, err := decodeManaged(desc.MediaType, body)
	referrer := err == nil && profile.IsReferrer(map[bool]string{true: ref}[refDigest == ""], m)
	switch {
	case t.repo.Kind == index.KindProxy && !referrer:
		return proxyRefusal(t)
	case err != nil:
		return err
	case referrer:
		return h.putReferrer(w, r, t, ref, refDigest, desc, body, m)
	}
	tag, err := manifestTag(ref, refDigest, m)
	if err != nil {
		return err
	}
	// The profile needs no backend: a manifest it refuses costs no request to the registry
	// (threat model M-2). Ingest checks it again, as it does for every publish.
	parsed, err := profile.Validate(m)
	if err != nil {
		return ingestError(t, m, fmt.Errorf("%w: %w", ingest.ErrInvalid, err))
	}
	if err := h.checkCreated(ctx, t, desc.Digest, parsed.Annotations.Created); err != nil {
		return err
	}
	if err := h.checkLayers(ctx, t, m.Layers); err != nil {
		return err
	}
	res, err := h.o.Ingest.Ingest(ctx, t.repo, t.path, tag, m, body, auth.PrincipalFrom(ctx))
	if err != nil {
		return ingestError(t, m, err)
	}
	loc := res.Digest.String()
	if refDigest != "" {
		loc = res.Tag
	}
	hdr := w.Header()
	hdr.Set("Location", "/v2/"+t.name+"/manifests/"+loc)
	hdr.Set("Docker-Content-Digest", res.Digest.String())
	if m.Subject != nil {
		// The facade serves the referrers API (referrers.go), whatever the backend has.
		hdr.Set("OCI-Subject", m.Subject.Digest.String())
	}
	hdr.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
	return nil
}

// proxyRefusal is the answer to a manifest pushed to the proxy repo of t that is not a
// referrer: a proxy repo holds what it fetched from its upstream, and a client adds to it only
// the referrers of what it holds (docs/spec/http-routing.md §6, proxy-and-group.md §1). 405
// UNSUPPORTED without Allow, since a PUT of a referrer is allowed. Its blob uploads, reads and
// deletes are a hosted repo's.
func proxyRefusal(t *target) error {
	return &apiError{status: http.StatusMethodNotAllowed, code: codeUnsupported,
		msg: fmt.Sprintf("%s/%s is a proxy repo: it holds what its upstream serves, and on /v2 it takes only referrers (signatures, attestations, SBOMs) of the versions it holds; publish to a hosted repo",
			t.repo.Format, t.repo.Name)}
}

// decodeManaged returns the manifest of a PUT to a managed repo, which must be an OCI image
// manifest of the media type mediaType: an image index (OCI or Docker), a Docker manifest or an
// OCI artifact manifest is not a Lajstrom artifact, and an oci repo takes it.
func decodeManaged(mediaType string, body []byte) (ocispec.Manifest, error) {
	switch mediaType {
	case ocispec.MediaTypeImageManifest:
	case ocispec.MediaTypeImageIndex, dockerManifestList:
		return ocispec.Manifest{}, &apiError{status: http.StatusBadRequest, code: codeManifestInvalid,
			msg: "an image index is not a Lajstrom artifact; an oci repo takes it"}
	default:
		return ocispec.Manifest{}, &apiError{status: http.StatusBadRequest, code: codeManifestInvalid,
			msg: fmt.Sprintf("a manifest of media type %s is not a Lajstrom artifact: the profile wants %s; an oci repo takes it",
				mediaType, ocispec.MediaTypeImageManifest)}
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return ocispec.Manifest{}, &apiError{status: http.StatusBadRequest, code: codeManifestInvalid,
			msg: "the body is not an OCI image manifest: " + err.Error(), err: err}
	}
	return m, nil
}

// manifestTag returns the tag to store m under: the reference when it is a tag. A push by
// digest is accepted when the annotations yield a tag, Tag(lajstrom.version, lajstrom.variant)
// (docs/spec/manifest-profile.md §4.2), which ingest then sets in the backend. Only those two
// keys are read, not profile.ParseAnnotations, which returns no version when any other required
// key is missing: the other keys are ingest's to check, as for a push by tag.
func manifestTag(ref string, refDigest digest.Digest, m ocispec.Manifest) (string, error) {
	if refDigest == "" {
		return ref, nil
	}
	version := m.Annotations[profile.AnnotationVersion]
	if version == "" {
		return "", &apiError{status: http.StatusBadRequest, code: codeManifestInvalid,
			msg: "a push by digest needs the " + profile.AnnotationVersion + " annotation; push with a tag"}
	}
	return profile.Tag(version, m.Annotations[profile.AnnotationVariant]), nil
}

// checkCreated answers MANIFEST_INVALID when created, org.opencontainers.image.created of the
// manifest d, is more than Options.MaxCreatedAhead after the server's clock or more than
// MaxCreatedBehind before it. It is a version's publish time, which the age rules and the
// cut-off of retention read (docs/spec/guarantees.md, INV-41): the frontends write it from the
// server's clock, a /v2 client from its own. The time is refused, not replaced: another time is
// another digest, which a push by digest cannot have and which oras, checking
// Docker-Content-Digest, takes for a failed push. A manifest that the repo has already passes
// whatever its time: its repeat writes nothing.
func (h *handler) checkCreated(ctx context.Context, t *target, d digest.Digest, created time.Time) error {
	now := h.o.Now()
	var off string
	switch {
	case created.After(now.Add(h.o.MaxCreatedAhead)):
		off = created.Sub(now).String() + " ahead of"
	case created.Before(now.Add(-h.o.MaxCreatedBehind)):
		off = now.Sub(created).String() + " behind"
	default:
		return nil
	}
	switch _, err := h.o.Index.GetVersionByDigest(ctx, t.repo.ID, d); {
	case err == nil:
		return nil
	case !errors.Is(err, index.ErrNotFound):
		return fmt.Errorf("facade: version %s of %s: %w", d, t.name, err)
	}
	return &apiError{status: http.StatusBadRequest, code: codeManifestInvalid,
		msg: fmt.Sprintf("%s %s is %s the server's clock, %s: it may be at most %s ahead and %s behind; push again, with the current time",
			ocispec.AnnotationCreated, created.Format(time.RFC3339), off, now.UTC().Format(time.RFC3339),
			h.o.MaxCreatedAhead, h.o.MaxCreatedBehind)}
}

// checkLayers answers MANIFEST_BLOB_UNKNOWN, naming the digest, when a blob of blobs (the
// layers of a manifest; the config too for a referrer) is not in the backend. Ingest checks the
// layers again (and pushes the empty config, which is Lajstrom's own), but there a missing layer
// is ingest.ErrInvalid, MANIFEST_INVALID: the second HEAD per layer is the price of the right
// code without a new error in the ingest contract. A blob whose digest is malformed is left to
// the profile check of ingest, or to the backend, which names it. Each digest is asked once, so
// a manifest that names one blob many times costs one HEAD. A blob the backend does not serve
// is read again (readBack) unless the registry is strongly consistent: a client uploads its
// blobs right before the manifest, and on ECR a blob can be 404 for a moment after its 201.
func (h *handler) checkLayers(ctx context.Context, t *target, blobs []ocispec.Descriptor) error {
	rr, err := h.backend(ctx, t)
	if err != nil {
		return err
	}
	seen := make(map[digest.Digest]bool, len(blobs))
	var deadline time.Time // of the read-back, from the first blob it reads again
	for _, l := range blobs {
		if seen[l.Digest] || l.Digest.Validate() != nil {
			continue
		}
		seen[l.Digest] = true
		ok, err := rr.Exists(ctx, l)
		if err == nil && !ok {
			ok, err = h.readBack(ctx, t, rr, l, &deadline)
		}
		switch {
		case err != nil:
			return fmt.Errorf("facade: layer %s of %s: %w", l.Digest, t.name, err)
		case !ok:
			return &apiError{status: http.StatusBadRequest, code: codeManifestBlobUnknown,
				msg: fmt.Sprintf("manifest blob unknown to registry: blob %s was not uploaded to %s", l.Digest, t.name)}
		}
	}
	return nil
}

// readBack reads the blob l of rr again, which the backend did not serve, until it does or
// *deadline: the visibility timeout (Options.VisibilityTimeout, or the registry's
// consistencyBound when longer) from the first blob of the manifest read again, so that the
// blobs of one manifest share one wait. A strongly consistent registry is not asked again.
func (h *handler) readBack(ctx context.Context, t *target, rr provider.Repository, l ocispec.Descriptor, deadline *time.Time) (bool, error) {
	p, err := h.registry(t)
	if err != nil {
		return false, err
	}
	caps, err := p.Capabilities(ctx)
	if err != nil {
		return false, fmt.Errorf("facade: capabilities of the registry of %s: %w", t.name, err)
	}
	if caps.Consistency == provider.ConsistencyStrong {
		return false, nil
	}
	if deadline.IsZero() {
		*deadline = time.Now().Add(max(h.o.VisibilityTimeout, caps.ConsistencyBound))
	}
	return provider.ReadBack{Timeout: time.Until(*deadline)}.Await(ctx, func(ctx context.Context) (bool, error) {
		return rr.Exists(ctx, l)
	})
}

// ingestError is the error table of the manifest hook, the one place where the errors of
// ingest become answers:
//
//	*http.MaxBytesError         413 SIZE_INVALID: a file above the repo's maxFileSize, named
//	ingest.ErrInvalid           400 MANIFEST_INVALID, the rule that failed in detail
//	*index.TagConflictError     409 TAG_INVALID, the digest the tag holds in the message and in
//	                            detail.existing
//	ingest.ErrConflict          409 TAG_INVALID with the conflict's message (a package path
//	                            that another spelling holds or is being published to, #244)
//	format.ErrUnsupported       501 UNSUPPORTED
//	provider.ErrUnauthorized    502 (the backend refused Lajstrom's credentials)
//	*ingest.NotVisibleError     503 UNAVAILABLE with Retry-After: the registry took the manifest
//	                            and did not serve it within the visibility timeout, which the
//	                            same PUT again completes; the message and the detail name what
//	                            it does not serve yet (notServed)
//	index.ErrUnavailable,       503 UNAVAILABLE with Retry-After (format.StatusOf): the database
//	provider.ErrUnavailable     or the registry takes no request now
//	anything else               500, logged with the request id
//
// The messages of ingest name annotations, rules, digests and OCI names, never a backend URL.
// A manifest with oras's default artifactType gets the hint of the one it needs.
func ingestError(t *target, m ocispec.Manifest, err error) error {
	var (
		conflict   *index.TagConflictError
		tooLarge   *http.MaxBytesError
		notVisible *ingest.NotVisibleError
	)
	switch {
	case errors.As(err, &tooLarge):
		return &apiError{status: http.StatusRequestEntityTooLarge, code: codeSizeInvalid, msg: err.Error(), err: err}
	case errors.Is(err, ingest.ErrInvalid):
		msg := fmt.Sprintf("the manifest is not a valid %s artifact for %s", t.repo.Format, t.name)
		if m.ArtifactType == unknownArtifactType {
			msg += "; pass --artifact-type " + artifactTypeOf(t.repo.Format)
		}
		return &apiError{status: http.StatusBadRequest, code: codeManifestInvalid, msg: msg, detail: validationText(err), err: err}
	case errors.As(err, &conflict):
		return &apiError{status: http.StatusConflict, code: codeTagInvalid,
			msg:    fmt.Sprintf("tag %s of %s is taken by %s: tags are immutable", conflict.Tag, t.name, conflict.Existing),
			detail: map[string]string{"existing": conflict.Existing.String()}, err: err}
	case errors.Is(err, ingest.ErrConflict):
		return &apiError{status: http.StatusConflict, code: codeTagInvalid, err: err}
	case errors.Is(err, format.ErrUnsupported):
		return &apiError{status: http.StatusNotImplemented, code: codeUnsupported, err: err}
	case errors.Is(err, provider.ErrUnauthorized):
		return &apiError{status: http.StatusBadGateway, err: err}
	case errors.As(err, &notVisible):
		return notServed(t, notVisible, err)
	case errors.Is(err, index.ErrUnavailable), errors.Is(err, provider.ErrUnavailable):
		return &apiError{err: err}
	}
	return &apiError{status: http.StatusInternalServerError, err: err}
}

// maxNamedBlobs is the most blobs that the answer to a push the registry does not serve yet
// names (notServed); it counts the rest. A manifest names up to maxLayers blobs and its config,
// and the answer would name each twice, in the message and in the detail (#693).
const maxNamedBlobs = 20

// notServed is the answer to a push that the registry took and does not serve yet, nv: 503
// UNAVAILABLE with the Retry-After of nv's cause, whose message and detail name what the
// registry does not serve, by the name the client pushed to (t.name, not the OCI name of the
// backend) and the digests and the tag it pushed (docs/spec/http-routing.md §2, #565): of the
// blobs, the first maxNamedBlobs, and how many more. A client's log then tells a registry that
// lags from one that lost the push.
func notServed(t *target, nv *ingest.NotVisibleError, err error) *apiError {
	named := *nv
	named.Blobs = nv.Blobs[:min(len(nv.Blobs), maxNamedBlobs)]
	more := len(nv.Blobs) - len(named.Blobs)
	missing := named.Missing()
	if more > 0 {
		// After the blobs named: Missing ends in the subject's referrers, when they are missing.
		at := len(missing)
		if nv.Subject != "" {
			at--
		}
		missing = slices.Insert(missing, at, moreBlobs(more))
	}
	return &apiError{status: http.StatusServiceUnavailable, code: codeUnavailable, told: true, err: err,
		msg: fmt.Sprintf("the registry took %s@%s and does not serve %s yet; the same push again completes it",
			t.name, nv.Digest, strings.Join(missing, ", ")),
		detail: notServedDetail{Name: t.name, Digest: nv.Digest, Missing: missingDetail{Manifest: nv.Manifest, Tag: nv.Tag,
			Blobs: named.Blobs, MoreBlobs: more, ReferrersOf: nv.Subject}}}
}

// moreBlobs is "1 more blob", "n more blobs": the blobs notServed counts and does not name.
func moreBlobs(n int) string {
	if n == 1 {
		return "1 more blob"
	}
	return fmt.Sprintf("%d more blobs", n)
}

// notServedDetail is the detail of notServed: the manifest pushed, and what of it the registry
// does not serve yet.
type notServedDetail struct {
	Name    string        `json:"name"`
	Digest  digest.Digest `json:"digest"`
	Missing missingDetail `json:"missing"`
}

// missingDetail is what the registry does not serve yet of a push: each key only when it does
// not serve it.
type missingDetail struct {
	Manifest    bool            `json:"manifest,omitempty"`    // the manifest, by its digest
	Tag         string          `json:"tag,omitempty"`         // the tag, which does not name the manifest
	Blobs       []digest.Digest `json:"blobs,omitempty"`       // the blobs the manifest names, the first maxNamedBlobs
	MoreBlobs   int             `json:"moreBlobs,omitempty"`   // how many more blobs, past those Blobs names
	ReferrersOf digest.Digest   `json:"referrersOf,omitempty"` // a referrer: the subject whose referrers do not list it
}

// artifactTypeOf is the artifactType of the repo format f: apt stores deb.
func artifactTypeOf(f string) string {
	if typ, ok := profile.TypeByFormat(f); ok {
		return profile.ArtifactType(typ.Name)
	}
	return profile.ArtifactType(f)
}

// validationText is the detail of a MANIFEST_INVALID: the message of ingest without the text of
// the ErrInvalid sentinel, which the code says, on one line (the profile joins its findings with
// newlines).
func validationText(err error) string {
	s := strings.ReplaceAll(err.Error(), ingest.ErrInvalid.Error()+": ", "")
	return strings.ReplaceAll(s, "\n", "; ")
}

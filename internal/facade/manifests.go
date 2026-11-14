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
	"slices"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
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

// maxLayers is the most layers a manifest PUT may name, on every repo: an image has tens, a
// ModelPack artifact hundreds. The facade asks the registry about each layer of a managed
// manifest, and ingest asks again, so without a bound one PUT of 4 MiB with 25 000 layers made
// 50 000 requests to the registry (threat model M-2).
const maxLayers = 1000

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
// does not exist, whatever the reference (docs/spec/rbac.md §3). The body is checked against
// its digest as it streams (streamManifest).
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
	if h.ociProxy(t) {
		return h.getProxyManifest(w, r, t, ref)
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
	if rc != nil {
		h.streamManifest(ctx, w, t, desc, rc)
	}
	return nil
}

// streamManifest copies the manifest desc from body to the client through a verifier: its last
// byte goes out only when the bytes have the digest of desc, and otherwise the response is
// aborted as format.ServeBlob aborts a blob's (docs/spec/guarantees.md, INV-40). A stream that
// breaks is logged.
func (h *handler) streamManifest(ctx context.Context, w io.Writer, t *target, desc ocispec.Descriptor, body io.Reader) {
	if desc.Digest.Validate() == nil {
		body = &verifier{r: body, h: desc.Digest.Algorithm().Hash(), want: desc.Digest}
	}
	_, err := io.Copy(w, body)
	switch {
	case errors.Is(err, errNotDigest):
		metrics.DownloadDigestMismatches.WithLabelValues(t.repo.Registry).Inc()
		panic(fmt.Errorf("facade: manifest %s@%s: the bytes from registry %s do not have the digest, the response is aborted: %w",
			t.name, desc.Digest, t.repo.Registry, http.ErrAbortHandler))
	case err != nil && ctx.Err() == nil:
		h.o.Log.WarnContext(ctx, "facade: the manifest stream failed after the response started", "name", t.name,
			"digest", desc.Digest.String(), "error", err.Error(), "requestId", server.RequestID(ctx))
	}
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
// and oci_repos records the OCI name (recordPassThrough); the audit row of the push of a
// manifest without a subject is written pending before it and completed after it, ok or failed
// (intendPassThrough); the answer is 201, with OCI-Subject for a manifest with a subject: the
// facade serves the referrers API, and the provider writes the tag schema where the backend
// has none. Every other format goes through the manifest hook into ingest
// (putManaged), which takes only referrers on a proxy repo.
func (h *handler) putManifest(w http.ResponseWriter, r *http.Request, t *target, ref string) error {
	ctx := r.Context()
	if err := h.authorize(ctx, auth.ActionWrite, t); err != nil {
		return err
	}
	if err := h.checkName(ctx, t); err != nil { // an oci repo's push goes to the backend from here
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
	subject := subjectOf(body)
	if subject == nil && t.repo.Kind == index.KindProxy {
		return refuseProxyPush(t)
	}
	tag := ref
	if refDigest != "" {
		tag = ""
	}
	var ev *index.AuditEvent // a manifest with a subject is ingest's to audit, as a referrer
	if subject == nil {
		if ev, err = h.intendPassThrough(ctx, t, tag, desc.Digest); err != nil {
			return err
		}
	}
	if err := h.passThrough(r, t, ref, refDigest, desc, body); err != nil {
		h.completePassThrough(ctx, ev, err)
		return err
	}
	h.recordPassThrough(ctx, t, desc.Digest, ev)
	created(w, t, desc.Digest, subject)
	return nil
}

// intendPassThrough writes the audit row of the push of the manifest d to t, an oci repo,
// pending, before the push (docs/spec/rbac.md §5): the principal, the repo, the OCI name as the
// subject, the digest, the client address (server.ClientAddr), and the tag, "" for a push by
// digest, and the request id in the details. Every PUT writes one, the repeat of a manifest the
// backend has too: no index row tells the facade that it was there, as a version does for a
// managed repo. No push goes without its row, so a failure is the PUT's.
func (h *handler) intendPassThrough(ctx context.Context, t *target, tag string, d digest.Digest) (*index.AuditEvent, error) {
	details := map[string]string{}
	if tag != "" {
		details["tag"] = tag
	}
	if id := server.RequestID(ctx); id != "" {
		details["requestId"] = id
	}
	ev := &index.AuditEvent{At: h.o.Now(), Principal: auth.PrincipalFrom(ctx).String(), Action: "push",
		Repo: t.repo.Format + "/" + t.repo.Name, Subject: t.oci, Digest: d, IP: server.ClientAddr(ctx), State: index.AuditPending}
	if len(details) > 0 {
		ev.Details, _ = json.Marshal(details) // strings always marshal
	}
	if err := h.o.Index.AppendAudit(ctx, ev); err != nil {
		return nil, fmt.Errorf("facade: the audit row of the push of %s before the push: %w", d, err)
	}
	return ev, nil
}

// completePassThrough marks the pending row ev ok, or failed with cause when cause is not nil,
// in a transaction of its own; nil ev is no row. A failure is logged, and the row stays pending
// until reindex or the reconcile loop marks it unknown.
func (h *handler) completePassThrough(ctx context.Context, ev *index.AuditEvent, cause error) {
	if ev == nil {
		return
	}
	ev.State, ev.Error = index.AuditOK, ""
	if cause != nil {
		ev.State, ev.Error = index.AuditFailed, cause.Error()
	}
	if err := h.o.Index.CompleteAudit(context.WithoutCancel(ctx), ev); err != nil {
		h.o.Log.ErrorContext(ctx, "facade: the audit row of a push not completed; it is unknown after ingest.claimTTL", "audit", ev.ID,
			"digest", ev.Digest.String(), "state", ev.State, "error", err.Error(), "requestId", server.RequestID(ctx))
	}
}

// recordPassThrough records the manifest d that passThrough stored in t, an oci repo, in one
// transaction: oci_repos gets the OCI name, and the audit row ev, when there is one, is ok. The
// backend has the manifest and is the truth, and the answer is 201, so a failure is logged and
// the row is completed on its own: reindex records the repository.
func (h *handler) recordPassThrough(ctx context.Context, t *target, d digest.Digest, ev *index.AuditEvent) {
	err := h.o.Index.Tx(ctx, func(w index.Writer) error {
		if err := w.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: t.repo.ID, Path: t.oci, SeenAt: h.o.Now()}); err != nil {
			return err
		}
		if ev == nil {
			return nil
		}
		ev.State = index.AuditOK
		return w.CompleteAudit(ctx, ev)
	})
	if err != nil {
		h.o.Log.ErrorContext(ctx, "facade: record the OCI repository and the push in the audit log", "name", t.name,
			"digest", d.String(), "principal", auth.PrincipalFrom(ctx).String(), "error", err.Error(), "requestId", server.RequestID(ctx))
		h.completePassThrough(ctx, ev, nil)
	}
}

// passThrough stores a manifest of an oci repo in the backend as it came, by digest or under the
// tag ref: a manifest with a subject through ingest (ingestReferrer), which takes the referrers
// lock where the backend has no referrers API and for a push by tag, any other one directly, by
// tag under the tag lock of its digest. A MANIFEST_BLOB_UNKNOWN that outlasts retryBlobsLag is
// the answer with the registry's status and code, as the hook's is MANIFEST_BLOB_UNKNOWN too,
// and names the first blob the registry does not have (nameMissing).
func (h *handler) passThrough(r *http.Request, t *target, ref string, refDigest digest.Digest, desc ocispec.Descriptor, body []byte) error {
	tag := ref
	if refDigest != "" {
		tag = ""
	}
	ctx := r.Context()
	if subject := subjectOf(body); subject != nil {
		return h.nameMissing(ctx, t, body, h.ingestReferrer(r, t, tag, desc, body, subject.Digest))
	}
	rr, err := h.backend(ctx, t)
	if err != nil {
		return err
	}
	push := func(ctx context.Context) error { return rr.Push(ctx, desc, bytes.NewReader(body)) }
	if tag != "" {
		// The tag lock of the manifest, which a delete holds while it counts the manifest's tags
		// (docs/spec/index-schema.md, Coordination): the tag is counted or pushed after it. A push
		// again takes it again, so that a delete does not wait out the backoff.
		push = func(ctx context.Context) error {
			return h.o.Index.WithReferrersLock(ctx, t.oci, desc.Digest, func(ctx context.Context) error {
				return rr.PushReference(ctx, desc, bytes.NewReader(body), tag)
			})
		}
	}
	err = h.retryBlobsLag(ctx, t, push)
	switch {
	case errors.Is(err, provider.ErrConflict):
		return &apiError{code: codeTagInvalid, err: err}
	case blobsUnknown(err):
		return h.nameMissing(ctx, t, body, &apiError{code: codeManifestBlobUnknown, err: err})
	case err != nil:
		return &apiError{code: codeManifestInvalid, err: err}
	}
	return nil
}

// nameMissing returns err, the registry's last MANIFEST_BLOB_UNKNOWN for the manifest body of t,
// an oci repo, with the first blob of body that the registry does not have named in the message,
// after the registry's, and in detail.digest, as checkLayers names a layer of a managed repo
// (#850). It sends one HEAD per distinct blob that body lists (listedBlobs), in order, and stops
// at the first the registry does not have; a layer with urls, which the registry need not hold,
// and a malformed digest, which the registry would name, are not asked. When the registry has
// them all, or a HEAD fails otherwise, err is the answer as it is: the registry's name, status
// and code. Any other err is returned as it is. The HEADs come only after the last refusal: on a
// strong registry the first, elsewhere the one that outlasted the retries.
func (h *handler) nameMissing(ctx context.Context, t *target, body []byte, err error) error {
	if !blobsUnknown(err) {
		return err
	}
	rr, rerr := h.backend(ctx, t)
	if rerr != nil {
		return err
	}
	blobs, what := listedBlobs(body)
	seen := make(map[digest.Digest]bool, len(blobs))
	for _, b := range blobs {
		if seen[b.Digest] || b.Digest.Validate() != nil || len(b.URLs) > 0 {
			continue
		}
		seen[b.Digest] = true
		ok, herr := rr.Exists(ctx, b)
		switch {
		case herr != nil:
			return err
		case !ok:
			return &apiError{code: codeManifestBlobUnknown, msg: fmt.Sprintf(what, b.Digest, t.name),
				detail: map[string]string{"digest": b.Digest.String()}, err: err}
		}
	}
	return err
}

// listedBlobs returns what the manifest body lists that the registry must have before it takes
// it, and the message that names one of them missing: the config and the layers of an image
// manifest, OCI or Docker, or the first maxLayers manifests of an index. readManifest has checked
// that body is JSON with at most maxLayers layers; a body whose descriptors do not decode lists
// none.
func listedBlobs(body []byte) ([]ocispec.Descriptor, string) {
	var m struct {
		Config    *ocispec.Descriptor  `json:"config"`
		Layers    []ocispec.Descriptor `json:"layers"`
		Manifests []ocispec.Descriptor `json:"manifests"`
	}
	if json.Unmarshal(body, &m) != nil {
		return nil, ""
	}
	if len(m.Manifests) > 0 {
		return m.Manifests[:min(len(m.Manifests), maxLayers)], "manifest %s was not pushed to %s"
	}
	var blobs []ocispec.Descriptor
	if m.Config != nil {
		blobs = append(blobs, *m.Config)
	}
	return append(blobs, m.Layers...), "blob %s was not uploaded to %s"
}

// retryBlobsLag sends a manifest with push, and again while a registry not classified strong
// answers MANIFEST_BLOB_UNKNOWN, with the backoff of a read-back, for the visibility timeout
// (Options.VisibilityTimeout, or the registry's consistencyBound when longer): a client uploads
// the blobs right before the manifest, and ECR's manifest check may not see one for a moment
// after its 201 (#446). A strong registry's refusal is the answer at once, as is any other error.
func (h *handler) retryBlobsLag(ctx context.Context, t *target, push func(context.Context) error) error {
	err := push(ctx)
	if !blobsUnknown(err) {
		return err
	}
	p, perr := h.registry(t)
	if perr != nil {
		return err
	}
	caps, cerr := p.Capabilities(ctx)
	if cerr != nil || caps.Consistency == provider.ConsistencyStrong {
		return err
	}
	again := false
	_, werr := provider.ReadBack{Timeout: max(h.o.VisibilityTimeout, caps.ConsistencyBound)}.Await(ctx, func(ctx context.Context) (bool, error) {
		if !again { // the first answer is the push above: wait before the next
			again = true
			return false, nil
		}
		err = push(ctx)
		return !blobsUnknown(err), nil
	})
	if werr != nil {
		return werr
	}
	return err
}

// blobsUnknown reports whether err is the registry's MANIFEST_BLOB_UNKNOWN.
func blobsUnknown(err error) bool {
	var rej *provider.RejectedError
	return errors.As(err, &rej) && rej.Code == codeManifestBlobUnknown
}

// readManifest reads the manifest of a PUT, at most limit bytes, and returns its descriptor
// and bytes; refDigest, when not empty, is the digest the reference names, whose algorithm
// digests the body. A manifest with more than maxLayers layers is MANIFEST_INVALID, before
// anything asks the registry.
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
		status, how := brokeOff(err)
		return ocispec.Descriptor{}, nil, &apiError{status: status, code: codeManifestInvalid, msg: "the manifest body " + how, err: err}
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !manifestMediaTypes[mediaType] {
		return ocispec.Descriptor{}, nil, &apiError{status: http.StatusBadRequest, code: codeManifestInvalid,
			msg: fmt.Sprintf("Content-Type %q is not a manifest media type", r.Header.Get("Content-Type"))}
	}
	if err := checkFields(body, mediaType); err != nil {
		return ocispec.Descriptor{}, nil, err
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

// checkFields checks the fields of a manifest body that every repo relies on: JSON, at most
// maxLayers layers, a subject whose digest parses, and a mediaType, when it has one, that is
// the Content-Type mediaType.
func checkFields(body []byte, mediaType string) error {
	var fields struct {
		MediaType string `json:"mediaType"`
		Subject   *struct {
			Digest string `json:"digest"`
		} `json:"subject"`
		Layers json.RawMessage `json:"layers"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return &apiError{status: http.StatusBadRequest, code: codeManifestInvalid, msg: "the manifest is not JSON", err: err}
	}
	// Layers that are not a list are left to the manifest's decoder, or to the backend.
	var layers []json.RawMessage
	if json.Unmarshal(fields.Layers, &layers) == nil && len(layers) > maxLayers {
		return &apiError{status: http.StatusBadRequest, code: codeManifestInvalid,
			msg: fmt.Sprintf("the manifest has %d layers, the limit is %d", len(layers), maxLayers)}
	}
	// The subject's digest goes back in the OCI-Subject header of the answer: a digest only.
	if fields.Subject != nil {
		if _, err := digest.Parse(fields.Subject.Digest); err != nil {
			return &apiError{status: http.StatusBadRequest, code: codeManifestInvalid,
				msg: fmt.Sprintf("the manifest's subject has the digest %q, which is not one", fields.Subject.Digest)}
		}
	}
	if fields.MediaType != "" && fields.MediaType != mediaType {
		return &apiError{status: http.StatusBadRequest, code: codeManifestInvalid,
			msg: fmt.Sprintf("the manifest's mediaType %q is not its Content-Type %q", fields.MediaType, mediaType)}
	}
	return nil
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
	if err := h.refuseJournal(ctx, t, ref); err != nil {
		return err
	}
	del := format.Deletion{Path: t.path, Reference: ref, ClientIP: server.ClientAddr(ctx)}
	if _, err := h.o.Ingest.Delete(ctx, t.repo, del, auth.PrincipalFrom(ctx)); err != nil {
		return deleteError(t, ref, err)
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

// refuseJournal refuses the delete of an entry of an npm or a pypi package's journal, by its
// tag _j<seq> or by a digest the index has of it, 409 DENIED: an entry goes with its package,
// which only the unpublish of an npm package deletes (docs/spec/manifest-profile.md §8).
func (h *handler) refuseJournal(ctx context.Context, t *target, ref string) error {
	if !profile.HasJournal(t.repo.Format) {
		return nil
	}
	_, entry := profile.JournalSeq(ref)
	if !entry && strings.Contains(ref, ":") {
		pkg, err := h.o.Index.GetPackageByNameOCI(ctx, t.repo.ID, t.path)
		if errors.Is(err, index.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		rows, err := h.o.Index.ListJournal(ctx, pkg.ID)
		if err != nil {
			return err
		}
		entry = slices.ContainsFunc(rows, func(e index.JournalEntry) bool { return e.Digest.String() == ref })
	}
	if !entry {
		return nil
	}
	msg := fmt.Sprintf("%s in %s is an entry of the %s journal, which goes with its package", ref, t.name, t.repo.Format)
	if t.repo.Format == "npm" {
		msg += ": npm unpublish deletes it"
	}
	return &apiError{status: http.StatusConflict, code: codeDenied, msg: msg}
}

// deleteError is the error table of a manifest delete:
//
//	provider.ErrUnsupported   405 UNSUPPORTED: the backend does not delete manifests
//	index.ErrNotFound         404 MANIFEST_UNKNOWN
//	index.ErrConflict         409 DENIED: other tags name the manifest of a tag of an oci repo,
//	                          named in the message and in detail.tags (#284)
//	provider.ErrConflict      409 DENIED: the backend refused this one delete
//	anything else             format.StatusOf: 502 for the backend's credentials, 503 for an
//	                          open circuit breaker, else 500, logged
func deleteError(t *target, ref string, err error) error {
	var tagged interface{ OtherTags() []string }
	switch {
	case errors.As(err, &tagged) && errors.Is(err, index.ErrConflict):
		tags := tagged.OtherTags()
		return &apiError{status: http.StatusConflict, code: codeDenied, err: err, detail: map[string][]string{"tags": tags},
			msg: fmt.Sprintf("%s in %s is also tagged %s: a delete by tag would delete those tags too, nothing was deleted; delete by digest to delete the manifest with every tag",
				ref, t.name, strings.Join(tags, ", "))}
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

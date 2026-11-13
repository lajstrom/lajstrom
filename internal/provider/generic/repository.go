package generic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// repository is oras-go's remote.Repository with its errors translated into the sentinels of
// package provider (translate), and the blob upload sessions of provider.BlobUploader
// (uploads.go). Blobs and manifests stream through it: Push and Fetch pass the readers on and
// never buffer a blob. The methods that provider.Repository does not name (Mount,
// FetchReference, Blobs, Manifests) are oras-go's, untranslated.
type repository struct {
	*remote.Repository
	p    *Provider
	name string // the Lajstrom name, without the instance prefix, for error messages
}

// op names an operation on the repository for an error message: "fetch raw/scratch/x@<ref>".
func (r *repository) op(verb, ref string) string {
	if ref == "" {
		return verb + " " + r.name
	}
	return verb + " " + r.name + "@" + ref
}

// Fetch returns a reader of the blob or manifest target, whose descriptor carries its size:
// oras-go checks it against Content-Length. A client's download of a blob by its digest
// alone goes through Provider.OpenBlob.
func (r *repository) Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error) {
	rc, err := r.Repository.Fetch(ctx, target)
	if err != nil {
		return nil, r.p.translate(r.op("fetch", target.Digest.String()), err)
	}
	return rc, nil
}

// Exists reports whether the blob or manifest target exists. A blob is asked through
// Provider.StatBlob where the vendor's HEAD cannot be believed (quirks.blobExistsByGET), so that
// a blob ingest or the facade skips is one it can read back; the vendor is known before the
// first request (readQuirks).
func (r *repository) Exists(ctx context.Context, target ocispec.Descriptor) (bool, error) {
	if !isManifest(target.MediaType) {
		q, err := r.p.readQuirks(ctx)
		if err != nil {
			return false, err
		}
		if q.blobExistsByGET {
			_, err = r.p.statBlob(ctx, r.name, target.Digest, &q)
			if errors.Is(err, provider.ErrNotFound) {
				return false, nil
			}
			return err == nil, err
		}
	}
	ok, err := r.Repository.Exists(ctx, target)
	return ok, r.p.translate(r.op("exists", target.Digest.String()), err)
}

// Push uploads the blob or manifest expected from content; a blob is streamed in one
// monolithic upload, which is done again from the start when it fails transiently and content
// can be rewound (pushRetry), and while a registry not classified strong answers its PUT
// BLOB_UPLOAD_UNKNOWN for the session its POST opened (sendFresh); a manifest PUT is retried
// by the transport. Content that already exists is success, and so is a referrer whose old
// referrers index the registry would not delete (pushed).
func (r *repository) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error {
	var err error
	if isManifest(expected.MediaType) {
		err = r.Repository.Push(ctx, expected, content)
	} else {
		err = r.sendFresh(ctx, time.Now(), content, func() error {
			return pushRetry(ctx, r.p.clients.retries, r.p.log, content, func(c io.Reader) error {
				return r.Repository.Push(ctx, expected, c)
			})
		})
	}
	if errors.Is(err, errdef.ErrAlreadyExists) {
		return nil
	}
	return r.p.translate(r.op("push", expected.Digest.String()), r.pushed(ctx, err))
}

// pushed is err of a manifest push, or nil when the push stored the manifest and the new
// sha256-<hex> referrers index and only the delete of the index it replaced failed: oras-go
// deletes it on a registry without the referrers API, and some registries refuse deletes
// (GHCR; distribution with deletes off). The old index stays, untagged, for the registry's
// garbage collector; it is logged.
func (r *repository) pushed(ctx context.Context, err error) error {
	var re *remote.ReferrersError
	if !errors.As(err, &re) || !re.IsReferrersIndexDelete() {
		return err
	}
	r.p.log.WarnContext(ctx, "generic: the registry refused to delete a replaced referrers index; the old referrers index stays, untagged",
		"repository", r.name, "subject", re.Subject.Digest.String(), "error", err.Error())
	return nil
}

// Delete removes the manifest or blob target. A 405 DENIED refuses this one delete, the way
// Zot keeps a manifest that an image index lists and a blob that a manifest uses
// (docs/registries/zot.md, Delete), and so does ECR's 405 UNSUPPORTED for a manifest an index
// lists (deniedOne): it is provider.ErrConflict and changes nothing else. The first manifest
// delete that the registry refuses otherwise with 405 turns Capabilities.DeleteManifest false.
func (r *repository) Delete(ctx context.Context, target ocispec.Descriptor) error {
	err := r.Repository.Delete(ctx, target)
	op := r.op("delete", target.Digest.String())
	if resp := deniedOne(err); resp != nil {
		return &backendError{op: op, kind: provider.ErrConflict, detail: responseDetail(resp), cause: err}
	}
	if err != nil && isManifest(target.MediaType) && refusesDelete(err) && !r.p.deleteRefused.Swap(true) {
		r.p.log.WarnContext(ctx, "registry refuses manifest deletes; DeleteManifest is now false")
	}
	return r.p.translate(op, err)
}

// Tag points the tag reference at the manifest desc.
func (r *repository) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {
	return r.p.translate(r.op("tag", reference), r.Repository.Tag(ctx, desc, reference))
}

// Resolve returns the descriptor of the manifest that reference, a tag or a digest, names.
func (r *repository) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	desc, err := r.Repository.Resolve(ctx, reference)
	return desc, r.p.translate(r.op("resolve", reference), err)
}

// PushReference uploads the manifest expected and tags it reference in one PUT.
func (r *repository) PushReference(ctx context.Context, expected ocispec.Descriptor, content io.Reader, reference string) error {
	return r.p.translate(r.op("push", reference), r.pushed(ctx, r.Repository.PushReference(ctx, expected, content, reference)))
}

// Tags calls fn with each page of the repository's tags, starting after last. A registry
// whose tag list is in an order of its own (unorderedTags: GHCR, ECR) is read from the start,
// every page by its Link, and fn gets the tags after last, sorted, at once.
func (r *repository) Tags(ctx context.Context, last string, fn func(tags []string) error) error {
	op := r.op("tags", "")
	if !r.p.unorderedTags {
		return r.p.translate(op, r.Repository.Tags(ctx, last, fn))
	}
	var all []string
	if err := r.Repository.Tags(ctx, "", func(page []string) error {
		all = append(all, page...)
		return nil
	}); err != nil {
		return r.p.translate(op, err)
	}
	slices.Sort(all)
	if last != "" {
		i, found := slices.BinarySearch(all, last)
		if found {
			i++
		}
		all = all[i:]
	}
	return fn(all)
}

// Referrers calls fn with the manifests whose subject is desc, of artifactType when it is not
// empty, from the referrers API or the sha256-<digest> tag schema.
func (r *repository) Referrers(ctx context.Context, desc ocispec.Descriptor, artifactType string, fn func(referrers []ocispec.Descriptor) error) error {
	return r.p.translate(r.op("referrers", desc.Digest.String()), r.Repository.Referrers(ctx, desc, artifactType, fn))
}

// manifestMediaTypes are the media types oras-go sends to the manifests endpoint by default.
var manifestMediaTypes = map[string]bool{
	ocispec.MediaTypeImageManifest:                              true,
	ocispec.MediaTypeImageIndex:                                 true,
	"application/vnd.oci.artifact.manifest.v1+json":             true,
	"application/vnd.docker.distribution.manifest.v2+json":      true,
	"application/vnd.docker.distribution.manifest.list.v2+json": true,
}

// isManifest reports whether mediaType is a manifest's.
func isManifest(mediaType string) bool { return manifestMediaTypes[mediaType] }

// refusesDelete reports whether err is a registry's answer that it does not delete:
// UNSUPPORTED, or a 405 that deniedOne does not claim.
func refusesDelete(err error) bool {
	var resp *errcode.ErrorResponse
	return errors.As(err, &resp) && deniedOne(err) == nil &&
		(resp.StatusCode == http.StatusMethodNotAllowed || hasCode(resp, errcode.ErrorCodeUnsupported))
}

// deniedOne returns the response of err when it refuses one delete rather than deletes: 405
// with DENIED and without UNSUPPORTED, or ECR's 405 for a manifest an index lists
// (listedByIndex). Otherwise it is nil.
func deniedOne(err error) *errcode.ErrorResponse {
	var resp *errcode.ErrorResponse
	if !errors.As(err, &resp) || resp.StatusCode != http.StatusMethodNotAllowed {
		return nil
	}
	if hasCode(resp, errcode.ErrorCodeDenied) && !hasCode(resp, errcode.ErrorCodeUnsupported) || listedByIndex(resp) {
		return resp
	}
	return nil
}

// ecrListedByIndex starts the message of ECR's 405 UNSUPPORTED to the delete of a manifest that an
// image index lists: "Requested image referenced by manifest list: [sha256:…]" (measured by #19,
// docs/registries/ecr.md, Delete). Its other deletes go through.
const ecrListedByIndex = "Requested image referenced by manifest list"

// listedByIndex reports whether resp is ECR's refusal to delete a manifest an index lists.
func listedByIndex(resp *errcode.ErrorResponse) bool {
	for _, e := range resp.Errors {
		if e.Code == errcode.ErrorCodeUnsupported && strings.HasPrefix(e.Message, ecrListedByIndex) {
			return true
		}
	}
	return false
}

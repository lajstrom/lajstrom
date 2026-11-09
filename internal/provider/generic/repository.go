package generic

import (
	"context"
	"errors"
	"io"
	"net/http"

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
		return nil, translate(r.op("fetch", target.Digest.String()), err)
	}
	return rc, nil
}

// Exists reports whether the blob or manifest target exists.
func (r *repository) Exists(ctx context.Context, target ocispec.Descriptor) (bool, error) {
	ok, err := r.Repository.Exists(ctx, target)
	return ok, translate(r.op("exists", target.Digest.String()), err)
}

// Push uploads the blob or manifest expected from content; a blob is streamed in one
// monolithic upload. Content that already exists is success.
func (r *repository) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error {
	err := r.Repository.Push(ctx, expected, content)
	if errors.Is(err, errdef.ErrAlreadyExists) {
		return nil
	}
	return translate(r.op("push", expected.Digest.String()), err)
}

// Delete removes the manifest or blob target. A 405 DENIED refuses this one delete, the way
// Zot keeps a manifest that an image index lists and a blob that a manifest uses
// (docs/registries/zot.md, Delete): it is provider.ErrConflict and changes nothing else. The
// first manifest delete that the registry refuses otherwise with 405 turns
// Capabilities.DeleteManifest false.
func (r *repository) Delete(ctx context.Context, target ocispec.Descriptor) error {
	err := r.Repository.Delete(ctx, target)
	op := r.op("delete", target.Digest.String())
	if resp := deniedOne(err); resp != nil {
		return &backendError{op: op, kind: provider.ErrConflict, detail: responseDetail(resp), cause: err}
	}
	if err != nil && isManifest(target.MediaType) && refusesDelete(err) && !r.p.deleteRefused.Swap(true) {
		r.p.log.WarnContext(ctx, "registry refuses manifest deletes; DeleteManifest is now false")
	}
	return translate(op, err)
}

// Tag points the tag reference at the manifest desc.
func (r *repository) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {
	return translate(r.op("tag", reference), r.Repository.Tag(ctx, desc, reference))
}

// Resolve returns the descriptor of the manifest that reference, a tag or a digest, names.
func (r *repository) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	desc, err := r.Repository.Resolve(ctx, reference)
	return desc, translate(r.op("resolve", reference), err)
}

// PushReference uploads the manifest expected and tags it reference in one PUT.
func (r *repository) PushReference(ctx context.Context, expected ocispec.Descriptor, content io.Reader, reference string) error {
	return translate(r.op("push", reference), r.Repository.PushReference(ctx, expected, content, reference))
}

// Tags calls fn with each page of the repository's tags, starting after last.
func (r *repository) Tags(ctx context.Context, last string, fn func(tags []string) error) error {
	return translate(r.op("tags", ""), r.Repository.Tags(ctx, last, fn))
}

// Referrers calls fn with the manifests whose subject is desc, of artifactType when it is not
// empty, from the referrers API or the sha256-<digest> tag schema.
func (r *repository) Referrers(ctx context.Context, desc ocispec.Descriptor, artifactType string, fn func(referrers []ocispec.Descriptor) error) error {
	return translate(r.op("referrers", desc.Digest.String()), r.Repository.Referrers(ctx, desc, artifactType, fn))
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
// with DENIED and without UNSUPPORTED. Otherwise it is nil.
func deniedOne(err error) *errcode.ErrorResponse {
	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) && resp.StatusCode == http.StatusMethodNotAllowed &&
		hasCode(resp, errcode.ErrorCodeDenied) && !hasCode(resp, errcode.ErrorCodeUnsupported) {
		return resp
	}
	return nil
}

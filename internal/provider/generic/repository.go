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

// Fetch returns a reader of the blob or manifest target. A blob whose descriptor has a
// negative size, as the facade has it for a client's GET by digest, is fetched by its digest
// alone (one GET; oras-go checks the size of every other descriptor against Content-Length),
// and the reader is then an io.ReadSeekCloser that goes forwards only: seeking to its end
// gives the size without reading.
func (r *repository) Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error) {
	op := r.op("fetch", target.Digest.String())
	if target.Size < 0 && !isManifest(target.MediaType) {
		desc, rc, err := r.Repository.Blobs().FetchReference(ctx, target.Digest.String())
		if err != nil {
			return nil, translate(op, err)
		}
		return &sizedReader{rc: rc, size: desc.Size}, nil
	}
	rc, err := r.Repository.Fetch(ctx, target)
	if err != nil {
		return nil, translate(op, err)
	}
	return rc, nil
}

// sizedReader is a blob's body with its size, as an io.ReadSeekCloser over a stream that goes
// forwards only: a seek records the offset, the next Read discards the bytes up to it, and a
// seek before what was read fails. Seeking to the end and back to the start before the first
// Read, which is how http.ServeContent and the facade learn the size, reads nothing.
type sizedReader struct {
	rc   io.ReadCloser
	size int64
	pos  int64 // bytes taken from rc
	off  int64 // where the next Read starts; at least pos
}

func (s *sizedReader) Seek(offset int64, whence int) (int64, error) {
	abs := offset
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		abs += s.off
	case io.SeekEnd:
		abs += s.size
	default:
		return 0, errors.New("generic: seek: invalid whence")
	}
	if abs < s.pos {
		return 0, errors.New("generic: seek: the blob stream cannot go backwards")
	}
	s.off = abs
	return abs, nil
}

func (s *sizedReader) Read(p []byte) (int, error) {
	if s.off > s.pos {
		n, err := io.CopyN(io.Discard, s.rc, s.off-s.pos)
		s.pos += n
		if err != nil {
			return 0, err
		}
	}
	n, err := s.rc.Read(p)
	s.pos += int64(n)
	s.off = s.pos
	return n, err
}

func (s *sizedReader) Close() error { return s.rc.Close() }

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

// Delete removes the manifest or blob target. The first manifest delete that the registry
// refuses with 405 turns Capabilities.DeleteManifest false.
func (r *repository) Delete(ctx context.Context, target ocispec.Descriptor) error {
	err := r.Repository.Delete(ctx, target)
	if err != nil && isManifest(target.MediaType) && refusesDelete(err) && !r.p.deleteRefused.Swap(true) {
		r.p.log.WarnContext(ctx, "registry refuses manifest deletes; DeleteManifest is now false")
	}
	return translate(r.op("delete", target.Digest.String()), err)
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

// refusesDelete reports whether err is a registry's answer that it does not delete: 405 or
// UNSUPPORTED.
func refusesDelete(err error) bool {
	var resp *errcode.ErrorResponse
	return errors.As(err, &resp) &&
		(resp.StatusCode == http.StatusMethodNotAllowed || hasCode(resp, errcode.ErrorCodeUnsupported))
}

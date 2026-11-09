package format

import (
	"context"
	"encoding/json"
	"io"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Entry is what a plugin reads out of a manifest: the data for one row in versions and its rows
// in files (docs/spec/index-schema.md, Derived). Ingest adds the repo, tag and digest.
type Entry struct {
	// Name is the package name in the format's own spelling (annotation lajstrom.name).
	Name string
	// Version is the version exactly as the format spells it (annotation lajstrom.version).
	Version string
	// Variant is the platform discriminator (annotation lajstrom.variant); empty when none.
	Variant string
	// Attrs holds the format-specific annotations, lajstrom.<format>.*.
	Attrs map[string]string
	// Files has one element per file layer, in manifest order. The metadata layer is not a file.
	Files []FileEntry
	// Metadata is the content of the metadata layer; nil when the manifest has none (raw).
	Metadata json.RawMessage
}

// FileEntry is one file layer of a manifest (docs/spec/manifest-profile.md §2).
type FileEntry struct {
	// Name is the exact file name from org.opencontainers.image.title.
	Name string
	// MediaType is the layer media type, application/vnd.lajstrom.<format>.<role>.v1[+suffix].
	MediaType string
	// Digest is the layer digest.
	Digest digest.Digest
	// Size is the layer size in bytes.
	Size int64
	// Attrs holds the layer annotations.
	Attrs map[string]string
}

// Upload is what a protocol frontend received in one publish request: the files and the
// protocol's parameters. The principal is passed separately to Publisher.Upload.
type Upload struct {
	// Files are the uploaded files, in the order the client sent them.
	Files []UploadedFile
	// Params are protocol parameters: twine form fields, apt suite and component, the raw path.
	Params map[string]string
}

// UploadedFile is one file of an Upload. It is re-openable: Build opens it to parse and digest
// it, and ingest opens it again to push the blob. The HTTP layer makes the request body
// re-openable (a request spool, never artifact storage) and removes it when the request ends.
type UploadedFile struct {
	// Name is the file name as the client sent it.
	Name string
	// Size is the size in bytes, or -1 when unknown.
	Size int64
	// ContentType is the Content-Type the client sent, if any.
	ContentType string
	// Open returns a new reader positioned at the start of the file.
	Open func() (io.ReadCloser, error)
}

// Built is the result of Plugin.Build: a complete manifest and the blobs ingest has to push
// before the manifest.
type Built struct {
	// Manifest is the manifest to store; it satisfies docs/spec/manifest-profile.md §1.
	Manifest ocispec.Manifest
	// Blobs are the blobs the manifest references: config, file layers and metadata layer.
	// Ingest skips those the backend already has.
	Blobs []BlobSource
	// Entry is the index entry for the manifest, as Validate would return it.
	Entry Entry
	// Tag is the tag computed from version and variant (docs/spec/manifest-profile.md §4.2).
	Tag string
	// RepoPath is <format>/<repo>/<package path> without the instance prefix (§4.1).
	RepoPath string
}

// BlobSource is one blob to push together with a way to read its bytes.
type BlobSource struct {
	// Descriptor carries media type, digest and size of the blob.
	Descriptor ocispec.Descriptor
	// Open returns a new reader positioned at the start of the blob.
	Open func(ctx context.Context) (io.ReadCloser, error)
}

// BlobFetcher reads a blob of the manifest being validated, from the OCI repository the
// manifest is pushed to. Validate must close the returned reader. Read streams the whole blob,
// checked against its digest. The reader may also be an io.ReaderAt over the blob, of the size of
// its descriptor, that reads parts with range requests and never the whole (ingest's is, #161): a
// plugin that needs random access, to the directory at the end of a zip, type-asserts it and
// calls ReadAt and not Read, so that a facade push or a reindex reads kilobytes of a large file.
// The bytes of ReadAt are not checked against the digest, which only the whole blob can be.
type BlobFetcher func(ctx context.Context, d digest.Digest) (io.ReadCloser, error)

// IndexScope names the part of a repo an index document covers, in the format's own terms:
// "jammy/main/amd64" (apt suite/component/arch), "simple/requests" (pypi project),
// "com.example/lib-core" (maven groupId/artifactId). It is the scope column of index_cache.
type IndexScope string

// Document is one rendered index document, for example an apt Packages file or a pypi simple
// page. Callers cache it in index_cache.
type Document struct {
	// Kind is the index_cache kind: Packages, Release, InRelease, simple, maven-metadata.
	Kind string
	// Path is the URL path of the document relative to /<format>/<repo>/.
	Path string
	// ContentType is the Content-Type to serve the document with.
	ContentType string
	// Content is the document body.
	Content []byte
}

// Published is what Publisher.Upload reports back to the frontend.
type Published struct {
	// Digest is the manifest digest.
	Digest digest.Digest
	// Tag is the tag of the stored manifest.
	Tag string
	// RepoPath is <format>/<repo>/<package path> without the instance prefix.
	RepoPath string
	// Entry is the index entry of the stored manifest.
	Entry Entry
}

package provider

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
)

// Provider is one backend OCI registry. Only implementations of this interface talk to the
// registry; everything else reaches blobs and manifests through it.
// See docs/spec/layout.md §2; per-backend behaviour is recorded in docs/registries/.
//
// Repository names passed to and returned by a Provider are Lajstrom paths,
// <format>/<repo>/<package path>, e.g. "apt/ubuntu-internal/nginx"
// (docs/spec/manifest-profile.md §4.1). The provider prepends the instance prefix of its
// registry configuration (registries[].prefix) and strips it from listings; nothing outside
// internal/provider sees the prefix (docs/spec/http-routing.md §2).
type Provider interface {
	// Name is the registry's name in the configuration; repos.registry refers to it.
	Name() string

	// Capabilities reports what the backend supports. Implementations may probe the registry
	// once and cache the answer.
	Capabilities(ctx context.Context) (Capabilities, error)

	// Repository returns the data path for one OCI repository. It does not create the
	// repository; see EnsureRepository.
	Repository(ctx context.Context, name string) (Repository, error)

	// EnsureRepository creates the repository where the backend needs an explicit create
	// (ECR). It is a no-op where create-on-push works and when the repository exists.
	EnsureRepository(ctx context.Context, name string) error

	// ListRepositories returns the names of the repositories that start with prefix. Without a
	// catalog it returns an error wrapping ErrUnsupported and callers fall back to the
	// oci_repos table (issue #37).
	ListRepositories(ctx context.Context, prefix string) ([]string, error)

	// DeleteRepository removes the repository and its content, or returns an error wrapping
	// ErrUnsupported where the backend cannot do that.
	DeleteRepository(ctx context.Context, name string) error

	// OpenBlob opens the blob d of the repository name for a download, in one request to the
	// backend: a GET with rangeHeader (the client's Range, "" for none) passed through. The
	// answer is either a redirect the client is sent to (RedirectURL set, Body nil: a
	// cross-origin Location the backend gave, a presigned URL) or an open body with the
	// headers the caller forwards; the caller closes the body. A redirect to the registry's own
	// origin is followed here, not returned. With redirect false, or Capabilities.BlobRedirect
	// false, every redirect is followed and the body streamed: the redirect decision is taken
	// per blob (policy.blobRedirect of the repo, docs/adr/0009). rangeHeader should name one
	// range: a backend answers several with a multipart/byteranges body, whose Content-Type a
	// Blob does not carry. provider.ErrNotFound when the backend has no such blob; a
	// Docker-Content-Digest other than d is an error.
	OpenBlob(ctx context.Context, name string, d digest.Digest, rangeHeader string, redirect bool) (Blob, error)

	// StatBlob answers HEAD: the size and digest of the blob d, no body, no redirect (the
	// provider follows them). provider.ErrNotFound when the backend has no such blob.
	StatBlob(ctx context.Context, name string, d digest.Digest) (Blob, error)

	// StatManifest answers HEAD of the manifest d of the repository name by its digest: its
	// media type, size and digest, no body. On a 404 it asks GET as well, whose answer carries
	// the error code a HEAD cannot: 404 with MANIFEST_UNKNOWN wraps ErrManifestUnknown and
	// ErrNotFound, any other 404 (NAME_UNKNOWN, no code) ErrNotFound alone, and a GET that
	// finds the manifest after all answers as the HEAD would have.
	StatManifest(ctx context.Context, name string, d digest.Digest) (ocispec.Descriptor, error)
}

// Blob is a blob opened for a download (OpenBlob), or its headers (StatBlob).
type Blob struct {
	// RedirectURL is where the client is sent (307); Body is nil then. The URL is the
	// backend's Location as it gave it, absolute: a presigned URL carries its own
	// credentials, and the client never gets Lajstrom's.
	RedirectURL string
	// Body is the bytes as the backend sent them; nil for a redirect, a 416 and a StatBlob.
	Body io.ReadCloser
	// Status is 200, 206 (a range) or 416 (a range the blob cannot satisfy), as the backend
	// answered; 307 with RedirectURL.
	Status int
	// Size is the Content-Length of Body (the range's length for a 206; for a StatBlob, the
	// blob's size); -1 when unknown.
	Size int64
	// ContentRange is the backend's Content-Range on a 206 or a 416, "" otherwise.
	ContentRange string
	// Digest is the Docker-Content-Digest of the backend, the digest asked for when it sent
	// none.
	Digest digest.Digest
	// AcceptRanges is true when the backend said Accept-Ranges: bytes.
	AcceptRanges bool
}

// Repository is the data path to one OCI repository. The generic provider returns oras-go's
// remote.Repository, wrapped. Fetch, Exists, Push and Delete work on blobs and manifests
// alike, selected by the descriptor's media type.
type Repository interface {
	content.Storage          // Fetch, Exists, Push
	content.Deleter          // Delete; manifests only where Capabilities.DeleteManifest
	content.TagResolver      // Tag, Resolve
	registry.ReferencePusher // PushReference: manifest and tag in one PUT (ingest uses this, not Push+Tag)
	registry.TagLister       // Tags
	registry.ReferrerLister  // Referrers; falls back to the tag schema without Capabilities.Referrers
	BlobUploader             // the backend's blob upload session, for the facade (#24)
	BlobStreamer             // a blob Lajstrom streams in itself, for the frontends (ADR 0014)
}

// BlobStreamer opens the blob of a frontend's upload, which goes to the registry while ingest
// hashes and inspects it, and is kept only once every check passed (ADR 0014,
// docs/adr/0014-uploads-stream-into-the-registry.md).
type BlobStreamer interface {
	// OpenBlobWriter returns a writer of one new blob of the repository. It may defer the
	// session's POST to the first chunk, so that a blob that is cancelled before then costs the
	// backend nothing. The writer's requests use ctx.
	OpenBlobWriter(ctx context.Context) (BlobWriter, error)
}

// BlobWriter is one blob on its way into the backend through an upload session. Write sends
// the bytes in chunks of the provider's size, each a PATCH with its Content-Range, and holds at
// most one chunk in memory; nothing is stored until Commit closes the session with the digest
// the caller computed (PUT ?digest=). Cancel drops the session and what it holds (DELETE).
// Either ends the writer: a Write after them fails, Cancel after Commit and a second Cancel do
// nothing. A Write or Commit that failed leaves the session to Cancel. Not safe for concurrent
// use.
type BlobWriter interface {
	io.Writer
	// Size is the number of bytes written.
	Size() int64
	// Commit sends what is buffered and closes the session with d, the digest of every byte
	// written. A digest the backend refuses wraps oras-go's content.ErrMismatchedDigest.
	Commit(ctx context.Context, d digest.Digest) (ocispec.Descriptor, error)
	// Cancel ends the session and drops its bytes. A backend that cannot cancel
	// (ErrUnsupported) leaves the session to expire; it stores nothing either way.
	Cancel(ctx context.Context) error
}

// BlobUploader drives the backend's blob upload session (distribution spec: POST, PATCH,
// PUT, GET, DELETE on blobs/uploads). An Upload is opaque to callers and serialisable: a
// replica that did not start it continues it, so the facade keeps it in the index between
// requests and never in memory (docs/spec/layout.md §1, conventions).
//
// Errors wrap the sentinels of this package as the other methods do: a session the backend
// no longer knows is ErrNotFound, and any other 4xx that no sentinel names, or a 503, is a
// *RejectedError (of a 429 or 503 that is ErrUnavailable too). FinishBlobUpload reports a digest the backend refuses with an error that
// wraps oras-go's content.ErrMismatchedDigest. No error message carries the backend's URL or
// the instance prefix.
type BlobUploader interface {
	// StartBlobUpload opens a session: POST …/blobs/uploads/.
	StartBlobUpload(ctx context.Context) (Upload, error)
	// MountBlob asks the backend to mount the blob d from the repository from (a Lajstrom
	// name; "" lets the backend look for it anywhere): POST …/blobs/uploads/?mount=&from=.
	// mounted is false when the backend opened a session instead, which u is.
	MountBlob(ctx context.Context, d digest.Digest, from string) (mounted bool, u Upload, err error)
	// AppendBlobChunk streams r, size bytes (-1 when unknown), to the end of the session:
	// PATCH. It returns the session with the backend's new location and offset.
	AppendBlobChunk(ctx context.Context, u Upload, r io.Reader, size int64) (Upload, error)
	// FinishBlobUpload closes the session with the digest of the whole blob, after the last
	// chunk r of size bytes (-1 when unknown; size 0 and a nil r for none): PUT …?digest=.
	FinishBlobUpload(ctx context.Context, u Upload, d digest.Digest, r io.Reader, size int64) (ocispec.Descriptor, error)
	// BlobUploadStatus returns the session with the backend's offset: GET.
	BlobUploadStatus(ctx context.Context, u Upload) (Upload, error)
	// CancelBlobUpload ends the session and drops what it holds: DELETE.
	CancelBlobUpload(ctx context.Context, u Upload) error
}

// Upload is one backend upload session: its location (absolute URL or path, as the backend
// answered), its backend id, and the bytes the backend holds so far.
type Upload struct {
	Location string
	ID       string
	Offset   int64
	// ChunkMinLength is the OCI-Chunk-Min-Length of the backend's last answer for the
	// session, the smallest chunk but the last that it takes, in bytes; 0 when it sent none.
	// The facade passes it on to the client.
	ChunkMinLength int64
}

// Pinger is a Provider that can say whether its registry answers now, with Lajstrom's
// credentials: GET /v2/ for a registry of the distribution API. serve pings every registry that
// has it in the background, and /-/readyz is ready while each answered within the last minute
// (docs/spec/http-routing.md §5). A Provider without it counts as answering.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Set is the configured backend registries by name (registries[].name in the configuration).
// index.Repo.Registry selects one; the server builds the Set at startup.
type Set interface {
	// Provider returns the registry configured under name.
	Provider(name string) (Provider, bool)
}

// Map is a Set backed by a map from registry name to Provider.
type Map map[string]Provider

// Provider implements Set.
func (m Map) Provider(name string) (Provider, bool) {
	p, ok := m[name]
	return p, ok
}

// Capabilities describes what a backend registry supports. The facade, ingest and the provider
// fallbacks branch on it instead of on the registry's product name.
type Capabilities struct {
	// CreateOnPush is true when pushing to a new repository creates it (false for ECR).
	CreateOnPush bool
	// Referrers is true when the backend serves the OCI referrers API; otherwise the
	// sha256-<digest> tag schema is used (docs/spec/manifest-profile.md §6).
	Referrers bool
	// Catalog is true when the backend lists repositories, so ListRepositories works.
	Catalog bool
	// DeleteManifest is true when manifests can be deleted.
	DeleteManifest bool
	// ImmutableTags is true when the backend itself refuses to re-point a tag.
	ImmutableTags bool
	// BlobRedirect is true when OpenBlob may return a redirect URL. It is the operator's
	// switch (registries[].capabilities.blobRedirect): false means OpenBlob follows every
	// redirect and streams, and a client never gets a URL of the backend.
	BlobRedirect bool
	// MaxBlobSize is the largest blob the backend accepts, in bytes; 0 means unknown.
	MaxBlobSize int64
	// Consistency is when the backend's reads see a write it acknowledged: strong, eventual,
	// or unknown (ConsistencyUnknown) when neither the vendor table nor the configuration
	// says. Ingest reads every publish back whatever it is; it decides whether a 404 of a blob
	// a client says it uploaded is worth reading again.
	Consistency Consistency
	// ConsistencyBound is the longest lag known of an eventual backend, from the 2xx of a write
	// until every read sees it (registries[].capabilities.consistencyBound, which
	// `lajstrom registry probe` measures); 0 when none is known.
	ConsistencyBound time.Duration
	// MaxNameDepth is the most components the backend takes in a repository name below the
	// instance prefix, a Lajstrom name <format>/<repo>/<package path> counted whole; 0 means no
	// limit. Docker Hub takes one below its namespace; GitLab behind its token service four, a
	// project per Lajstrom repo and two components below it (docs/registries/quirks.md). The
	// grammar of a name needs no capability: every name Lajstrom writes keeps to the one every
	// registry tested takes (ADR 0017). CheckName applies it.
	MaxNameDepth int
}

// CheckName reports whether the backend of c takes the repository name, a Lajstrom name as
// Repository gets it: nil, or an error that says which limit of c the name breaks
// (MaxNameDepth), for the caller to answer with a 4xx before anything is pushed.
func (c Capabilities) CheckName(name string) error {
	if depth := strings.Count(name, "/") + 1; c.MaxNameDepth > 0 && depth > c.MaxNameDepth {
		return fmt.Errorf("the OCI repository %s has %d components, and the registry takes at most %d below its prefix "+
			"(capabilities.maxNameDepth)", name, depth, c.MaxNameDepth)
	}
	return nil
}

// Consistency is a backend's read-after-write model (docs/registries/README.md, Consistency).
type Consistency string

// The models. ConsistencyUnknown is the zero value.
const (
	// ConsistencyUnknown: nobody classified the backend.
	ConsistencyUnknown Consistency = ""
	// ConsistencyStrong: every read after the 2xx of a write sees it, by tag and by digest.
	ConsistencyStrong Consistency = "strong"
	// ConsistencyEventual: the backend may answer 404 for a manifest, a blob or a tag it
	// acknowledged a moment before, or list it late (ECR, GHCR).
	ConsistencyEventual Consistency = "eventual"
)

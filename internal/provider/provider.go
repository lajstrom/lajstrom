package provider

import (
	"context"

	"github.com/opencontainers/go-digest"
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

	// BlobRedirectURL returns a URL a client may be redirected to (307) to download a blob of
	// the repository. The boolean is false when the backend cannot give one for this blob;
	// callers then stream the blob through Repository.Fetch.
	BlobRedirectURL(ctx context.Context, name string, d digest.Digest) (string, bool, error)
}

// Repository is the data path to one OCI repository. The generic provider returns oras-go's
// remote.Repository, possibly wrapped. Fetch, Exists, Push and Delete work on blobs and
// manifests alike, selected by the descriptor's media type.
type Repository interface {
	content.Storage          // Fetch, Exists, Push
	content.Deleter          // Delete; manifests only where Capabilities.DeleteManifest
	content.TagResolver      // Tag, Resolve
	registry.ReferencePusher // PushReference: manifest and tag in one PUT (ingest uses this, not Push+Tag)
	registry.TagLister       // Tags
	registry.ReferrerLister  // Referrers; falls back to the tag schema without Capabilities.Referrers
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
	// BlobRedirect is true when BlobRedirectURL can return URLs.
	BlobRedirect bool
	// MaxBlobSize is the largest blob the backend accepts, in bytes; 0 means unknown.
	MaxBlobSize int64
}

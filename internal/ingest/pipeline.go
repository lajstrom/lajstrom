package ingest

import (
	"context"
	"log/slog"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Pipeline is the single write path: every manifest Lajstrom stores goes through it, whether
// it comes from the OCI facade (Ingest) or from a protocol frontend (Upload).
// See docs/spec/layout.md §2 and docs/architecture.md §3 (flow A).
type Pipeline struct {
	providers provider.Set
	index     index.Store
	plugins   func(name string) (format.Plugin, bool)
	audit     audit.Writer
	now       func() time.Time
	log       *slog.Logger
}

// Options configures a Pipeline.
type Options struct {
	// Providers resolves repo.Registry to the backend registry.
	Providers provider.Set
	// Index is the index database.
	Index index.Store
	// Plugins looks up the plugin of a repo's format; nil means format.Lookup.
	Plugins func(name string) (format.Plugin, bool)
	// Audit records every successful publish.
	Audit audit.Writer
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
}

// New returns a Pipeline with the defaults of Options applied.
func New(o Options) *Pipeline {
	p := &Pipeline{
		providers: o.Providers,
		index:     o.Index,
		plugins:   o.Plugins,
		audit:     o.Audit,
		now:       o.Now,
		log:       o.Log,
	}
	if p.plugins == nil {
		p.plugins = format.Lookup
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	return p
}

var _ format.Publisher = (*Pipeline)(nil)

// Result describes a stored manifest.
type Result struct {
	// Digest is the manifest digest.
	Digest digest.Digest
	// Tag is the tag the manifest is stored under.
	Tag string
	// RepoPath is <format>/<repo>/<package path> without the instance prefix.
	RepoPath string
	// Entry is the index entry the plugin returned.
	Entry format.Entry
	// Existed is true when the tag already pointed at this digest: the call was an idempotent
	// repeat and nothing was written.
	Existed bool
}

// Ingest stores a manifest that already exists as bytes; the facade's PUT manifest hook calls
// it. pkgPath and tag are where the client pushed: the package path below <format>/<repo>/
// and the tag, both taken from the /v2 URL. raw is the manifest exactly as received and
// defines the digest; m is its parsed form. The steps (docs/spec/manifest-profile.md §5):
//
//  1. check the profile rules that hold for every format: artifactType selects the plugin of
//     repo's format, config is the empty descriptor, the required annotations are present and
//     agree with repo, the package path computed from the annotations (§4.1) equals pkgPath
//     and the tag computed from them (§4.2) equals tag;
//  2. run the plugin's Validate with a BlobFetcher backed by the provider;
//  3. check that every layer exists in the backend;
//  4. check the tag in the index and in the backend: the same digest returns Existed, another
//     digest returns ErrConflict;
//  5. PUT the manifest with its tag in one request (provider.Repository.PushReference);
//  6. upsert package, version and files in one index transaction; only after the registry
//     accepted the manifest, and when this fails the manifest stays for reindex to pick up;
//  7. invalidate the index_cache scopes the change affects and write the audit event.
//
// Errors wrap ErrInvalid (steps 1 to 3), ErrConflict (step 4), or the provider's and the
// index's sentinels.
func (p *Pipeline) Ingest(ctx context.Context, repo index.Repo, pkgPath, tag string, m ocispec.Manifest, raw []byte, principal auth.Principal) (*Result, error) {
	_, _, _, _, _, _, _ = ctx, repo, pkgPath, tag, m, raw, principal // the body comes with issue #12
	return nil, ErrNotImplemented
}

// Upload is the frontend path: the plugin of repo's format builds the manifest from up
// (Plugin.Build), the pipeline pushes the blobs the backend does not have yet, streaming each
// from its source, and then stores the manifest as Ingest does. It implements format.Publisher.
func (p *Pipeline) Upload(ctx context.Context, repo index.Repo, up format.Upload, principal auth.Principal) (*format.Published, error) {
	_, _, _, _ = ctx, repo, up, principal // the body comes with issue #12
	return nil, ErrNotImplemented
}

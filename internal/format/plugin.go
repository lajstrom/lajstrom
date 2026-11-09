package format

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Plugin is one package format. It maps a protocol onto the manifest profile and back.
// Implementations are stateless; their dependencies come in through Deps.
// See docs/spec/layout.md §2 and docs/spec/manifest-profile.md.
type Plugin interface {
	// Name is the first URL segment and the value of the lajstrom.format annotation: "raw",
	// "pypi", "apt", "maven". It is lowercase ASCII letters and digits with single hyphens
	// between them, ^[a-z0-9]+(-[a-z0-9]+)*$, and not one of Reserved
	// (docs/spec/http-routing.md §1); CheckName tests both.
	Name() string

	// ArtifactType is the manifest artifactType, application/vnd.lajstrom.<type>.v1; the apt
	// plugin uses type deb (docs/spec/manifest-profile.md §1).
	ArtifactType() string

	// EscapeName maps a package name in the format's spelling to the package path below
	// <format>/<repo>/ (docs/spec/manifest-profile.md §4.1). Names the format does not allow
	// give an error wrapping ErrInvalid.
	EscapeName(name string) (string, error)

	// Validate inspects a manifest that is about to be stored (oras push through the facade, or
	// the output of Build), reads the blobs it needs through fetch, and returns the index entry.
	// It must detect disagreement between the bytes, the metadata layer and the annotations
	// and report it as an error wrapping ErrInvalid (docs/spec/manifest-profile.md §5).
	// Ingest runs the profile checks that are the same for every format before Validate.
	Validate(ctx context.Context, repo index.Repo, m ocispec.Manifest, fetch BlobFetcher) (*Entry, error)

	// Build assembles a manifest and the blobs to push from files received by the protocol
	// frontend (twine upload, curl -T, a maven deploy session). It reads the files to parse and
	// digest them and pushes nothing; ingest pushes Built.Blobs and then the manifest.
	Build(ctx context.Context, repo index.Repo, in Upload) (*Built, error)

	// Routes mounts the protocol frontend on r, below /<format>/. Handlers read through
	// deps.Index and publish only through deps.Ingest.
	Routes(r Router, deps Deps)

	// RenderIndex produces the protocol's index documents for one scope of a repo from the
	// index database: apt Packages and Release, a pypi simple page, maven-metadata.xml.
	RenderIndex(ctx context.Context, repo index.Repo, scope IndexScope, st index.Reader) ([]Document, error)
}

// Router is what a plugin mounts its handlers on. Patterns are net/http ServeMux patterns
// relative to /<format>/, for example "GET /{repo}/simple/{project}/"; the implementation,
// NewRouter, prefixes them with the plugin name and resolves {repo}.
type Router interface {
	// Handle registers h for pattern.
	Handle(pattern string, h http.Handler)
	// HandleFunc registers h for pattern.
	HandleFunc(pattern string, h http.HandlerFunc)
}

// Publisher is the write path as plugins see it. *ingest.Pipeline implements it; the interface
// lives here so that format does not import ingest.
type Publisher interface {
	// Upload builds the manifest with the plugin of repo's format, pushes the blobs and the
	// manifest, and indexes the result. See ingest.Pipeline.Upload.
	Upload(ctx context.Context, repo index.Repo, up Upload, principal auth.Principal) (*Published, error)
}

// Sessions are the publication sessions of a format whose upload is several requests: maven PUTs
// each file of a version, then maven-metadata.xml, which commits them (docs/architecture.md, flow
// B). Each file goes to the registry when it arrives; the session in the index records it, and
// Commit publishes the files as one version through Publisher.Upload. A session expires a while
// after its last file (30 minutes by default), and its files are then dropped: a part of a version
// is never published (docs/spec/http-routing.md §3, maven). One principal holds a session; a
// key, the package and version, has one open session in a repo. *ingest.Pipeline implements it;
// the interface lives here so that format does not import ingest.
type Sessions interface {
	// Open returns the open session of the package name at version in repo, opened by
	// principal, and opens one when there is none. An open session of another principal is
	// index.ErrConflict; a name the format does not allow, or an empty version, ErrInvalid.
	Open(ctx context.Context, repo index.Repo, name, version string, principal auth.Principal) (*index.Session, error)

	// Add stores the bytes of r in the registry as one blob and records it in the session as
	// the file name, with its size and checksums, in place of a file of that name; size is -1
	// when unknown. A session that is not open, or has expired, is index.ErrNotFound; one being
	// committed, index.ErrConflict.
	Add(ctx context.Context, sessionID int64, name string, r io.Reader, size int64) (SessionFile, error)

	// Files returns the files of the session in the order they were first added.
	Files(ctx context.Context, sessionID int64) ([]SessionFile, error)

	// Commit publishes the files of the session as one version, through Publisher.Upload, and
	// closes the session. It is the idempotent repeat when the version holds these files already
	// (Published.Existed) and index.ErrConflict when it holds others, or when another Commit of
	// the session is running. A session that is not open, or has expired, is index.ErrNotFound;
	// one without files, ErrInvalid.
	Commit(ctx context.Context, sessionID int64) (*Published, error)

	// Abort closes the session without publishing; its blobs are left to the registry's garbage
	// collection. A session that is closed already is left as it is.
	Abort(ctx context.Context, sessionID int64) error
}

// SessionFile is a file of a publication session: its name there, the blob it is in the
// registry, and its checksums, computed while it arrived.
type SessionFile struct {
	// Name is the file's name as the format gave it to Add (maven: the path below the repo).
	Name string `json:"name"`
	// Digest and Size are those of the blob.
	Digest digest.Digest `json:"digest"`
	Size   int64         `json:"size"`
	// Checksums are the md5, sha1, sha256 and sha512 of the bytes, lowercase hexadecimal, by
	// those names.
	Checksums map[string]string `json:"checksums"`
}

// Signer signs a repo's index documents with the repo's own key and serves the public keys
// clients pin (apt's signed-by). internal/keys implements it; format does not import keys. A
// nil Signer in Deps means signing is off.
type Signer interface {
	// ClearSign returns data clearsigned with the repo's key: apt's InRelease.
	ClearSign(ctx context.Context, repo index.Repo, data []byte) ([]byte, error)
	// DetachSign returns the armored detached signature of data: apt's Release.gpg.
	DetachSign(ctx context.Context, repo index.Repo, data []byte) ([]byte, error)
	// PublicKeys returns the armored keyring that verifies what the repo signs: apt's key.asc.
	PublicKeys(ctx context.Context, repo index.Repo) ([]byte, error)
}

// Deps are the services a plugin's routes use. The server builds one Deps and passes it to
// every plugin.
type Deps struct {
	// Index reads the index; frontends also refresh index_cache through it.
	Index index.Store
	// Ingest is the only way to publish.
	Ingest Publisher
	// Providers resolves repo.Registry to the backend; routes use it to stream blobs and to
	// build redirect URLs for downloads.
	Providers provider.Set
	// Auth decides whether a principal may perform an action on a repo.
	Auth auth.Authorizer
	// Audit records writes.
	Audit audit.Writer
	// Log is the plugin's logger.
	Log *slog.Logger
	// Signer signs index documents with per-repo keys (apt InRelease, Release.gpg, key.asc);
	// nil when the server has no master key (keys.masterKeyEnv) and signing is off.
	Signer Signer
	// Sessions are the publication sessions, for a format whose upload is several requests.
	Sessions Sessions
}

# Spec: repository layout, package contracts, approved dependencies

- Status: normative for the MVP (2026-11-06). The Go interfaces in §2 are the shapes merged with
  the skeleton (issue #7, M0-01) and are contracts: changing an exported interface in
  `internal/{format,provider,index,ingest}`, `internal/index/reindex` included, needs an issue
  labelled `contract` and a maintainer's approval. The sections on `internal/auth` and
  `internal/audit` are informative: they describe those packages and are kept current, and a
  change to them needs no contract issue.
- Why this exists: two people work in parallel with AI assistants. Agreed package boundaries
  and interfaces are what keeps their pull requests from colliding.

## 1. Layout

```
cmd/lajstrom/                 main; subcommands: serve, reindex, migrate, version, healthcheck, registry (probe),
                              admin (repo/user/token/binding/key/bootstrap/retention/deb)
internal/config/              config file (YAML) + env overrides; validation; example in deploy/
internal/server/              http.Server, routing (net/http ServeMux, Go 1.22 patterns), middleware:
                              request id, logging, metrics, auth, panic recovery; mounts formats, facade, api
internal/facade/              OCI distribution API (/v2): proxy, token endpoint, manifest PUT hook
internal/format/              Plugin interface, registry of plugins, shared helpers (Router, Deps)
internal/format/raw/          raw plugin: parse, routes, index
internal/format/pypi/         pypi plugin: wheel/sdist metadata, simple index, upload API
internal/format/deb/          deb/apt plugin: control parsing, Packages/Release rendering, GPG, pool routes
internal/format/maven/        maven plugin: coordinates, POM, sessions, maven-metadata.xml, checksums
internal/format/npm/          npm plugin: packuments, publish, dist-tags journal, proxy and group hooks, audit
internal/format/rpm/          rpm plugin: header parsing, repodata (repomd, primary, filelists, other), signed repomd, Packages routes
internal/format/formattest/   test doubles: Fake plugin, partial index.Store; imported only by tests
internal/oci/profile/         media types, annotations, Escape/EscapeComponent, manifest build+validate (pure)
internal/ingest/              pipeline (validate → parse → push → index), publication sessions
internal/provider/            Provider interface, Capabilities, errors
internal/provider/generic/    oras-go based provider for distribution, Zot, Harbor, GHCR (+ small adapters)
internal/provider/ecr/        ECR adapter: auth token, CreateRepository, DescribeRepositories
internal/provider/probe/      lajstrom registry probe: measures a registry's capabilities (docs/registries)
internal/provider/providertest/ test doubles: an in-memory Provider; imported only by tests
internal/index/               Store interface, models, migrations (embedded SQL) and their runner
internal/index/reindex/       reindex worker: walk the registry, mark and sweep, runs, the report
internal/index/migrations/    <dialect>/NNNN_<name>.sql for sqlite and postgres, embedded; the same versions in both
internal/index/internal/sqlstore/ the SQL both engines share: index.Store over database/sql and a Dialect
internal/index/sqlite/        modernc.org/sqlite implementation
internal/index/postgres/      pgx implementation
internal/index/storetest/     conformance suite of index.Store, run by both engines; imported only by tests
internal/proxy/               proxy repos (ADR 0013): the settings, the upstream client, the metadata cache, fetch on miss
internal/auth/                principals, password hashing, PAT/deploy tokens, RBAC evaluation
internal/auth/token/          the bearer JWT of /v2: scopes, issue, verify
internal/auth/oidc/           OIDC relying party: discovery, JWKS, ID token verification, code exchange (#69)
internal/api/                 /api/v1 management REST (repos, users, tokens, bindings), JSON
internal/keys/                server signing key, per-repo GPG keys (apt, rpm), encryption at rest
internal/audit/               audit log writer
internal/metrics/             the Prometheus collectors (fixed labels) and the handler of the metrics listener
internal/testutil/            fixture builders, in memory: .deb, .rpm, .whl/sdist, .jar + POM, npm tarball, archives, bombs, fuzz seeds
internal/version/             build information (version, commit, date), set with -ldflags; /-/version
deploy/compose/               dev stack: zot, distribution, postgres; lajstrom.example.yaml
docs/                         architecture, specs, ADRs, plan, registries (spike results), quickstarts
test/                         e2e/, conformance/, upgrade/, load/: the levels that run a server (§4, docs/testing.md)
tools/                        bootstrap/ (bootstrap.py), guarantees/ (the check of guarantees.md, run by make test)
```

Conventions:

- `internal/` only; nothing is a public Go API in v0.x.
- A format plugin never imports another format plugin, nor `ingest`: plugins import
  `oci/profile`, `format`, `index` (read), `auth` and `provider` (reads; `make lint` refuses a
  write), and raw and npm `server`; they publish through `format.Deps.Ingest`. Frontend HTTP
  handlers live inside the plugin package (`routes.go`; maven's `deploy.go` and `resolve.go`);
  parsing is pure (no I/O beyond the reader passed in), in `parse.go` where the format has one.
- Only `provider/*` talks to the backend registry. Only `index/*` talks to the database.
- Errors: wrap with `%w`, sentinel errors per package (`ErrNotFound`, `ErrConflict`,
  `ErrUnsupported`). HTTP handlers map sentinel errors to status codes in one place per package.
- Context everywhere; no global state except the registries of plugins and of provider types,
  and no state in memory that must agree across replicas: several replicas serve the same
  registry and the same database, so anything that must be coordinated across requests (tag
  claims, upload sessions, background jobs) goes through the database, never through memory or
  local files. A replica keeps per-replica state across requests only where a stale or missing
  entry cannot break a guarantee: the RBAC snapshot (reloaded when `auth_version` moves), the
  login and token throttles, the upload slots, the proxy's single flight, ECR tokens.
- Logging with `log/slog`; metrics with Prometheus client; no other observability deps in the MVP.

## 2. Contracts

The exported shapes, grouped by package, first merged with issue #7 (M0-01) and, for the format
helpers, issue #11, and kept in step with the code since. Of `internal/format`,
`internal/provider`, `internal/index` and `internal/ingest` every exported identifier is here
but the fields of the index models (`internal/index/models.go`) and the `Error` and `Unwrap`
methods of error types; the sections of other packages show what the contract packages use of
them. Signatures are exact; comments are shortened. The doc comments in the code are the full
contract (`go doc -all ./internal/<package>`).

### Format plugin (`internal/format`)

```go
// Plugin is one package format. Implementations are stateless; dependencies come in via Deps.
type Plugin interface {
    Name() string                           // first URL segment and lajstrom.format: raw, pypi, apt, maven, npm
    ArtifactType() string                   // application/vnd.lajstrom.<type>.v1 ("deb" for apt)
    EscapeName(name string) (string, error) // package name → package path below <format>/<repo>/
    // Validate inspects a manifest about to be stored, reads blobs through fetch, returns the
    // index entry; disagreement between bytes, metadata layer and annotations wraps ErrInvalid.
    Validate(ctx context.Context, repo index.Repo, m ocispec.Manifest, fetch BlobFetcher) (*Entry, error)
    // Build assembles manifest and blobs from frontend uploads; it pushes nothing.
    Build(ctx context.Context, repo index.Repo, in Upload) (*Built, error)
    Routes(r Router, deps Deps)             // mounted below /<format>/ through NewRouter
    RenderIndex(ctx context.Context, repo index.Repo, scope IndexScope, st index.Reader) ([]Document, error)
}

// Streamer is a Plugin whose frontend uploads stream into the registry (ADR 0014): the frontend
// calls Publisher.UploadStream, Build takes the file without Open. raw, apt, pypi and npm are
// Streamers; maven's files stream through Sessions.Add instead.
type Streamer interface {
    Plugin
    // the checksums Build needs besides sha256, and the inspector the bytes pass through
    Inspect(repo index.Repo, up StreamUpload) (Inspection, error)
}

// Router takes ServeMux patterns relative to /<format>/, e.g. "GET /{repo}/simple/{project}/".
type Router interface {                     // implementation: NewRouter
    Handle(pattern string, h http.Handler)
    HandleFunc(pattern string, h http.HandlerFunc)
}

// Publisher is the write path as plugins see it; *ingest.Pipeline implements it.
type Publisher interface {
    Upload(ctx context.Context, repo index.Repo, up Upload, principal auth.Principal) (*Published, error)
    // UploadStream: one file of a Streamer's format, read once into a registry upload session,
    // the hashes and the inspector; the session is closed with the digest ingest computed once
    // Build and every check of Upload passed, cancelled otherwise (ADR 0014)
    UploadStream(ctx context.Context, repo index.Repo, up StreamUpload, principal auth.Principal) (*Published, error)
    // Delete: the manifest del names, its referrers before it, then its rows and the repo's
    // rendered indexes, one audit row; ingest.Pipeline.Delete (#61)
    Delete(ctx context.Context, repo index.Repo, del Deletion, principal auth.Principal) (*Deleted, error)
    // Journal appends ops to the journal of the package pkgPath (manifest-profile.md §8, ADR
    // 0012): under the package's lock, one manifest per op with the next numbers, then the
    // current state in the index, the repo's rendered indexes invalidated and an audit row per
    // op, in one transaction. It returns the numbers given; ingest.Pipeline.Journal (#307)
    Journal(ctx context.Context, repo index.Repo, pkgPath string, ops []JournalOp, principal auth.Principal) ([]int64, error)
}

// JournalOp is one change of a package's journal (manifest-profile.md §8): ArtifactType and Op
// select the kind, the other fields are the keys of that op, empty when the op has none.
type JournalOp struct {
    ArtifactType string // application/vnd.lajstrom.npm.dist-tags.v1, …npm.deprecation.v1
    Op           string // add, rm, deprecate, undeprecate
    Tag          string // lajstrom.npm.tag
    Version      string // lajstrom.npm.version
    Message      string // lajstrom.npm.message, deprecate only
}

// Sessions are the publication sessions of a format whose upload is several requests (maven,
// flow B); *ingest.Pipeline implements them (#53). A key, <name>:<version>, has one open session
// in a repo, held by one principal. Add streams the file into the registry (ADR 0014) and
// records it with its checksums; Refuse marks a file the frontend refused after it arrived
// (maven: a checksum file that disagrees), which keeps Commit from publishing until Add
// replaces it; Commit publishes the files through Upload, once at a time; a session whose time
// is up (30 minutes after its last request, or the repo's maven.sessionTimeout) is dropped,
// never published in part.
type Sessions interface {
    Open(ctx context.Context, repo index.Repo, name, version string, principal auth.Principal) (*index.Session, error)
    Add(ctx context.Context, sessionID int64, name string, r io.Reader, size int64) (SessionFile, error)
    Files(ctx context.Context, sessionID int64) ([]SessionFile, error)
    Refuse(ctx context.Context, sessionID int64, name, reason string) error
    Commit(ctx context.Context, sessionID int64) (*Published, error) // Existed: the idempotent repeat
    Abort(ctx context.Context, sessionID int64) error
}
type SessionFile struct {
    Name      string                        // maven: the path below the repo
    Digest    digest.Digest
    Size      int64
    Checksums map[string]string             // md5, sha1, sha256, sha512, lowercase hex
}

// Signer signs a repo's index documents with the repo's own key and serves the public keys
// clients pin (apt's signed-by); internal/keys implements it, format does not import keys (#41).
type Signer interface {
    ClearSign(ctx context.Context, repo index.Repo, data []byte) ([]byte, error)  // InRelease
    DetachSign(ctx context.Context, repo index.Repo, data []byte) ([]byte, error) // Release.gpg, armored
    PublicKeys(ctx context.Context, repo index.Repo) ([]byte, error)              // key.asc: an armored keyring
    Current(ctx context.Context, repo index.Repo, signed []byte) (bool, error)    // signed only by keys that still sign
}

type Deps struct {
    Index     index.Store
    Ingest    Publisher
    Providers provider.Set
    Auth      auth.Authorizer
    Audit     audit.Writer
    Log       *slog.Logger
    Signer    Signer                        // nil: signing is off (no keys.masterKeyEnv)
    Sessions  Sessions                      // publication sessions (maven)
    Proxy     Proxy                         // the core of proxy repos (*proxy.Core, ADR 0013); nil: none is served
}

type Entry struct {
    Name, Version, Variant string           // lajstrom.name, lajstrom.version, lajstrom.variant
    Attrs    map[string]string              // lajstrom.<format>.* annotations
    Files    []FileEntry                    // one per file layer, in manifest order
    Metadata json.RawMessage                // metadata layer; nil when none
}
type FileEntry struct {
    Name, MediaType string
    Digest          digest.Digest
    Size            int64
    Attrs           map[string]string       // layer annotations
}
type Upload struct {                        // the principal is a parameter of Publisher.Upload
    Files  []UploadedFile
    Params map[string]string                // protocol parameters: twine fields, suite, raw path
    Origin *Origin                          // set by the proxy fetcher only (ADR 0013); nil on a client's upload
}
// Origin is where a proxied artifact came from (manifest-profile.md §3.1): ingest writes
// lajstrom.origin.url and lajstrom.origin.checksum from it, leaves lajstrom.uploaded-by out, and
// audits proxy.fetch instead of push (rbac.md §7).
type Origin struct {
    Upstream string                         // upstreams[].name, details.upstream of the audit row
    URL      string                         // the first file's URL as asked, before redirects, without userinfo, query, fragment
    Checksum string                         // <algorithm>:<lowercase hex>, the upstream's checksum that was verified
    Published time.Time                     // the upstream's publish time of the version, org.opencontainers.image.created; zero: the time of the fetch
}
type UploadedFile struct {
    Name        string                      // maven: the path below /maven/<repo>/
    Size        int64                       // -1 when unknown
    ContentType string
    Open        func() (io.ReadCloser, error) // re-openable: Build reads it, ingest pushes it; nil when streamed
    // Lowercase hex by md5, sha1, sha256, sha512, computed once as the bytes streamed in; nil
    // when none. All four and a known Size: the file is in the backend (a session pushed it,
    // #53), Build need not open it and leaves it out of Built.Blobs. Without Open, the file
    // streamed (UploadStream): sha256 and Inspection.Checksums, and Build describes its layer
    // from them, leaving it out of Built.Blobs; ingest commits its session.
    Checksums   map[string]string
    Inspector   Inspector                   // a streamed file's, closed, for Build to read; nil otherwise
}
// StreamUpload is one file that streams into the registry as it is read (ADR 0014). The
// frontend knows the package before the first byte: raw from the path, npm from the URL, pypi
// from the file name of the distribution, apt from the control file it read ahead.
type StreamUpload struct {
    Name   string                           // the package name: EscapeName of it is the OCI repository
    File   UploadedFile                     // Name, Size (-1 unknown) and ContentType; the rest is ingest's
    Body   io.Reader                        // read once, to its end; bounded by the frontend (LimitBody, 413)
    Params map[string]string
    Origin *Origin
}
type Inspection struct {
    Checksums []string                      // besides sha256: md5, sha1, sha512
    Inspector Inspector                     // nil: Build needs nothing of the bytes but their checksums (raw)
}
// Inspector reads a file on its way into the registry: every byte through Write once, in order,
// Close after the last; bounded (#243), hostile input; an error refuses the upload.
type Inspector interface {
    io.Writer
    Close() error
}
type Built struct {
    Manifest ocispec.Manifest
    Blobs    []BlobSource                   // config, file layers, metadata layer
    Entry    Entry
    Tag      string
    RepoPath string                         // <format>/<repo>/<package path>, no instance prefix
}
type BlobSource struct {
    Descriptor ocispec.Descriptor
    Open       func(ctx context.Context) (io.ReadCloser, error)
}
// Read streams the whole blob, checked against its digest; the reader may also be an io.ReaderAt
// that reads parts with range requests, never the whole, and unchecked (ingest's is, #161).
type BlobFetcher func(ctx context.Context, d digest.Digest) (io.ReadCloser, error)
type IndexScope string                      // jammy (apt suite), simple/requests, com.example/lib-core, @acme/widgets
type Document struct {
    Kind, Path, ContentType string          // Path relative to /<format>/<repo>/
    Content                 []byte
}
type Published struct {
    Digest   digest.Digest
    Tag      string
    RepoPath string
    Entry    Entry
    Existed  bool                           // idempotent repeat: nothing was written
}
type Deletion struct {                      // the principal is a parameter of Publisher.Delete
    Path      string                        // package path below <format>/<repo>/ (Package.NameOCI)
    Reference string                        // a tag or a digest
    ClientIP  string                        // for the audit row
    Condition *Condition                    // not nil: a compare-and-delete of a version
    Job       string                        // the batch (POST …/versions:delete) the delete is part of, for the audit row; "" outside one
}
// Condition makes the delete of a version conditional (docs/api.md, Delete a version); ingest
// checks it under the locks of the delete, Reference must be the digest the client read, and a
// failed condition deletes nothing: the version gone is index.ErrNotFound, another digest
// RefusedChanged, published after UnmodifiedSince RefusedYoung, named by a pointer without
// Force RefusedProtected.
type Condition struct {
    Version, Variant string                 // the version the client read; Variant "" without variants
    UnmodifiedSince  time.Time              // a version published after it is refused; zero: none
    Force            bool                   // delete a version that pointers name
}
type Refusal string                         // why a conditional delete deleted nothing
const (
    RefusedChanged   Refusal = "changed"    // another digest than the client read (412)
    RefusedYoung     Refusal = "young"      // published after the cut-off (412)
    RefusedProtected Refusal = "protected"  // a pointer names the version (409)
)
// RefusedError: a Condition failed and nothing was deleted. Unwrap: ErrPrecondition (412) for
// RefusedChanged and RefusedYoung, index.ErrConflict (409) for RefusedProtected.
type RefusedError struct {
    Reason      Refusal
    Subject     string                      // the version, in the message: `version 1.0 of "requests" in pypi/libs`
    Digests     []digest.Digest             // the version's variants now, for RefusedChanged
    PublishedAt time.Time                   // the newest variant's publish time, for RefusedYoung
    Pointers    []Pointer                   // the pointers that name the version, for RefusedProtected, sorted
}
// Pointer names a version and can move to another: an npm dist-tag, Kind "dist-tag".
type Pointer struct {
    Kind string `json:"kind"`
    Name string `json:"name"`
}
// PointersOf: the pointers among a package's state (index.Reader.ListJournalState) that name
// version, in the order of the state.
func PointersOf(state []index.JournalState, version string) []Pointer
// PartialError is a delete of several manifests that failed part way (a raw path, an npm
// package, a version with every variant): Err decides the status, Deleted stays deleted, and
// ClientMessage names it for a 5xx too (guarantees.md, INV-13). Partial returns err as one, or
// err itself when nothing was deleted.
type PartialError struct {
    Deleted []string                        // "1.0.0 (sha256:…)"
    Err     error                           // Unwrap
}
func Partial(err error, deleted []string) error
type Deleted struct {
    Digest    digest.Digest
    Version   *index.Version                // the version it was; nil for a manifest that was none
    Referrers int                           // deleted before it, theirs included
}

func Register(p Plugin)                     // panics on nil, a name CheckName rejects, a duplicate
func All() []Plugin                         // sorted by name
func Lookup(name string) (Plugin, bool)

// Plugin names: ^[a-z0-9]+(-[a-z0-9]+)*$ and not reserved (http-routing.md §1).
var Reserved = []string{"-", ".well-known", "api", "oci", "ui", "v2"} // read-only; oci: pass-through repos (#24)
func IsReserved(segment string) bool
func CheckName(name string) error           // empty, reserved, or not the grammar; names the name

// NewRouter is the Router of plugin name: "GET /{repo}/x" is mounted as "GET /<name>/{repo}/x".
// A pattern with {repo} gets the repo middleware: one deps.Index.GetRepo per request; a missing
// repo or one of another format answers 404 (401 to the anonymous principal when deps.Auth is
// set, rbac.md §3), a failed lookup 500 (logged), else the repo is in the context. Panics on a
// nil mux or Index, a bad name or pattern, a nil handler.
func NewRouter(mux *http.ServeMux, name string, deps Deps) Router
func WithRepo(ctx context.Context, repo index.Repo) context.Context
func RepoFrom(ctx context.Context) (index.Repo, bool)

// Authorize asks deps.Auth whether auth.PrincipalFrom(ctx) may do a on repo: nil when allowed;
// denied: ErrUnauthenticated for anonymous, ErrForbidden for a principal that can read the
// repo, index.ErrNotFound for one that cannot (rbac.md §3); the authorizer's error unchanged.
// Through a group (ViaFrom(ctx), set by WriteTarget once the principal may read the group), a
// principal that is not anonymous and cannot read repo gets ErrForbidden naming repo, not
// index.ErrNotFound: the group's resource shows its members to its readers (rbac.md §7).
// A nil deps.Auth allows everything the credential allows, for the tests; serve sets auth.RBAC.
func Authorize(ctx context.Context, deps Deps, a auth.Action, repo index.Repo) error

// WriteTarget returns the repo that takes a write route sent to repo, and the context to serve
// it with (proxy-and-group.md §8.4): a hosted repo and ctx unchanged; for a group, once the
// principal may read the group (Authorize), its hosted member (deps.Index.ListGroupMembers) and
// WithVia(ctx, repo); for a proxy, or a group without a hosted member, an error wrapping
// ErrMethodNotAllowed once the principal may read it. The handler then authorizes the route's
// own action on the repo returned, with the context returned, and serves the write there.
func WriteTarget(ctx context.Context, deps Deps, repo index.Repo) (context.Context, index.Repo, error)
// WithVia marks ctx as a request that came through group: ingest adds details.via,
// <format>/<group>, to the audit rows of the uploads, deletes and journal entries made with it,
// and the proxy core to its proxy.fetch and proxy.reject rows (rbac.md §7). ViaFrom reads it.
func WithVia(ctx context.Context, group index.Repo) context.Context
func ViaFrom(ctx context.Context) (index.Repo, bool)

// WriteError answers StatusOf(err) as text/plain (nosniff) with ClientMessage(err); a 401
// carries WWW-Authenticate: Basic realm="lajstrom", a 405 Allow: GET, HEAD, a 503 of
// provider.ErrUnavailable, of index.ErrUnavailable or of an *UnavailableError Retry-After
// (SetRetryAfter).
func WriteError(w http.ResponseWriter, err error) int
func StatusOf(err error) int
// ClientMessage: err.Error() for a 4xx but a 408, a fixed message for a 408, the status text for
// a 5xx followed by what a *PartialError in err deleted before it failed.
func ClientMessage(err error) string
// SetRetryAfter sets Retry-After from provider.RetryAfter(err), or from the *UnavailableError
// err wraps: whole seconds, rounded up, at least 1; 5 for index.ErrUnavailable; nothing for an
// error without one. WriteError, the facade, the API and the authentication middleware call it.
func SetRetryAfter(h http.Header, err error)

// CachedDocument returns the index_cache row of (repo, scope, kind), or renders the scope with
// p.RenderIndex, stores every document (SHA256, RenderedAt) and returns the one of kind; lookup,
// render and store run in one st.Tx, and the documents are stored only while the repo's
// IndexCacheGeneration is the one read before the render. index.ErrNotFound when no document
// of kind is rendered.
func CachedDocument(ctx context.Context, st index.Store, p Plugin, repo index.Repo, scope IndexScope, kind string) (*index.Cache, error)
// ServeDocument: http.ServeContent with ETag "<sha256>" and Last-Modified RenderedAt.
func ServeDocument(w http.ResponseWriter, r *http.Request, c *index.Cache, contentType string)

// ServeBlob is the one read path of blob bytes, for the facade and every frontend (ADR 0009):
// GET and HEAD of the blob d with one request to the backend at most. If-None-Match is 304
// before any request; HEAD is Provider.StatBlob; GET is Provider.OpenBlob with the client's
// Range when it is one byte range, a redirect answered 307 with Location, a body streamed with
// the backend's status, Content-Length, Content-Range and Accept-Ranges. Errors before the
// response go to opts.Fail (nil: WriteError); the error returned is a stream with
// Content-Length that broke after the headers, for the caller's log. A stream without
// Content-Length that breaks, and a whole blob whose bytes are not d, abort the response: a
// panic wrapping http.ErrAbortHandler, which callers let through to net/http (INV-40, #568).
type ServeBlobOptions struct {
    ContentType    string                   // required
    ETag           string                   // "" means the blob digest, quoted
    LastModified   time.Time                // zero means none
    ManifestDigest digest.Digest            // Docker-Content-Digest when not the blob's (raw: the manifest)
    Redirect       bool                     // false: stream even where the provider could redirect
    Filename       string                   // Content-Disposition: attachment when set
    Fail           func(w http.ResponseWriter, err error) // errors before the response; nil: WriteError
}
func ServeBlob(w http.ResponseWriter, r *http.Request, prov provider.Provider, name string, d digest.Digest, opts ServeBlobOptions) error
// BlobRedirect reads policy.blobRedirect of the repo: "never" is false, anything else true.
func BlobRedirect(repo index.Repo) bool

// MaxFileSize: policy.maxFileSize of the repo, else its format's default (npm 512 MiB, pypi and
// maven 4 GiB, apt, raw and any other 8 GiB, oci 16 GiB); a value that is no non-negative
// integer is an error (500). LimitBody bounds an upload's body by it (http.MaxBytesReader): a
// Content-Length above it is refused before a read, and past it the reader fails wrapping the
// *http.MaxBytesError (413).
func MaxFileSize(repo index.Repo) (int64, error)
func LimitBody(w http.ResponseWriter, r *http.Request, repo index.Repo) (io.Reader, error)
// Spool copies r to a temporary file of $TMPDIR for one request, so that it can be read more
// than once: a request spool, never served from; Close removes it. size -1 is unknown; a body
// of another length than a known size wraps ErrInvalid. Uploads stream (ADR 0014); the proxy
// fetch spools its download with it (proxy-and-group.md §5).
type Spooled struct{ /* unexported */ }
func Spool(r io.Reader, name, contentType string, size int64) (*Spooled, error)
func (s *Spooled) File() UploadedFile       // re-openable, with name, size and content type
func (s *Spooled) Close() error
// ReadInspector is the Inspector of a parser that reads the file (gzip, tar): read runs on a
// goroutine of its own from the first Write, Close waits for it and returns its error.
func ReadInspector(read func(r io.Reader) error) Inspector

var ErrInvalid, ErrUnsupported, ErrUnauthenticated, ErrForbidden error
var ErrPrecondition error                   // a conditional delete's condition failed (RefusedError): 412
var ErrStreamed error                       // a BlobFetcher asked, in Validate, for the file that streams in with this upload
var ErrReadBack error                       // Build of a streamed file whose inspector did not keep what it needs: ingest commits and builds again, reading it back
// The statuses of a proxy's upstream and of a repo that takes no writes (ADR 0013,
// proxy-and-group.md §6).
var ErrMethodNotAllowed error               // a write route of a proxy, or of a group without a hosted member
var ErrBadGateway error                     // the upstream failed, refused Lajstrom's credentials, redirected where it may not, sent bytes the checksum or Validate refuses
var ErrUnavailable error                    // the upstream answered 429; an *UnavailableError carries its wait
var ErrGatewayTimeout error                 // the upstream did not answer within upstreams[].timeout
type UnavailableError struct{ RetryAfter time.Duration } // Unwrap: ErrUnavailable; the upstream's Retry-After, at most 60 s, 5 s without one
```

`StatusOf` is the one error table of the frontends; plugins wrap these sentinels with `%w`.
They are checked in this order:

| Error (`errors.Is`, `errors.As`) | Status |
| --- | --- |
| `os.ErrDeadlineExceeded` (the client sent no byte of the body within `server.stallTimeout`, also where a plugin made it `ErrInvalid`) | 408, a fixed message |
| `format.ErrInvalid` | 400 |
| `format.ErrUnauthenticated` | 401, `WWW-Authenticate: Basic realm="lajstrom"` |
| `format.ErrForbidden` | 403 |
| `format.ErrMethodNotAllowed` | 405, `Allow: GET, HEAD` |
| `index.ErrNotFound`, `provider.ErrNotFound` | 404 |
| `index.ErrConflict`, `provider.ErrConflict` | 409 |
| `format.ErrPrecondition` (a conditional delete) | 412 |
| `*http.MaxBytesError`; a `*provider.RejectedError` of a `413` (the file is too large for the backend) | 413 |
| `format.ErrUnsupported`, `provider.ErrUnsupported` | 501 |
| `provider.ErrUnauthorized` (the backend rejected Lajstrom's own credentials); a `*provider.RejectedError` that is not busy (the backend refused what Lajstrom sent it, #494) | 502 |
| `provider.ErrUnavailable` (the backend answered `429` or `503`, a busy `*provider.RejectedError`; a push the registry does not serve after the read-back, `*ingest.NotVisibleError`; the circuit breaker of the registry is open, the request was not sent; or its host name does not resolve, #561; or it refused or reset the connection, #593; or its host or network cannot be reached, `EHOSTUNREACH`, `ENETUNREACH`; or, after the retries, a request timed out or its connection closed without an answer, and the breaker counted that as the registry's failure, #670. A deadline or cancel of the caller's context, and a client's body that stalls or breaks off as it streams to the registry, are not the registry's: they are not `provider.ErrUnavailable` and keep their own error) | 503, `Retry-After`: the backend's for a `429` or `503` (1 without one), 30 s for a push not served, the seconds until the breaker's trial request, none for a registry that could not be reached or did not answer; under `/v2` the code `UNAVAILABLE`, on the API `UNAVAILABLE` |
| `index.ErrUnavailable` (the database did not answer) | 503, `Retry-After: 5`; under `/v2` the code `UNAVAILABLE`, on the API `UNAVAILABLE`; the authentication middleware answers it so too |
| `format.ErrBadGateway` | 502 |
| `format.ErrUnavailable` (the upstream answered `429`) | 503, `Retry-After`: the `*format.UnavailableError`'s, whole seconds, at least 1 |
| `format.ErrGatewayTimeout` | 504 |
| anything else, `nil` included | 500 |

### Proxy and group repos (`internal/format`, ADR 0013)

What a plugin provides for proxy and group repos of its format, and what its handlers ask the
core (proxy-and-group.md §10). A format has the kind `proxy` when its plugin implements
`Proxier`, and the kind `group` when it implements `Grouper`; creating a repo of a kind the
format does not have is `400`. The core, `internal/proxy`, does everything else: the allow and
deny lists, `proxy_meta` and its TTLs, the fetch claims, the upstream client, the stream and the
checksum, the ingest call, the statuses of §6, a group's members and which of them take part. A
handler asks the core for a document or a file of a proxy or a group alike, and for a write
asks `WriteTarget` where it goes.

```go
// Proxier is implemented by a plugin whose format has proxy repos.
type Proxier interface {
    // Key is a name as proxy_meta, index_cache and the allow and deny patterns see it: npm the
    // name, pypi the PEP 503 name, maven groupId:artifactId. A name the format does not allow
    // wraps ErrInvalid.
    Key(name string) (string, error)
    // MetadataRequest is the upstream request for the document of kind of name.
    MetadataRequest(name, kind string) (UpstreamRequest, error)
    // ParseMetadata reads a document of kind of name as the upstream sent it, or as Keep kept
    // it (Keep parses to the same Entries). A body it cannot read wraps ErrInvalid.
    ParseMetadata(name, kind string, body []byte) (*UpstreamIndex, error)
    // FileRequest maps a path below /<format>/<repo>/ to the artifact and the file it names; a
    // path that names no file of the format wraps index.ErrNotFound.
    FileRequest(path string) (ArtifactRef, error)
    // RenderProxy renders the document of kind of name for a proxy repo (§7) from kept, what
    // proxy_meta holds (nil without a row: the repo's versions alone, as a hosted repo's
    // document), and the versions of the name that st has in repo, whose bytes (checksums,
    // size) win over kept's. Every link to a file starts with base,
    // <server.externalURL>/<format>/<repo>/. The Document's Kind is kind.
    RenderProxy(ctx context.Context, repo index.Repo, name, kind string, kept []byte, st index.Reader, base string) (Document, error)
}
type UpstreamRequest struct {
    Path   string                           // relative to upstreams[].url, escaped, with its query: "left-pad", "@acme%2fwidgets"
    Accept string                           // the Accept header; "" for none
}
type UpstreamIndex struct {
    Keep        []byte                      // what proxy_meta keeps: the body, or a form the plugin trimmed
    ContentType string                      // of Keep
    Entries     []UpstreamEntry             // one per artifact the document lists
}
type UpstreamEntry struct {
    Version, Variant string
    Files     []UpstreamFile                // what one fetch downloads (npm: the tarball), in the order Build takes them
    Params    map[string]string             // the Upload.Params Build needs for the artifact, as a client's upload carries them
    Published time.Time                     // the upstream's publish time; zero when none (ADR 0008, quarantine)
}
type UpstreamFile struct {
    Name      string                        // UploadedFile.Name, and the name of the files row
    URL       string                        // absolute, or relative to upstreams[].url
    Checksums map[string]string             // sha512, sha256, sha1 → lowercase hex; the strongest is verified
}
type ArtifactRef struct {
    Name, Version, Variant string           // the package name as the format spells it, the version, the variant ("" when none)
    File                   string           // UpstreamFile.Name and files.name of the file asked for
    Kind                   string           // the document kind whose Entries list the artifact
}

// Grouper is implemented by a plugin whose format has group repos (#311). It is a Proxier: the
// core reads a group's members with Key, FileRequest and the documents of Proxy.Document.
type Grouper interface {
    Proxier
    // MergeGroup merges the documents of kind of name of the members of group that take part
    // (§8.1, §8.2), in member order, into the group's document (§8.3): the union of their
    // versions, the first member's entry of a version that several have, the format's own
    // fields by the format's rules (npm: http-routing.md, npm, Group). Every link to a file
    // starts with base, <server.externalURL>/<format>/<group>/; a member's links are under its
    // own base. docs has at least one element. The Document's Kind is kind.
    MergeGroup(ctx context.Context, group index.Repo, name, kind string, docs []MemberDocument, base string) (Document, error)
}
type MemberDocument struct {
    Repo    index.Repo                       // the member; its Kind tells the hosted member from a proxy
    Content []byte                           // its document of the kind, as it serves it
}

// Adopter is implemented by a Proxier whose proxy repos may become hosted repos (§2.6, #441).
type Adopter interface {
    Proxier
    // AdoptKind is the kind of the kept document AdoptOps reads: npm the full packument.
    AdoptKind() string
    // AdoptOps returns the journal ops (Publisher.Journal) that give a hosted repo, which
    // holds the versions held of name, the pointers and the deprecations of kept, the proxy's
    // document of AdoptKind as ParseMetadata kept it; npm: a dist-tag add per dist-tag whose
    // version is held, a deprecate per held version with a message. What it leaves out (a
    // dist-tag of a version not held) is described in skipped. A body it cannot read wraps
    // ErrInvalid.
    AdoptOps(name string, kept []byte, held []string) (ops []JournalOp, skipped []string, err error)
}

// Prefetcher is implemented by a Proxier whose proxy repos take a prefetch (§5.1, #441).
type Prefetcher interface {
    Proxier
    // PrefetchKinds are the kinds of the documents a prefetch reads, at least one: the first
    // lists the entries it fetches (npm: the full packument); the others it reads so that an
    // offline proxy keeps them (npm: the abbreviated packument, which npm install asks for).
    PrefetchKinds() []string
    // Pointed returns the versions that the pointers of a document of kind of name name, read
    // from kept as ParseMetadata kept it (npm: the targets of its dist-tags). A prefetch without
    // allVersions takes their entries. A body it cannot read wraps ErrInvalid.
    Pointed(name, kind string, kept []byte) ([]string, error)
}

// Proxy is the core as the plugins' handlers see it; *proxy.Core implements it. Document and
// File take a repo of kind proxy, or of kind group of a format whose plugin is a Grouper;
// Forward and Denies a repo of kind proxy; another kind wraps ErrUnsupported.
type Proxy interface {
    // Document of a proxy: the lists (§3), then the index_cache row of (repo, Key(name), kind)
    // while the proxy_meta row of (Key(name), kind) is fresh; otherwise the upstream is asked
    // (§4.2), the row written and the index_cache rows of scope Key(name) invalidated in one
    // transaction, and the document rendered (RenderProxy) and stored. Without a row it is
    // rendered and not stored. Errors wrap the sentinels of §6.
    // Document of a group (§8): the members in order (Index.ListGroupMembers), each proxy member
    // hidden while the hosted member has a package of the package path of name and name
    // matches no group.merge pattern; the hosted member's document is CachedDocument of scope
    // Key(name) and kind (a format with groups renders a hosted repo's document of a name under
    // that scope), a proxy member's what Document answers for it with WithVia(ctx, group), a 403
    // of its lists or a 404 meaning the member does not have the name, and another error that
    // it does not take part (§8.1). The documents are merged (MergeGroup) on every call and
    // nothing is stored: the Cache has RepoID the group's, Scope Key(name), SHA256 the group's
    // ETag (§8.5), RenderedAt the newest member document's, no ID. When no member has the name:
    // the first proxy member's error, else index.ErrNotFound.
    Document(ctx context.Context, repo index.Repo, name, kind string) (*index.Cache, error)
    // File of a proxy: the file path names (FileRequest), held or fetched and ingested on a miss
    // (§5) by the principal of ctx (auth.PrincipalFrom); the handler serves it as a hosted file
    // (ServeBlob). Errors wrap the sentinels of §6.
    // File of a group: the first member taking part (as for Document) that holds the file, or,
    // for a proxy member, File on it, with WithVia(ctx, group); index.ErrNotFound when the
    // hosted member has the name and not the version. The FileMatch's Package.RepoID is the
    // member that holds the file: the handler serves it from that repo (Index.GetRepoByID).
    File(ctx context.Context, repo index.Repo, path string) (*index.FileMatch, error)
    // Forward sends req to the repo's upstream and returns the answer, which nothing keeps: a
    // request of the format's protocol that only the upstream can answer (npm audit). It is
    // the core's own request through the upstream client (§9.2): the upstream's credentials on
    // its origin only, no header of the client's, the timeouts and retries of §2.1 (the body
    // is sent again), at most 5 redirects, never https to http, public addresses off the
    // upstream's host. Body is read whole before the first byte goes out, at most 16 MiB; a
    // larger one wraps *http.MaxBytesError (413) and nothing is sent, an error of Body is
    // returned wrapped. A 2xx is returned with StatusCode, Header (the upstream's: the handler
    // copies what its protocol needs, never Set-Cookie) and Body, which the caller closes: it
    // streams, a read past maxMetadataSize fails wrapping ErrBadGateway, a stall of the timeout
    // ErrGatewayTimeout. Otherwise nothing of the upstream's body is read: an offline repo
    // (§4.4) sends nothing and wraps index.ErrNotFound, as do the upstream's 404 and 410; its
    // 400, 413, 415 and 422 wrap ErrInvalid (the client's body refused); its 429 is an
    // *UnavailableError; a timeout ErrGatewayTimeout; anything else ErrBadGateway (§6). Not
    // cached, no audit row; logged and counted as every upstream request (operation forward).
    Forward(ctx context.Context, repo index.Repo, req ForwardRequest) (*http.Response, error)
    // Denies reports whether the deny list of the proxy repo refuses name (§3), matched on
    // Key(name), or on name as it is when the format does not allow it, and the pattern: a
    // name it refuses never leaves the server, and the handler leaves it out of what it sends
    // with Forward (npm audit, #514). allow does not count here. No I/O.
    Denies(repo index.Repo, name string) (pattern string, denied bool, err error)
}
// ForwardRequest is what Proxy.Forward sends: each part chosen by the plugin, nothing copied
// from the client's request.
type ForwardRequest struct {
    Method      string                      // POST; "" means GET
    Path        string                      // relative to upstreams[].url, escaped, with its query: "-/npm/v1/security/advisories/bulk"; one that resolves outside the upstream's URL is an error
    ContentType string                      // of Body; "" for none
    Accept      string                      // "" for none
    Body        io.Reader                   // nil for none; sent without Content-Encoding (a plugin decodes a gzipped body)
}
```

### Registry provider (`internal/provider`)

```go
// Names are <format>/<repo>/<package path>; the provider prepends registries[].prefix and
// strips it from listings.
type Provider interface {
    Name() string                           // registries[].name
    Capabilities(ctx context.Context) (Capabilities, error)
    Repository(ctx context.Context, name string) (Repository, error)
    EnsureRepository(ctx context.Context, name string) error      // no-op where create-on-push works
    ListRepositories(ctx context.Context, prefix string) ([]string, error)
    DeleteRepository(ctx context.Context, name string) error
    // OpenBlob: one GET of the blob with rangeHeader ("" for none) passed through. The answer
    // is a redirect for the client (RedirectURL: a cross-origin Location the backend gave, a
    // presigned URL) or the open body with its headers; a redirect to the registry's own origin
    // is followed. redirect false, or Capabilities.BlobRedirect false, follows every redirect
    // and streams: the decision is taken per blob (policy.blobRedirect of the repo, ADR 0009).
    // ErrNotFound for a blob the backend does not have; another Docker-Content-Digest is an error.
    OpenBlob(ctx context.Context, name string, d digest.Digest, rangeHeader string, redirect bool) (Blob, error)
    // StatBlob: one HEAD, redirects followed; the size and digest, no body.
    StatBlob(ctx context.Context, name string, d digest.Digest) (Blob, error)
    // StatManifest: one HEAD of the manifest by its digest; on a 404 a GET too, for the error
    // code a HEAD has no body for. ErrManifestUnknown (and ErrNotFound) only for a 404 with
    // MANIFEST_UNKNOWN: what prune counts as unknown (guarantees.md, INV-5).
    StatManifest(ctx context.Context, name string, d digest.Digest) (ocispec.Descriptor, error)
}

// Blob is a blob opened for a download, or its headers (StatBlob).
type Blob struct {
    RedirectURL  string                     // the client is sent there (307); Body is nil
    Body         io.ReadCloser              // the bytes as the backend sent them; nil for a redirect, a 416, a StatBlob
    Status       int                        // 200, 206 or 416 as the backend answered; 307 with RedirectURL
    Size         int64                      // Content-Length of Body (the range's for a 206); -1 unknown
    ContentRange string                     // the backend's Content-Range on a 206 or 416
    Digest       digest.Digest              // Docker-Content-Digest of the backend, d when it sent none
    AcceptRanges bool                       // the backend said Accept-Ranges: bytes
}

// Repository is the data path; the generic provider wraps oras-go's remote.Repository (error translation).
type Repository interface {
    content.Storage                         // Fetch, Exists, Push (blobs and manifests)
    content.Deleter                         // Delete
    content.TagResolver                     // Tag, Resolve
    registry.ReferencePusher                // PushReference: manifest and tag in one PUT
    registry.TagLister                      // Tags
    registry.ReferrerLister                 // Referrers (tag-schema fallback without the API)
    BlobUploader                            // the backend's upload session, for the facade (#24)
    BlobStreamer                            // a blob Lajstrom streams in itself, for the frontends (ADR 0014)
}

// BlobUploader drives the backend's blob upload session chunk by chunk; an Upload is opaque and
// serialisable, so a replica that did not start it continues it. A digest the backend refuses
// wraps content.ErrMismatchedDigest.
type BlobUploader interface {
    StartBlobUpload(ctx context.Context) (Upload, error)                                                // POST …/blobs/uploads/
    MountBlob(ctx context.Context, d digest.Digest, from string) (mounted bool, u Upload, err error)     // POST …?mount=&from=
    AppendBlobChunk(ctx context.Context, u Upload, r io.Reader, size int64) (Upload, error)             // PATCH; size -1 unknown
    FinishBlobUpload(ctx context.Context, u Upload, d digest.Digest, r io.Reader, size int64) (ocispec.Descriptor, error) // PUT …?digest=
    BlobUploadStatus(ctx context.Context, u Upload) (Upload, error)                                      // GET
    CancelBlobUpload(ctx context.Context, u Upload) error                                                // DELETE
}
type Upload struct {                        // location as the backend answered it, backend id, bytes held
    Location       string
    ID             string
    Offset         int64
    ChunkMinLength int64                    // the backend's last OCI-Chunk-Min-Length; 0 when none (the facade passes it on)
}

// BlobStreamer opens the blob of a frontend's upload, kept only once every check passed (ADR 0014).
type BlobStreamer interface {
    OpenBlobWriter(ctx context.Context) (BlobWriter, error) // the session's POST may wait for the first chunk
}
// BlobWriter: Write sends chunks of the provider's size, each a PATCH with Content-Range, at most
// one chunk in memory; nothing is stored before Commit (PUT ?digest=); Cancel is the DELETE.
// Either ends it; a failed Write or Commit leaves the session to Cancel.
type BlobWriter interface {
    io.Writer
    Size() int64
    Commit(ctx context.Context, d digest.Digest) (ocispec.Descriptor, error) // content.ErrMismatchedDigest when refused
    Cancel(ctx context.Context) error
}
// NewChunkedWriter is the BlobWriter of a BlobUploader, chunks of chunkSize bytes, bounded to
// 1 byte to 256 MiB; the generic provider's are registries[].uploads.chunkSize, else the vendor
// table's (GHCR 4 MiB), else 8 MiB.
func NewChunkedWriter(ctx context.Context, u BlobUploader, chunkSize int) BlobWriter
var ErrWriterClosed error                   // Write or Commit after Commit or Cancel

// Pinger is optional: a Provider that has it is pinged by serve for /-/readyz (GET /v2/ with the
// configured credentials); one without it counts as answering (#23).
type Pinger interface {
    Ping(ctx context.Context) error
}

type Set interface {
    Provider(name string) (Provider, bool)
}
type Map map[string]Provider                // implements Set

// The implementations Register a Constructor per registries[].type from init (generic, ecr);
// New builds the Provider of one registry with it, NewSet every registry of the configuration
// (the errors of all, joined); Types lists the registered types, sorted.
type Constructor func(cfg config.Registry, log *slog.Logger) (Provider, error)
func Register(typ string, c Constructor)   // panics on an empty or duplicate type, a nil constructor
func New(cfg config.Registry, log *slog.Logger) (Provider, error) // nil log: slog.Default()
func NewSet(cfgs []config.Registry, log *slog.Logger) (Map, error)
func Types() []string

type Capabilities struct {
    CreateOnPush, Referrers, Catalog, DeleteManifest, ImmutableTags bool
    BlobRedirect bool                       // the operator's switch (capabilities.blobRedirect): false, OpenBlob never returns a URL
    MaxBlobSize int64                       // 0 = unknown
    Consistency Consistency                 // when reads see an acknowledged write: the vendor table's or capabilities.consistency
    ConsistencyBound time.Duration          // the longest lag known of an eventual backend (capabilities.consistencyBound); 0 = none known
    MaxNameDepth int                        // components of a name below the instance prefix; 0 = no limit
    MaxNameLength int                       // bytes of a name below the instance prefix; 0 = no limit
}
// CheckName: nil, or an error naming the limit of c that the name breaks; callers answer 4xx before a write.
func (c Capabilities) CheckName(name string) error
type Consistency string                     // docs/registries/README.md, Consistency
const (
    ConsistencyUnknown  Consistency = ""         // nobody classified the backend
    ConsistencyStrong   Consistency = "strong"   // every read after the 2xx of a write sees it
    ConsistencyEventual Consistency = "eventual" // a read may miss an acknowledged write for a while (ECR, GHCR)
)
// ReadBack reads again a backend that does not serve yet what it acknowledged: ingest's
// visibility barrier, and a blob a client says it uploaded. Await calls seen until it reports
// true, waiting 100 to 200 ms at first, then twice as long with jitter, at most 5 s a wait;
// false when the next wait would end past Timeout (zero: one read).
type ReadBack struct{ Timeout time.Duration }
func (b ReadBack) Await(ctx context.Context, seen func(context.Context) (bool, error)) (bool, error)

var ErrNotFound, ErrConflict, ErrUnsupported, ErrUnauthorized error
// ErrUnavailable: the backend takes no request now, or does not serve yet a write it
// acknowledged: its registry's circuit breaker is open and the request was not sent (an
// *UnavailableError with the time until the trial request); the backend answered 429 or 503
// (a busy *RejectedError); a push not served after the read-back (an *UnavailableError with a
// Reason). It is also a registry that could not be reached or did not answer (StatusOf's
// table above: #561, #593, #670), with no time.
var ErrUnavailable error
type UnavailableError struct {
    RetryAfter time.Duration
    Reason     string                       // for the log; "" is an open circuit breaker
}
// RetryAfter: of the *UnavailableError err wraps, or of its busy *RejectedError.
func RetryAfter(err error) (time.Duration, bool)
// ErrRejected: the backend refused a request with a status no other sentinel names (400
// BLOB_UPLOAD_INVALID, MANIFEST_INVALID, SIZE_INVALID, 413, 416), and a busy backend's 429 or
// 503. A *RejectedError says how; its message names the registry by registries[].name and
// holds nothing else of the backend's. Unwrap: ErrRejected, and ErrUnavailable when Busy.
var ErrRejected error
type RejectedError struct {
    Registry   string                       // registries[].name
    Status     int                          // a 4xx, or 503
    Code       string                       // the answer's first error code; "" when none
    RetryAfter time.Duration                // of a 429 or 503; zero when none
    Hint       string                       // what the operator may change, when the provider knows it
}
func (e *RejectedError) Busy() bool         // 429, 503: the backend throttles Lajstrom, not one client
// ErrManifestUnknown: a 404 with MANIFEST_UNKNOWN to StatManifest; the error wraps ErrNotFound too.
var ErrManifestUnknown error
```

### Index store (`internal/index`)

```go
type Store interface {
    Reader
    Writer
    Migrate(ctx context.Context) error
    // Tx: one transaction for fn; with a ctx of WithRepeatableRead, fn may run more than once
    Tx(ctx context.Context, fn func(Writer) error) error
    // WithReferrersLock calls fn holding the lock on the referrers of subject in the OCI
    // repository name: pg_advisory_xact_lock on Postgres, a process lock on SQLite
    // (index-schema.md, Coordination)
    WithReferrersLock(ctx context.Context, name string, subject digest.Digest, fn func(context.Context) error) error
    Close() error
}

// Get* return an error wrapping ErrNotFound; List*/Find* return an empty result.
type Reader interface {
    // control plane
    GetRepo(ctx context.Context, format, name string) (*Repo, error)
    GetRepoByID(ctx context.Context, id int64) (*Repo, error)
    ListRepos(ctx context.Context, format string) ([]Repo, error)
    // groups (ADR 0013, index-schema.md, group_members): the members of a group in member
    // order, and the groups that have a repo as a member, ordered by format and name
    ListGroupMembers(ctx context.Context, groupID int64) ([]Repo, error)
    ListGroupsOf(ctx context.Context, memberID int64) ([]Repo, error)
    GetUser(ctx context.Context, name string) (*User, error)
    GetUserByUID(ctx context.Context, uid string) (*User, error) // live or deleted
    GetUserByID(ctx context.Context, id int64) (*User, error)    // live or deleted: a token's owner
    ListUsers(ctx context.Context) ([]User, error)
    GetTokenByHash(ctx context.Context, hash string) (*Token, error)
    ListTokens(ctx context.Context, userID, repoID *int64) ([]Token, error)
    GetBootstrapToken(ctx context.Context) (*BootstrapToken, error) // used and expired too; the caller checks
    ListBindings(ctx context.Context, principal string) ([]Binding, error)
    // AuthVersion: the counter of auth_version, which every write of bindings and repos
    // increments in its transaction (rbac.md §3); an evaluator reads it before its snapshot
    AuthVersion(ctx context.Context) (int64, error)
    // OIDC (rbac.md §8, index-schema.md, identities): an identity by its issuer and subject,
    // and the groups of every identity with its user's id and uid and its groups_expire_at,
    // ordered by uid and name, whether expired or not (the RBAC snapshot compares the time)
    GetIdentity(ctx context.Context, issuer, subject string) (*Identity, error)
    ListIdentityGroups(ctx context.Context) ([]IdentityGroup, error)
    GetKey(ctx context.Context, repoID *int64, kind string) (*Key, error)
    ListKeys(ctx context.Context, repoID *int64) ([]Key, error)
    GetSession(ctx context.Context, repoID int64, key string) (*Session, error)
    GetSessionByID(ctx context.Context, id int64) (*Session, error)
    ListAudit(ctx context.Context, f AuditFilter) ([]AuditEvent, error)
    // coordination
    GetTagClaim(ctx context.Context, repoID int64, pkg, tag string) (*TagClaim, error)
    GetFetchClaim(ctx context.Context, repoID int64, pkg, tag string) (*FetchClaim, error)
    GetReindexRun(ctx context.Context, id int64) (*ReindexRun, error)
    // the flows of rbac.md §8 (index-schema.md, oidc_logins); state and deviceCode are the
    // sha256 hex of the secret, userCode as shown without its hyphen
    GetOIDCLogin(ctx context.Context, id int64) (*OIDCLogin, error)
    GetOIDCLoginByState(ctx context.Context, state string) (*OIDCLogin, error)
    GetOIDCLoginByDeviceCode(ctx context.Context, deviceCode string) (*OIDCLogin, error)
    GetOIDCLoginByUserCode(ctx context.Context, userCode string) (*OIDCLogin, error)
    // LatestReindexRun: the newest run that covered repoID, its own or a run of every repo;
    // repoID 0 selects the runs of every repo only
    LatestReindexRun(ctx context.Context, repoID int64) (*ReindexRun, error)
    ListReplicas(ctx context.Context) ([]Replica, error) // by StartedAt; a heartbeat 2 minutes old is a replica gone
    DatabaseTime(ctx context.Context) (time.Time, error) // the database's clock: clock_timestamp(); SQLite the host's
    // derived
    GetPackage(ctx context.Context, repoID int64, name string) (*Package, error)
    GetPackageByNameOCI(ctx context.Context, repoID int64, nameOCI string) (*Package, error)
    ListPackages(ctx context.Context, repoID int64, prefix string) ([]Package, error)
    // ListPackagesAfter: a page of ListPackages, the names after `after` in byte order ("": from
    // the first), at most limit (index-schema.md, Pages of packages and versions)
    ListPackagesAfter(ctx context.Context, repoID int64, prefix, after string, limit int) ([]Package, error)
    GetVersion(ctx context.Context, packageID int64, version, variant string) (*Version, error)
    GetVersionByTag(ctx context.Context, packageID int64, tag string) (*Version, error)
    GetVersionByDigest(ctx context.Context, repoID int64, d digest.Digest) (*Version, error)
    ListVersions(ctx context.Context, packageID int64) ([]Version, error)
    // ListVersionPage: the rows of the limit versions of a package after `after` (nil: from the
    // newest), newest first by VersionKey, every variant of each, a version's rows by variant
    ListVersionPage(ctx context.Context, packageID int64, after *VersionKey, limit int) ([]Version, error)
    // ListUnseenVersions: the versions of a repo whose IndexedAt is before seenBefore, with
    // their packages (the sweep of reindex)
    ListUnseenVersions(ctx context.Context, repoID int64, seenBefore time.Time) ([]VersionMatch, error)
    ListFiles(ctx context.Context, versionID int64) ([]File, error)
    FindFiles(ctx context.Context, repoID int64, name string) ([]FileMatch, error)
    GetIndexCache(ctx context.Context, repoID int64, scope, kind string) (*Cache, error)
    // IndexCacheGeneration: 0 at first, incremented by every invalidation of the repo's
    // index_cache; a render reads it first (index-schema.md, The generation of a repo's documents)
    IndexCacheGeneration(ctx context.Context, repoID int64) (int64, error)
    ListOCIRepos(ctx context.Context, repoID int64) ([]OCIRepo, error)
    // a cache of the upstream (ADR 0013): the row of a proxy repo's name key and document kind
    GetProxyMeta(ctx context.Context, repoID int64, name, kind string) (*ProxyMeta, error)
    // the npm journal (ADR 0012, index-schema.md, npm): a package's entries in seq order, and
    // its current state ordered by kind and key
    ListJournal(ctx context.Context, packageID int64) ([]JournalEntry, error)
    ListJournalState(ctx context.Context, packageID int64) ([]JournalState, error)
}

// Unique violations wrap ErrConflict; updates and deletes of a missing id wrap ErrNotFound.
type Writer interface {
    Reader
    // control plane
    CreateRepo(ctx context.Context, r *Repo) error
    UpdateRepo(ctx context.Context, r *Repo) error  // Format, Name and Kind never change
    // DeleteRepo: its proxy_meta, fetch_claims and, for a group, group_members rows go with
    // it; a repo that is a member of a group wraps ErrConflict and nothing is deleted
    DeleteRepo(ctx context.Context, id int64) error
    // SetGroupMembers makes the group_members rows of the group exactly memberIDs, position the
    // index in it; an id that is no repo wraps ErrNotFound, an id named twice ErrConflict. The
    // caller writes the group's repos row in the same transaction (CreateRepo, UpdateRepo), so
    // that auth_version moves with the members
    SetGroupMembers(ctx context.Context, groupID int64, memberIDs []int64) error
    // ConvertRepo makes the proxy repo id a hosted one (proxy-and-group.md §2.6): kind hosted,
    // upstream NULL, UpdatedAt now, auth_version incremented. A repo that is not a proxy, or
    // has a fetch claim that beat at or after staleBefore, wraps ErrConflict; no repo
    // ErrNotFound. The one change of a kind
    ConvertRepo(ctx context.Context, id int64, staleBefore time.Time) error
    CreateUser(ctx context.Context, u *User) error
    UpdateUser(ctx context.Context, u *User) error
    CreateToken(ctx context.Context, t *Token) error
    RevokeToken(ctx context.Context, id int64, at time.Time) error
    TouchToken(ctx context.Context, id int64, lastUsed time.Time) error
    // PutBootstrapToken makes t the bootstrap token while the stored one is the one of hash old
    // ("": none), UsedAt cleared; another row there wraps ErrConflict, so that replicas that
    // start together make one. UseBootstrapToken sets UsedAt when hash matches, it is unused
    // and not expired at at, else ErrNotFound, holding the row until the transaction ends
    PutBootstrapToken(ctx context.Context, t *BootstrapToken, old string) error
    UseBootstrapToken(ctx context.Context, hash string, at time.Time) error
    CreateBinding(ctx context.Context, b *Binding) error
    DeleteBinding(ctx context.Context, id int64) error
    // OIDC: CreateIdentity fills ID and CreatedAt when zero; (Issuer, Subject) taken wraps
    // ErrConflict. SetIdentityGroups makes the identity_groups rows of the identity exactly
    // names, stores loginAt and groupsExpireAt, and increments auth_version
    CreateIdentity(ctx context.Context, i *Identity) error
    DeleteIdentity(ctx context.Context, id int64) error
    SetIdentityGroups(ctx context.Context, id int64, names []string, loginAt, groupsExpireAt time.Time) error
    CreateKey(ctx context.Context, k *Key) error
    RetireKey(ctx context.Context, id int64, at time.Time) error
    UpsertSession(ctx context.Context, s *Session) error // a second open session of a key: ErrConflict
    // TouchSession moves the expiry of an open, unexpired session and returns it, holding the
    // row until the transaction ends: a change of its files starts with it.
    TouchSession(ctx context.Context, id int64, at, expiresAt time.Time) (*Session, error)
    DeleteSession(ctx context.Context, id int64) error
    ExpireSessions(ctx context.Context, before time.Time) (int, error)
    AppendAudit(ctx context.Context, ev *AuditEvent) error // State "" is ok
    // the row of a registry write: pending before it, then ok or failed once (pending or
    // unknown rows only); unknown when still pending before before (index-schema.md, audit)
    CompleteAudit(ctx context.Context, ev *AuditEvent) error
    ExpirePendingAudit(ctx context.Context, before time.Time) (int, error)
    // coordination: same digest shares the claim, a stale one is taken over, a fresh one with
    // another digest is *TagConflictError
    ClaimTag(ctx context.Context, c *TagClaim, staleBefore time.Time) error
    ReleaseTagClaim(ctx context.Context, id int64) error
    ExpireTagClaims(ctx context.Context, before time.Time) (int, error)
    // StartReindexRun inserts r as running under the reindex lock; a running run of an
    // overlapping scope with a heartbeat at or after staleBefore is *ReindexConflictError, an
    // older one is closed as abandoned first (index-schema.md, Coordination)
    StartReindexRun(ctx context.Context, r *ReindexRun, staleBefore time.Time) error
    // HeartbeatReindexRun sets HeartbeatAt; ErrNotFound when the run is not running any more
    HeartbeatReindexRun(ctx context.Context, id int64, at time.Time) error
    // FinishReindexRun stores State, FinishedAt, Report and Error of the running run r.ID and
    // deletes the finished runs of its scope beyond the ten newest
    FinishReindexRun(ctx context.Context, r *ReindexRun) error
    // ClaimFetch inserts c, the lock of a proxy fetch on (RepoID, Package, Tag) across replicas
    // (index-schema.md, Proxy and group repos), ClaimedAt now when zero and HeartbeatAt
    // ClaimedAt when zero, and fills ID. A claim there whose HeartbeatAt is at or after
    // staleBefore wraps ErrConflict and nothing changes (the loser of a race gets it too); an
    // older one is deleted and c inserted with a new id, so that the heartbeat and the release
    // of the fetch that held it find nothing
    ClaimFetch(ctx context.Context, c *FetchClaim, staleBefore time.Time) error
    HeartbeatFetchClaim(ctx context.Context, id int64, at time.Time) error // ErrNotFound: released or taken over
    ReleaseFetchClaim(ctx context.Context, id int64) error
    // CreateOIDCLogin inserts l and fills ID; a state, device code or user code taken wraps
    // ErrConflict. UpdateOIDCLogin stores Status, UserID and PolledAt of l while the row's status
    // is from, else ErrConflict and nothing changes: one redemption of a state, one approval,
    // one device code. ExpireOIDCLogins deletes the rows whose ExpiresAt is before before
    CreateOIDCLogin(ctx context.Context, l *OIDCLogin) error
    UpdateOIDCLogin(ctx context.Context, l *OIDCLogin, from string) error
    ExpireOIDCLogins(ctx context.Context, before time.Time) (int, error)
    ExpireFetchClaims(ctx context.Context, before time.Time) (int, error)  // HeartbeatAt before before; reindex, reconcile
    // StartReplica deletes the rows whose HeartbeatAt is before staleBefore and inserts r, a
    // serve process (index-schema.md, Coordination); on SQLite a fresh row of another replica
    // is *ReplicaConflictError naming it and nothing changes, on Postgres the rows coexist
    StartReplica(ctx context.Context, r *Replica, staleBefore time.Time) error
    HeartbeatReplica(ctx context.Context, id string, at time.Time) error // ErrNotFound: deleted as stale
    StopReplica(ctx context.Context, id string) error
    // operational: the answers 404 MANIFEST_UNKNOWN that a removal by reindex waits on
    // (index-schema.md, Coordination); ResetDerived leaves them
    RecordSighting(ctx context.Context, s *Sighting) error                     // insert, or set LastAt of the row; fills FirstAt from it
    ClearSightings(ctx context.Context, repoID int64, before time.Time) (int, error) // the repo's rows with LastAt before before
    // derived (written only by ingest, reindex and delete); every version write and delete keeps
    // packages.created_at the earliest created_at of the package's versions
    UpsertPackage(ctx context.Context, p *Package) error
    UpsertVersion(ctx context.Context, v *Version) error           // *TagConflictError on a tag with another digest
    TouchVersions(ctx context.Context, ids []int64, at time.Time) error // IndexedAt only: rows reindex saw
    ReplaceFiles(ctx context.Context, versionID int64, files []File) error
    DeleteVersion(ctx context.Context, id int64) error
    DeletePackage(ctx context.Context, id int64) error
    DeleteEmptyPackages(ctx context.Context, repoID int64) (int, error) // packages without versions
    // PutIndexCache stores c only while generation, read before the render, is the repo's, and
    // says whether it did; InvalidateIndexCache increments the generation, then deletes
    PutIndexCache(ctx context.Context, c *Cache, generation int64) (bool, error)
    InvalidateIndexCache(ctx context.Context, repoID int64, scope string) error
    UpsertOCIRepo(ctx context.Context, r *OCIRepo) error
    // DeleteOCIRepo: the row of (repoID, path), an oci repo's repository left without a tag;
    // ErrNotFound when there is none. The caller lists the tags again after it and records the
    // repository again when one is there (index-schema.md, Invariants and rules; #748)
    DeleteOCIRepo(ctx context.Context, repoID int64, path string) error
    PruneOCIRepos(ctx context.Context, repoID int64, seenBefore time.Time) (int, error)
    ResetDerived(ctx context.Context, repoID int64) error // leaves the claims of both kinds, prune_sightings and proxy_meta
    // proxy_meta, a cache of the upstream that reindex neither reads nor writes; the caller
    // invalidates the index_cache rows of scope Name in the same transaction (a 304: none)
    PutProxyMeta(ctx context.Context, m *ProxyMeta) error                         // insert, or replace the row of (RepoID, Name, Kind); fills ID
    TouchProxyMeta(ctx context.Context, id int64, checkedAt time.Time) error      // CheckedAt only: a 304
    DeleteProxyMeta(ctx context.Context, repoID int64, name string) (int, error)  // the rows of one name key, or every row of the repo when ""
    ExpireProxyMeta(ctx context.Context, repoID int64, before time.Time) (int, error) // the repo's absent rows checked before before
    // the npm journal (derived; written by ingest, reindex and delete). UpsertJournal inserts e
    // and fills ID, or, when the row of (PackageID, Seq) has e's tag and digest, sets its
    // IndexedAt and fills e from it; another tag or digest there is a *TagConflictError and
    // nothing changes: an entry is never rewritten. CreatedAt and IndexedAt are now when zero
    UpsertJournal(ctx context.Context, e *JournalEntry) error
    TouchJournal(ctx context.Context, ids []int64, at time.Time) error // IndexedAt only: entries reindex saw
    DeleteJournal(ctx context.Context, id int64) error                 // the reindex sweep
    // ReplaceJournalState makes the package's journal_state rows exactly state, PackageID
    // filled: the fold of its entries, written in the transaction of the entries it folds
    ReplaceJournalState(ctx context.Context, packageID int64, state []JournalState) error
}

// Models, one struct per table of index-schema.md (fields in internal/index/models.go):
// control plane Repo, User, Token, BootstrapToken, Binding, Identity, Key, Session, AuditEvent;
// coordination TagClaim, ReindexRun, FetchClaim, Replica, OIDCLogin; derived Package, Version,
// File, Cache (index_cache), OCIRepo, ProxyMeta, JournalEntry, JournalState; query types
// FileMatch (file + version + package), VersionMatch (version + package), VersionKey,
// IdentityGroup (a row of identity_groups with its identity's UserID and GroupsExpireAt and its
// user's UID) and AuditFilter. Ids are int64, timestamps UTC, nullable columns are pointers.
const ReindexRunning, ReindexDone, ReindexFailed, ReindexAbandoned = "running", "done", "failed", "abandoned" // ReindexRun.State
type ReindexOptions struct {                // reindex_runs.options: what the run was started with
    Prune  bool `json:"prune,omitempty"`
    Verify bool `json:"verify,omitempty"`
}
// AuditEvent.State: a change of the index alone is ok from its insert; a registry write is
// pending before the write, then ok or failed, and unknown once still pending after
// ingest.claimTTL (ExpirePendingAudit).
const AuditPending, AuditOK, AuditFailed, AuditUnknown = "pending", "ok", "failed", "unknown"
// NewUID: a user's uid (rbac.md §1), a UUID v7 in canonical lowercase text, its first 48 bits
// the milliseconds of at. IsUID: s has the form of a uid, of any version.
func NewUID(at time.Time) string
func IsUID(s string) bool

// The place of a version in ListVersionPage, which orders by it descending: CreatedAt, then
// Version in byte order (index-schema.md, Pages of packages and versions).
type VersionKey struct {
    CreatedAt time.Time                     // the earliest CreatedAt of the version's rows: its first variant
    Version   string
}

// Repo, for ADR 0013 (repos.upstream, repos.group):
//     Kind     string                         // KindHosted, KindProxy or KindGroup
//     Upstream json.RawMessage                // a proxy's settings (proxy-and-group.md §2.2); nil on another kind
//     Group    json.RawMessage                // a group's settings but its members, {"merge"} (§2.3); nil on another kind
// A group's Registry is "" and its Policy nil; its members are group_members rows
// (ListGroupMembers, SetGroupMembers), not a field: GetRepo stays one row.
const KindHosted, KindProxy, KindGroup = "hosted", "proxy", "group"

const ProxyMetaPresent, ProxyMetaAbsent = "present", "absent"
type ProxyMeta struct {                     // a row of proxy_meta
    ID, RepoID         int64
    Name, Kind         string               // Proxier.Key of the name, the plugin's document kind
    State              string               // ProxyMetaPresent or ProxyMetaAbsent
    Content            []byte               // UpstreamIndex.Keep; nil when absent
    ContentType        string
    SHA256             string               // hex sha256 of Content; "" when absent
    ETag, LastModified string               // the upstream's validators as it sent them; "" when none
    CheckedAt          time.Time            // when the upstream last answered for the name
}
// The npm journal (ADR 0012, manifest-profile.md §8, index-schema.md, npm). Both tables cascade
// with packages: DeletePackage, DeleteEmptyPackages and ResetDerived take a package's rows.
const JournalDistTag, JournalDeprecated = "dist-tag", "deprecated" // journal_state.kind
type JournalEntry struct {                  // a row of journal: one entry in the registry
    ID, PackageID int64
    Seq           int64                     // lajstrom.npm.seq
    Tag           string                    // _j00000007
    Digest        digest.Digest             // the entry's manifest
    ArtifactType  string
    Attrs         map[string]string         // the entry's lajstrom.npm.* annotations
    CreatedAt     time.Time                 // org.opencontainers.image.created
    IndexedAt     time.Time                 // when ingest or reindex last wrote or saw the row
}
type JournalState struct {                  // a row of journal_state: the fold of a package's entries
    PackageID  int64
    Kind       string                       // JournalDistTag or JournalDeprecated
    Key, Value string                       // a dist-tag and its version; a version and its message
    Seq        int64                        // the entry that set it
}
type Sighting struct {                      // a row of prune_sightings
    RepoID          int64
    Path            string                  // the OCI repository, as oci_repos.path
    Digest          digest.Digest           // the manifest
    FirstAt, LastAt time.Time               // the first answer since the row was cleared, the latest
}
type FetchClaim struct {                    // a row of fetch_claims
    ID, RepoID             int64
    Package, Tag           string           // the package path (Plugin.EscapeName), profile.Tag of version and variant
    Principal              string           // of the request whose miss started the fetch
    Replica                string           // host name and process id
    ClaimedAt, HeartbeatAt time.Time
}

var ErrNotFound, ErrConflict error
var ErrUnavailable error                    // the database did not answer: connect, 57P01/57P02/57P03/53300, class 08, timeouts 25P03/57P05/25P04, aborts 40P01/40001, a referrers lock's transaction it ended (#615); SQLite busy, cannot open
type TagConflictError struct {              // errors.Is(err, ErrConflict) is true; UpsertVersion, ClaimTag
    Tag      string
    Existing digest.Digest
}
type ReindexConflictError struct {          // errors.Is(err, ErrConflict) is true; StartReindexRun
    Running ReindexRun                      // the live run that overlaps: replica, StartedAt
}
type ReplicaConflictError struct {          // errors.Is(err, ErrConflict) is true; StartReplica on SQLite
    Live Replica                            // the replica that serves on the database
}

// PackageAt is GetPackageByNameOCI at the package path pkgPath (Plugin.EscapeName), or at each of
// its earlier forms, of before ADR 0017 and its amendment (profile.EarlierPackagePaths,
// manifest-profile.md §4.3): a package keeps the OCI repository it was first written to.
// ErrNotFound when none is.
func PackageAt(ctx context.Context, r Reader, repoID int64, pkgPath string) (*Package, error)

// WithRepeatableRead returns ctx for a Store.Tx that does not lose a concurrent update (#791):
// fn reads one snapshot, and when a row it writes was changed by a transaction that committed
// after that snapshot, the database aborts the transaction and Tx runs fn again from the start,
// up to four times in all; the last error wraps ErrUnavailable. fn must start from what it
// reads on its Writer and leave nothing outside the transaction. Postgres and YugabyteDB:
// REPEATABLE READ, run again on 40001 and 40P01; SQLite serializes its writes, and fn runs
// once. Without it Tx is read committed on Postgres and runs fn once. The update of a repo
// uses it. IsRepeatableRead reports it, for the implementations of Tx.
func WithRepeatableRead(ctx context.Context) context.Context
func IsRepeatableRead(ctx context.Context) bool

// Migrations: migrations/<dialect>/NNNN_<name>.sql, embedded (index-schema.md, Migrations).
const DialectSQLite, DialectPostgres = "sqlite", "postgres"
func MigrationVersions(dialect string) ([]int, error)                 // embedded, ascending
// ApplyMigrations applies the pending migrations, each in a transaction under the migration
// lock that records it in schema_migrations, and reports those it applied (empty: current).
// It refuses a schema that requires a newer binary (ErrSchemaTooNew) and, unless
// opts.Breaking, a pending migration that requires more than the schema has
// (ErrBreakingMigration), applying nothing (index-schema.md, Upgrades).
func ApplyMigrations(ctx context.Context, db *sql.DB, dialect string, opts MigrateOptions) (MigrationReport, error)
type MigrateOptions struct{ Breaking bool }
type MigrationReport struct {
    Applied []int                           // applied by this run, ascending
    Version int                             // newest in schema_migrations after the run
    Known   int                             // newest embedded; Version > Known: a newer schema it runs on
}
var ErrSchemaTooNew, ErrBreakingMigration error
func AppliedMigrations(ctx context.Context, db *sql.DB) ([]int, error) // from schema_migrations

// Drivers (internal/index/sqlite, internal/index/postgres): *Store implements index.Store and
// adds Ping(ctx) error, ApplyMigrations(ctx, MigrateOptions) (MigrationReport, error),
// AppliedMigrations(ctx) ([]int, error).
func Open(ctx context.Context, dsn string) (*Store, error)            // does not migrate
```

Both engines (SQLite, Postgres) pass the same conformance test suite in `internal/index/storetest`.

### Reindex (`internal/index/reindex`)

```go
type Options struct {
    Index       index.Store
    Providers   provider.Set
    Inspector   Inspector                     // *ingest.Pipeline
    Repairer    Repairer                      // *ingest.Pipeline; nil: a run writes nothing to the registry
    Audit       audit.Writer
    Now         func() time.Time              // nil: time.Now
    Log         *slog.Logger                  // nil: slog.Default()
    Concurrency int                           // manifest reads at once per registry; zero: 8
    ClaimTTL    time.Duration                 // stale tag claims are deleted; zero: ingest's default
    GraceAge    time.Duration                 // reconcile.graceAge: prune keeps a younger row; zero: 1 h
}
type Inspector interface {
    Inspect(ctx context.Context, repo index.Repo, pkgPath, tag string, m ocispec.Manifest, raw []byte, o ingest.InspectOptions) (*ingest.Rows, error)
}
// Repairer pushes a subject's sha256-<hex> index again with the referrers it lacks, through the
// one write path (#628); found maps the digest of each manifest a run read (under a tag, or
// listed by a sha256-<hex> index) to the package paths of its OCI repositories.
type Repairer interface {
    RepairReferrers(ctx context.Context, repo index.Repo, found map[digest.Digest][]string, principal auth.Principal) ([]ingest.ReferrersRepair, error)
}
func New(o Options) *Worker

type Request struct {
    Repo      *index.Repo                     // nil: every repo
    Prune     bool                            // sweep rows not seen (the CLI's default for a run of every repo)
    Verify    bool                            // read and check every manifest, known digests too
    Source    string                          // cli, api, reconcile
    Principal string
    // Repositories: OCI repositories of Repo read besides those the registry lists, the first
    // attach of a prefix on a registry without a catalog (#37); one with a tag is recorded in
    // oci_repos. Names below the instance prefix; they need Repo.
    Repositories []string
}
// Start takes the lock (StartReindexRun) and writes repo.reindex; *index.ReindexConflictError
// when a run overlaps. Run reads the registry, beats, and finishes the run whatever happens; its
// error is the run's own (Report.Repos[].Errors hold the manifests that failed). It writes
// nothing to the registry itself: a run of the reconcile loop or with Verify, on a registry
// without the referrers API, asks ingest to repair the repo's sha256-<hex> indexes
// (Repairer, once per repo after its tags are read) and reports what ingest pushed
// (RepoReport.RepairedReferrers) and what failed (Errors).
func (w *Worker) Start(ctx context.Context, req Request) (*index.ReindexRun, error)
func (w *Worker) Run(ctx context.Context, run *index.ReindexRun, req Request) (*Report, error)
// Reconcile runs Start and Run for every repo with prune every interval until ctx ends (#30):
// the first after a random part of it, a tick that overlaps a run elsewhere skipped; its runs
// write no repo.reindex but one audit row reconcile per repo they changed.
func (w *Worker) Reconcile(ctx context.Context, interval time.Duration)
const SourceReconcile = "reconcile"

type Report struct {
    Repos []RepoReport `json:"repos"`
}
type RepoReport struct {
    Repo            string    `json:"repo"`            // <format>/<name>
    OCIRepositories int       `json:"ociRepositories"`
    Manifests       int       `json:"manifests"`       // tags read
    Added           int       `json:"added"`
    Updated         int       `json:"updated"`         // read again (verify, degraded) and written
    Unchanged       int       `json:"unchanged"`
    Removed         int       `json:"removed"`
    Pending         int       `json:"pending"`         // answered MANIFEST_UNKNOWN, kept until a second answer and the grace age
    Degraded        int       `json:"degraded"`
    Referrers       int       `json:"referrers"`
    Foreign         []Finding `json:"foreign"`
    OrphanReferrers []Finding `json:"orphanReferrers"`
    OrphanJournal   []Finding `json:"orphanJournal"`   // npm journal entries of a repository without versions
    Drift           []Drift   `json:"drift"`
    Split           []Split   `json:"split"`           // packages at two forms of their OCI name, one per other form
    RepositoryUnknown []RepositoryUnknown `json:"repositoryUnknown"` // versions whose OCI repository the registry does not know
    RepairedReferrers []RepairedIndex `json:"repairedReferrers"` // sha256-<hex> indexes ingest pushed again (#628)
    Errors          []Finding `json:"errors"`
    Truncated       bool      `json:"truncated"`       // a list stopped at 1 000 entries
}
type RepairedIndex struct {                   // guarantees.md INV-35; #628
    Repository string          `json:"repository"` // the subject's OCI repository, below the repo's base
    Subject    digest.Digest   `json:"subject"`
    Index      digest.Digest   `json:"index"`      // the index pushed
    Added      []digest.Digest `json:"added"`      // the referrers it lacked, in the order of their pushes
}
type Finding struct {
    Repository   string        `json:"repository"`    // OCI repository below the repo's base
    Tag          string        `json:"tag,omitempty"`
    Digest       digest.Digest `json:"digest,omitempty"`
    MediaType    string        `json:"mediaType,omitempty"`
    ArtifactType string        `json:"artifactType,omitempty"`
    Subject      digest.Digest `json:"subject,omitempty"`
    Error        string        `json:"error,omitempty"`
}
type Drift struct {
    Package  string        `json:"package"`
    Tag      string        `json:"tag"`
    Index    digest.Digest `json:"index"`
    Registry digest.Digest `json:"registry"`
}
type Split struct {                           // index-schema.md, Invariants; ADR 0017
    Package     string   `json:"package"`
    Indexed     string   `json:"indexed"`     // the package's name_oci, below the repo's base
    IndexedTags []string `json:"indexedTags"` // the tags of its versions in the index
    Other       string   `json:"other"`       // another form of it, below the repo's base
    OtherTags   []string `json:"otherTags"`   // the tags of the profile manifests there, not indexed
}
type RepositoryUnknown struct {               // index-schema.md, Invariants; #567
    Repository string        `json:"repository"` // the OCI repository, below the repo's base
    Package    string        `json:"package"`
    Version    string        `json:"version"`
    Variant    string        `json:"variant,omitempty"`
    Tag        string        `json:"tag"`
    Digest     digest.Digest `json:"digest"`
    Delete     string        `json:"delete"`     // DELETE /api/v1/repos/<format>/<name>/packages/<package>/versions/<version>[?variant=]
}
```

### Ingest (`internal/ingest`)

```go
type Options struct {
    Providers provider.Set
    Index     index.Store
    Plugins   func(name string) (format.Plugin, bool) // nil: format.Lookup
    Audit     audit.Writer
    Now       func() time.Time                         // nil: time.Now
    Log       *slog.Logger                             // nil: slog.Default()
    ClaimTTL  time.Duration                            // zero: 10 minutes (ingest.claimTTL)
    SessionTimeout time.Duration                       // zero: 30 minutes after a session's last request; a repo's maven.sessionTimeout wins
    VisibilityTimeout time.Duration                    // the read-back of a push (step 6 of Ingest), twice for a referrer, raised by a registry's consistencyBound; zero: 30 s (ingest.visibilityTimeout)
}
// New returns a Pipeline, which implements format.Publisher (Upload, UploadStream, Delete,
// Journal) and format.Sessions.
func New(o Options) *Pipeline

// Ingest stores a manifest the facade received for <format>/<repo>/<pkgPath>:<tag>; raw defines
// the digest. Errors wrap ErrInvalid (profile, plugin, missing blobs, a lajstrom.origin.* key,
// which only Upload with an Origin writes) or ErrConflict (tag, or the package path of another
// spelling). Inspect reads the lajstrom.origin.* keys as any annotation. A push the registry
// does not serve within the visibility timeout is a *NotVisibleError, from Ingest, Upload,
// PutReferrer and Journal alike.
func (p *Pipeline) Ingest(ctx context.Context, repo index.Repo, pkgPath, tag string, m ocispec.Manifest, raw []byte, principal auth.Principal) (*Result, error)

// NotVisibleError names what of a push the registry does not serve yet; it wraps a
// *provider.UnavailableError of 30 s (503 with Retry-After). Missing names the same in words:
// "the manifest", "the tag <tag>", "the blob <digest>", "the referrers of <digest>".
type NotVisibleError struct {
    Name     string          // the OCI name of the push, without the registry's instance prefix
    Digest   digest.Digest   // the manifest
    Manifest bool            // the manifest is not served by its digest
    Tag      string          // the tag does not name the manifest yet; "" when it does or there is none
    Blobs    []digest.Digest // the blobs not served, sorted
    Subject  digest.Digest   // a referrer's subject whose referrers do not list it yet
    Waited   time.Duration
}
func (e *NotVisibleError) Missing() []string
// Upload is the frontend path: Plugin.Build → push missing blobs → store as Ingest does. With
// up.Origin (the proxy fetcher, ADR 0013) it sets lajstrom.origin.url and lajstrom.origin.checksum
// on the built manifest and removes lajstrom.uploaded-by, and the audit row is proxy.fetch with
// details {"upstream", "url", "checksum", "size"}, size the sum of the file layers'; without one,
// a built manifest with a lajstrom.origin.* key is ErrInvalid. Upload, Delete and Journal made
// with a context of format.WithVia (a write through a group, a fetch of a group's proxy member)
// add details.via, <format>/<group>, to their audit rows (rbac.md §7); nothing else changes.
func (p *Pipeline) Upload(ctx context.Context, repo index.Repo, up format.Upload, principal auth.Principal) (*format.Published, error)

// PutReferrer stores a referrer (a signature, an SBOM, an attestation: profile.IsReferrer, or a
// manifest with a subject on an oci repo) as it came, and indexes nothing. On a managed repo the
// subject must be a manifest of the repository, the blobs in the backend, a tag cosign's
// (ErrInvalid otherwise). On a managed repo the push runs under Store.WithReferrersLock of the
// subject, with Capabilities.Referrers or without, and the subject is resolved again under it
// (ErrInvalid when it was deleted meanwhile, #806); on an oci repo without
// Capabilities.Referrers. One audit event: push, Subject the subject's digest,
// Details {"kind", "artifactType"}. Approved by the maintainer on 2026-11-09 (#28).
func (p *Pipeline) PutReferrer(ctx context.Context, repo index.Repo, r Referrer, principal auth.Principal) (digest.Digest, error)
type Referrer struct {
    Path     string             // the package path below the repo
    Tag      string             // "" for a push by digest; on a managed repo cosign's legacy tag only
    Desc     ocispec.Descriptor // media type, digest and size of Manifest
    Manifest []byte             // as received
    Subject  digest.Digest      // its subject, or the manifest cosign's tag names
}

// Delete deletes the manifest that del.Reference (a tag or a digest) names in the OCI repository
// of del.Path, what refers to it, and what the index holds of it (http-routing.md §2, DELETE;
// #61). Capabilities.DeleteManifest false is provider.ErrUnsupported and changes nothing, but
// for a version whose OCI repository the backend does not know (a 404 without MANIFEST_UNKNOWN
// for the manifest and for the tag list): its rows go, and nothing is written to the backend
// (#692). On a
// managed repo a reference that names a version (its tag, or its digest in the package) deletes
// the version; anything else is resolved in the backend. A journal tag (_j…) of an npm package
// is deleted under the package's journal lock, not its tag lock (manifest-profile.md §8, the
// unpublish of a package), and the index transaction deletes its journal row, when the package
// still has one, and folds the package's state again from the entries left. The referrers go
// first, each with its own before it: cosign's legacy sha256-<hex>.sig, .att and .sbom, the tag-schema index
// sha256-<hex>, the referrers the backend lists; without the referrers API under
// Store.WithReferrersLock of the subject. The first request that changes anything is a plain
// delete, so a backend that refuses deletes changes nothing (a referrer deleted on its own: its
// subject's sha256-<hex> index goes first and is pushed again without it). Then the manifest, then one index
// transaction: the version's rows, its package's when it was the last, index_cache of the repo,
// the audit row (delete, Details {"referrers": n}). On an oci repo a delete by tag of a manifest
// that other tags also name is refused before any request. With del.Condition it is a
// compare-and-delete of a version (format.Condition), checked under the locks: a failed one
// deletes nothing and writes no audit row. Errors wrap provider.ErrUnsupported,
// index.ErrNotFound, index.ErrConflict (other tags name the manifest; the error's OtherTags()
// []string lists them, #284), provider.ErrConflict (the backend refused this one delete),
// provider.ErrUnavailable (the registry did not take a referrer's subject index again, and the
// referrer stays), or a *format.RefusedError (a condition failed).
func (p *Pipeline) Delete(ctx context.Context, repo index.Repo, del format.Deletion, principal auth.Principal) (*format.Deleted, error)

// RepairReferrers is how reindex repairs the registry (#628; guarantees.md INV-35): on a backend
// without Capabilities.Referrers, it pushes again the sha256-<hex> index of each subject of found
// (manifest digest → package paths of the OCI repositories a run found it in) that lacks
// referrers the audit log has as pushed to repo (the push rows whose subject is a digest, read
// once) and the registry still has with that subject, or that is missing while there are such:
// read again under Store.WithReferrersLock of the subject, which must still be in the
// repository, then pushed with them after those it lists, as Delete's repair does. One audit
// event per index pushed: referrers.repair by principal, Subject the subject's digest, Digest the
// index's, Details {"path", "added"}, pending before the push and completed after it. A backend
// with the API, or an Options.Audit writer, repairs nothing. One entry per index found lacking:
// pushed, or Err (a read or the push failed, the index is as it was); the error is the audit
// log's or the capabilities'.
func (p *Pipeline) RepairReferrers(ctx context.Context, repo index.Repo, found map[digest.Digest][]string, principal auth.Principal) ([]ReferrersRepair, error)
type ReferrersRepair struct {
    Path    string                          // the subject's OCI repository, below the repo's base
    Subject digest.Digest
    Index   digest.Digest                   // the index pushed; "" with Err
    Added   []digest.Digest                 // the referrers it lacked, in the order of their pushes
    Err     error
}

// MirrorManifest and MirrorBlob are the write path of an oci proxy repo (ADR 0018,
// proxy-and-group.md §11.5): what it fetched, kept as it came. MirrorManifest pushes the
// manifest or index under its keep-tag (profile.KeepTag), and with Tag the upstream's tag, both
// under Store.WithReferrersLock of its digest; a tag the backend refuses to move
// (provider.ErrConflict) leaves the manifest held and is the error, wrapping ErrConflict. Then
// the barrier, oci_repos, and the audit row proxy.fetch (Details {"upstream", "ref", "tag"}),
// pending before the push. MirrorBlob streams r into an upload session, hashed, and commits it
// only when the bytes have desc's size and digest (else ErrInvalid), then reads it back; no row.
// MirrorMount mounts the blob desc into path from the path from of the same repo (POST
// ?mount=&from=, no byte moved) and reads it back; false, and the session cancelled, when the
// backend opened one instead (202); no row (#876). All three refuse a repo that is not an oci
// proxy (ErrInvalid).
func (p *Pipeline) MirrorManifest(ctx context.Context, repo index.Repo, m Mirror, principal auth.Principal) error
func (p *Pipeline) MirrorBlob(ctx context.Context, repo index.Repo, path string, desc ocispec.Descriptor, r io.Reader) error
func (p *Pipeline) MirrorMount(ctx context.Context, repo index.Repo, path, from string, desc ocispec.Descriptor) (mounted bool, err error)
type Mirror struct {
    Path     string             // the upstream's repository below the repo
    Desc     ocispec.Descriptor // of Manifest, its exact bytes
    Manifest []byte
    Tag      string             // the upstream's tag to push or move; "" for none
    Upstream string             // upstreams[].name, for the audit row
}

// Inspect runs the steps of Ingest that only read (1 to 3) on a manifest found in the registry
// at <repo base>/<pkgPath>:<tag> and returns the rows step 7 writes; nothing is pushed or
// claimed, and a missing blob is ErrInvalid. With AllowDegraded, a metadata layer that is missing
// or unreadable gives rows from the annotations alone (Version.Degraded, no plugin Validate);
// any other failure is the error Ingest would return.
func (p *Pipeline) Inspect(ctx context.Context, repo index.Repo, pkgPath, tag string, m ocispec.Manifest, raw []byte, o InspectOptions) (*Rows, error)
type InspectOptions struct {
    AllowDegraded bool
}
// Rows are the derived rows of one stored manifest: what Ingest writes after the registry
// accepted it, and what reindex writes for a manifest it finds.
type Rows struct {
    Package index.Package                   // RepoID, Name, NameOCI
    Version index.Version                   // PackageID filled by WriteRows
    Files   []index.File
    OCIRepo string                          // the OCI repository without the instance prefix
}
// WriteRows writes rows on w with IndexedAt at: package, version, files, OCI repository. It does
// not touch index_cache, claims or audit; *index.TagConflictError when the tag holds another
// digest.
func WriteRows(ctx context.Context, w index.Writer, rows *Rows, at time.Time) error

type Result struct {
    Digest   digest.Digest
    Tag      string
    RepoPath string
    Entry    format.Entry
    Existed  bool                           // idempotent repeat: nothing was written
}

// Pipeline implements format.Sessions (Open, Add, Files, Refuse, Commit, Abort). The files
// column of a session is {"name", "version", "files": [SessionFile…], "refused", "committing"};
// Commit marks the session committing before it publishes, so that one Commit runs at a time,
// and a mark older than ClaimTTL is a Commit that died. ExpireSessions marks the open sessions
// of every kind whose time is up; serve calls it every minute, and nothing depends on it.
func (p *Pipeline) ExpireSessions(ctx context.Context) (int, error)

var ErrInvalid = format.ErrInvalid
var ErrConflict = index.ErrConflict
var ErrNotImplemented error                 // unused since #12
```

### Auth and audit (`internal/auth`, `internal/audit`)

```go
type Kind string                            // KindUser, KindToken, KindGroup, KindAnonymous
type Principal struct {
    Kind Kind
    Name string                             // a user's uid (index.NewUID), a group's name, a deploy token's id; empty for anonymous
}
func (p Principal) String() string          // user:<uid>, token:<id>, group:<name>, anonymous; user:<name> parses too, as input
func ParsePrincipal(s string) (Principal, error) // errors wrap ErrInvalidPrincipal
var Anonymous Principal
var ErrInvalidPrincipal error

// The request's principal: the authentication middleware of internal/server stores it from
// Basic credentials and token secrets on every route (#43), the facade from the bearer of /v2
// (#26); a request without credentials is anonymous.
func WithPrincipal(ctx context.Context, p Principal) context.Context
func PrincipalFrom(ctx context.Context) Principal // Anonymous when nothing was stored

type Action string                          // ActionRead, ActionWrite, ActionDelete, ActionManage
type Authorizer interface {
    Allowed(ctx context.Context, p Principal, a Action, format, repo string) (bool, error)
}

// package audit
type Writer interface {
    Write(ctx context.Context, ev index.AuditEvent) error // zero ev.At means now
}

// package auth/oidc (#69, rbac.md §8): the relying party of auth.oidc. The login flows of
// internal/auth use the interface only, so that a library may implement it (decision on #69).
type Provider interface {
    // AuthCodeURL is the IdP's authorization endpoint with the code flow's parameters: the
    // state, the nonce and the S256 challenge of the PKCE verifier. Errors wrap ErrUnavailable
    // when the discovery document cannot be read.
    AuthCodeURL(ctx context.Context, state, nonce, challenge string) (string, error)
    // Exchange redeems code with the verifier at the token endpoint and verifies the ID token
    // against nonce (rbac.md §8). Errors wrap ErrRefused (the IdP refused the code, or the ID
    // token or its claims do not verify; the reason is for the log, never the client) or
    // ErrUnavailable (the IdP does not answer).
    Exchange(ctx context.Context, code, verifier, nonce string) (*Identity, error)
}
type Identity struct {
    Issuer, Subject string                  // iss and sub of the ID token
    Username        string                  // the claim auth.oidc.usernameClaim, unchecked
    Groups          []string                // the claim auth.oidc.groupsClaim, the names of rbac.md §8 kept
}
var ErrRefused, ErrUnavailable error
```

The group of a principal is `auth.KindGroup`; the credential of an OIDC login's request is
`auth.CredOIDC`, which may mint a personal access token as a password may (`MayCreatePAT`).

### Decisions

Where the merged shapes differ from the first sketch of this section, and why:

- **Publisher breaks the format/ingest cycle.** `Deps.Ingest` is the `format.Publisher`
  interface, implemented by `*ingest.Pipeline`, so `format` does not import `ingest`. Its
  `Upload` returns `*format.Published`, and the principal is a parameter, not a field of
  `Upload`.
- **Providers is a `provider.Set` keyed by registry name** (`Deps.Providers`,
  `ingest.Options.Providers`): a repo chooses its registry with `repos[].registry`.
- **Provider names carry no instance prefix.** Callers pass `<format>/<repo>/<package path>`;
  the provider adds `registries[].prefix` and strips it from listings
  ([http-routing.md](http-routing.md) §2).
- **Ingest takes the pushed package path and tag**, so it can reject a manifest pushed to a
  path or tag that does not follow from its annotations.
- **Repository embeds `registry.ReferencePusher`**: ingest puts manifest and tag in one PUT.
  `Delete` comes from `content.Deleter`.
- **UploadedFile is re-openable** in `Upload`: Build reads the file to parse and digest it,
  ingest reads it again to push it. A publication session's file is read back from the
  registry. Artifact bytes live only in the registry.
- **A streamed upload has its own entry point**
  ([ADR 0014](../adr/0014-uploads-stream-into-the-registry.md)). Every format that a client
  uploads to is a `Streamer` (raw, apt, maven, pypi, npm), and the proxy fetch streams the
  upstream's file through it too (#694); `Upload` stays for the files of a publication session,
  which it reads back from the registry.
  `UploadStream` takes the package name from the frontend, because a session belongs to an OCI
  repository. The file reaches `Build` without `Open`, with its checksums and its inspector.
  Ingest holds the session in the target and commits it between the backend's tag check and
  the manifest's `PUT`. A refused upload, and the idempotent repeat, cancel it.
  `Validate` cannot fetch the streamed blob, which is not committed yet; a plugin whose
  `Validate` reads its file takes what it needs from its inspector (`format.ErrStreamed`). A
  `Build` whose inspector did not hold all it needs, a wheel or a `.zip` sdist it could not
  follow, returns `format.ErrReadBack`: ingest commits the session and calls `Build` again
  with an `Open` that reads the file back from the registry by range requests (ADR 0014 step
  5).
- **The provider chunks a streamed blob, not ingest.** `BlobWriter` sends chunks of known size
  with `Content-Range`, which every registry measured takes after the first write (the
  streamed `PATCH` is refused there by Gitea and JFrog). It holds one chunk in memory, 8 MiB in
  the generic provider by default. That size is a choice, not a measurement: larger than the
  5 MiB smallest part of an S3 multipart upload, and 64 MiB for eight uploads at once. GHCR
  takes at most 4 MiB a `PATCH` ([ghcr.md](../registries/ghcr.md)), which its vendor entry
  says; `registries[].uploads.chunkSize` overrides both. A blob that fits one chunk is one
  monolithic `PUT`, and one cancelled before its first chunk never opens a session.
- **Scopes and Attrs are typed** (`Token.Scopes []string`, `Version.Attrs` and `File.Attrs`
  `map[string]string`); the store marshals them. **`Cache`, not `IndexCache`**, because
  `index.IndexCache` stutters.
- **`ListAudit(AuditFilter)`** is on the Reader for `GET /api/v1/audit`, and `FindFiles`
  returns `FileMatch` (file with its version and package) for lookups by file name.
- **Plugin names are checked in `format`.** The grammar `^[a-z0-9]+(-[a-z0-9]+)*$` and the
  reserved first segments (`Reserved`, moved from `server`) live in `format`; `Register` panics
  on a name `CheckName` rejects, and `server.New` runs the same check for plugins passed
  without `Register`.
- **The Router resolves `{repo}` once per request and does not cache.** One `GetRepo` per
  request is the cost of a primary-key lookup; a cache across requests would keep serving a
  deleted repo.
- **The principal comes from the context, not from a header parser.** The authentication
  middleware of `internal/server` verifies the credentials and stores the principal with
  `auth.WithPrincipal` (#43), the facade the bearer's (#26); a request without credentials is
  anonymous, and a handler never parses a header itself: credentials parsed without being
  verified would be a hole. A nil
  `Deps.Auth` allows everything the credential allows, for the tests; `serve` always sets
  `auth.RBAC` (rbac.md §3).
- **A denial does not reveal whether the repo exists** (rbac.md §3). `Authorize` answers 401
  to anonymous, 404 to a principal that cannot read the repo, and 403 only to one that can read
  it but lacks the action; the repo middleware answers a missing repo with the same 401 to
  anonymous when `Deps.Auth` is set.
- **One error table per package.** `format.StatusOf` maps the sentinels of `format`, `index`
  and `provider` for every frontend; a plugin wraps them and never picks a status itself.
- **One SQL implementation for both engines.** `internal/index/internal/sqlstore` implements
  `index.Store` over `database/sql`; the drivers only open the database and name a dialect:
  placeholders (`?` or `$n`), time encoding (SQLite: UTC RFC 3339 text with six fractional
  digits, so text order is time order; Postgres: `timestamptz`), the driver's constraint
  errors, and the DDL of the migration files. Timestamps are truncated to microseconds and
  stored in UTC, so both engines return exactly what was written. Postgres text columns are
  `COLLATE "C"`, so order and comparison are SQLite's byte order. Unique violations wrap
  `ErrConflict`; a row that refers to a missing row wraps `ErrNotFound`; a database that did
  not answer wraps `ErrUnavailable`: on Postgres a connection that could not be made or broke,
  and the SQLSTATEs of a server that ends or refuses connections (`57P01`, `57P02`, `57P03`,
  `53300`, class `08`) or ends a session on a timeout (`25P03`, `57P05`, `25P04`), and of a
  transaction it aborted that may pass when run again, a deadlock (`40P01`) or a conflict it
  could not serialize (`40001`) (#627), and a referrers lock whose transaction the database
  ended before the lock's function returned (#615); on SQLite `SQLITE_BUSY` after the busy
  timeout and `SQLITE_CANTOPEN` (#560). `UpsertVersion`
  resolves a race on a tag with `INSERT … ON CONFLICT DO NOTHING` and a second read, so the
  loser gets the `TagConflictError`. `serve` migrates on start under the same lock as
  `lajstrom migrate`. On YugabyteDB, which the Postgres driver tells by the `server_version`
  it reports, the dialect differs in one thing (#629): `UpsertPackage` takes
  `pg_advisory_xact_lock` on the package path before it inserts a package, because YugabyteDB
  takes the entries of the two unique indexes of `packages` in no fixed order and two writers
  of one new package deadlock on them.
- **A tag claim is the lock on a tag across replicas, in a table of its own** (#12). Ingest
  inserts a `tag_claims` row in its own transaction before the registry write, then reads the
  version of the tag; another digest there, or a fresh claim of another digest, is the 409, and
  the same digest is the idempotent path. The claims are keyed by the package path, and a claim
  on the package path, with the digest of the spelling, comes before the one on the tag (#244):
  two spellings of a name share an OCI repository, so a package of another spelling, or a fresh
  claim of another spelling, is the 409 before the manifest is pushed. Then `PushReference`; a
  registry error releases the claim on the tag. Then one transaction confirms: version, files,
  `index_cache`, audit, both claims released.
  A claim older than `Options.ClaimTTL` is taken over. The claim is not a `versions` row with a
  null `indexed_at`: derived rows stay rebuildable from manifests, readers of `versions` need no
  extra condition, `ResetDerived` cannot drop a claim mid-publish, and the claim has its own
  timestamp for the TTL. `Upload` treats the same file layers under the tag as the idempotent
  repeat (manifest-profile.md §4.2): two builds of the same files differ only by `created`, so
  a retried upload must not be a 409; the check runs before the blobs are pushed and again under
  the claim. `Ingest`, the facade path, keeps the OCI meaning: another manifest is the 409.
- **Signing is `Deps.Signer`, an interface of `format`** (#41). `internal/keys` implements it
  over the index and the master key; `format` and the plugins do not import `keys`. The apt
  routes sign the bytes of the `Release` in `index_cache` and store `InRelease` and
  `Release.gpg` beside it, with its `rendered_at` and a `signed_at`, in a transaction that
  first checks that this `Release` is still the cached one; the `Signer` runs outside the
  transaction of `CachedDocument`, because it reads the repo's keys and inserts the first one,
  which inside that transaction would wait for the connection or the write lock it holds
  (SQLite). A cached `InRelease` or `Release.gpg` is served only while `Signer.Current` says
  that its signatures are by the active key and by keys still in their grace; otherwise it is
  signed again. The end of a retired key's grace is a time, not a write, so no invalidation
  marks it (#577).
- **Proxy repos plug in through `format.Proxier`** (ADR 0013, #309). It is the sketch of
  proxy-and-group.md §10 without `MergeGroup`, which came with the groups of #311 (`Grouper`,
  below), and with
  three changes: `RenderProxy` reads the held versions through an `index.Reader`, as
  `RenderIndex` does, rather than taking them, so that it reads their files and metadata as it
  needs; an `UpstreamEntry` carries the `Params` that `Build` takes, so that the core builds the
  upload without knowing the format; an `ArtifactRef` names the document `Kind` that lists it.
  One kind names the `proxy_meta` row, the upstream request, the parse, the rendered document
  and its `index_cache` row, whose scope in a proxy repo is `Key(name)`. The handlers reach the
  core through `Deps.Proxy` and serve what it returns as they serve a hosted repo's.
- **A proxy's settings are JSON in `index`**, as `Policy` is: `Repo.Upstream`. `internal/proxy`
  parses and checks them, and the management API calls it.
- **A takeover of a stale fetch claim gives the claim a new id**, unlike a tag claim's: the
  fetch whose claim went stale finds no row to beat or to release, and cannot release its
  successor's.
- **A `proxy_meta` write does not invalidate on its own.** The core writes the row and drops
  the `index_cache` rows of the name in one `Tx`; a `304` is `TouchProxyMeta`, which drops
  nothing. Expired `absent` rows are swept per repo, since `negativeTTL` is a repo's.
- **A proxy forwards what it does not keep through the core** (#384). npm audit is a `POST`
  that only the upstream can answer, and the core's client is the one way to an upstream
  (its credentials, timeouts and SSRF rules), so `Proxy.Forward` sends a request the plugin
  builds field by field, `ForwardRequest`, not the client's `*http.Request`: no header of the
  client's reaches the upstream unless the plugin names it. The body is held in memory, at
  most 16 MiB, so that a retry and a `307` or `308` send it again; it goes decoded, since a
  plugin decodes it to bound it and a group's (#311) to read it. The answer streams back,
  bounded by `maxMetadataSize` rather than a key of its own. The upstream's error statuses are
  classified as a document's (proxy-and-group.md §6), its `400`, `413`, `415` and `422` being
  the client's `400`, and its error body is never passed on. A group forwards to its first
  proxy member, without the names its hosted member holds (#311; http-routing.md, npm, Audit).
- **Groups plug in through `format.Grouper`** (#311), a `Proxier` with `MergeGroup`, rather
  than a sixth method of `Proxier`: a format may have proxies before groups (#67, #68), and
  the implementers of `Proxier` keep compiling. `MergeGroup` takes the members' documents as
  they serve them, links under their own bases, and writes every link under the group's. The
  hosted member's document is its `index_cache` row of scope `Key(name)`, so no hook maps a name
  to a hosted scope; a format whose hosted scope differs adds one with its groups.
- **A proxy becomes hosted through `format.Adopter`** (#441), a `Proxier` with two methods.
  The core reads the kept document of each held package and needs of the plugin only which kind
  to read and the journal ops it means: a proxy's dist-tags are its upstream's, in a cache, and a
  hosted repo's are its journal, so the conversion writes them as ops through
  `Publisher.Journal`, as a client's `npm dist-tag` would, and nothing new reaches the registry
  by another path. `index.Store.ConvertRepo` is a method of its own, not a looser `UpdateRepo`:
  it is the one change of a kind, it checks the fetch claims in the transaction that flips it,
  and `UpdateRepo` keeps refusing every other change of a kind.
- **A prefetch plugs in through `format.Prefetcher`** (#441), a `Proxier` with two methods,
  rather than more methods of `Proxier`: the core reads a name's entries as a miss does, and
  needs of the plugin only which documents to read (the one that lists the entries first, and
  those a client asks for, so that the repo serves them once offline) and which versions the
  pointers name, the default of a prefetch. A format without it has no prefetch; its proxies serve misses as before.
- **`format.Proxy` serves groups with `Document` and `File`.** A handler asks it for a proxy's
  document or a group's alike. `File` keeps its `*index.FileMatch`, whose `Package.RepoID`
  names the member that holds the file: the handler looks that repo up (`GetRepoByID`, one
  primary-key read) to reach its registry, as a group has none.
- **Members are rows, `group_members`, not a list in `repos.group`.** The foreign key
  `ON DELETE RESTRICT` keeps a member from being deleted by any replica, without a check that
  races with a concurrent change of the group; `ListGroupsOf` is an index scan; and `repos.group`
  keeps what `manage` changes (`merge`) apart from what only the admin changes (members).
- **A write through a group is the member's, found by `format.WriteTarget`**, a function of
  `format` over the index rather than a method of the core: it is the same for every format,
  and it replaces the check each handler made of a proxy's kind. The group reaches the audit rows of ingest and of the
  core through the context (`WithVia`), not as a field of `Upload`, `Deletion` and `JournalOp`:
  one marker, read where the rows are written. `Authorize` reads it too, so that a principal
  that read the group and is denied on its hosted member gets `403` naming the member.
- **The npm journal has store methods of its own** (ADR 0012, #307). `ListJournal` and
  `ListJournalState` read one package: the journal writer reads the last `seq` and the state
  under the package's journal lock, a packument reads both for its `dist-tags`, `deprecated`,
  `_rev` and `time.modified`, and reindex reads a package's entries once per OCI repository and
  compares them with the `_j` tags it lists, so it needs neither a lookup by tag nor a list of
  the repo's unseen entries. `ReplaceJournalState` writes the whole fold, not a change: the
  writer folds its entries over the state it read, reindex folds every entry, and both write
  the result the same way. `UpsertJournal` keeps the append-only rule of `UpsertVersion`: an
  entry under its `seq` is never rewritten. The validation of an entry and the fold are pure
  functions of `internal/oci/profile`, which ingest and reindex share.
- **Keyset pages are methods beside the whole lists** (#552). `ListPackagesAfter` and
  `ListVersionPage` are added, and `ListPackages` and `ListVersions` keep their signatures: about
  25 callers (the renderers, reindex, ingest) need every row, and a limit that they would all
  pass as "none" is churn for nothing. A version page returns rows, as `ListVersions` does, and
  not versions grouped: the API groups them, with their pointers, as before. Its key is a
  `VersionKey` and not the API's cursor text, which stays the API's.

## 3. Approved dependencies

Everything else needs a maintainer decision in the pull request (CONTRIBUTING.md, dependency
license policy). Prefer the standard library.

| Need | Dependency | License |
| --- | --- | --- |
| OCI client | `oras.land/oras-go/v2`, `github.com/opencontainers/image-spec`, `github.com/opencontainers/go-digest` | Apache-2.0 |
| SQLite | `modernc.org/sqlite` (no cgo) | BSD-3 |
| Postgres | `github.com/jackc/pgx/v5` (`database/sql` via `stdlib`) | MIT |
| Migrations | embedded `*.sql` files, tiny runner in `internal/index` (no library) | |
| Config | `gopkg.in/yaml.v3` | MIT / Apache-2.0 |
| CLI | `github.com/spf13/cobra` | Apache-2.0 |
| JWT (bearer tokens) | `github.com/golang-jwt/jwt/v5` | MIT |
| Passwords | `golang.org/x/crypto` (argon2id), `golang.org/x/term` (the CLI's password prompt) | BSD-3 |
| OpenPGP (apt signing) | `github.com/ProtonMail/go-crypto` | BSD-3 |
| Compression | `github.com/klauspost/compress` (zstd, gzip), `github.com/ulikunitz/xz` | BSD-3 / Apache-2.0 |
| AWS (ECR adapter) | `github.com/aws/aws-sdk-go-v2` (`ecr`, and `config` for the credential chain), `github.com/aws/smithy-go` (its API errors) | Apache-2.0 |
| Metrics | `github.com/prometheus/client_golang`; `github.com/prometheus/client_model` in tests | Apache-2.0 |
| Tests | `testing` + `github.com/google/go-cmp`; `github.com/stretchr/testify` acceptable | BSD-3 / MIT |

Not approved without discussion: ORMs, chi/gin/echo, testcontainers (we use compose and CI
service containers), any GPL/LGPL/AGPL/SSPL/BSL dependency, cgo.

## 4. Testing strategy

| Level | Where | Runs | How |
| --- | --- | --- | --- |
| Unit | next to the code | `go test ./...`, always | pure functions, table tests, fixtures built in-process (`internal/testutil` builds a `.deb`, a `.whl`, a `.jar` + POM in memory; no vendored binaries from distributions) |
| Store conformance | `internal/index/storetest` | always for SQLite; Postgres when `LAJSTROM_TEST_POSTGRES_DSN` is set | same suite, both engines |
| Integration | `//go:build integration` | `make test-integration`; CI job with service containers | real registry (Zot and distribution) via `LAJSTROM_TEST_REGISTRY=host:port`; provider, facade, ingest, reindex |
| End-to-end | `test/e2e/` | `make e2e`; CI job | `bin/lajstrom` served on the host against the dev stack's registry, and real clients: `oras`, `pip`/`twine`, `apt-get`, `mvn`, `npm`, `cosign`, most in containers. One script per scenario, exit code = pass/fail |
| Fuzz | `FuzzEscape`, `FuzzParseControl`, … | the seed corpus in `make test`; `make fuzz` or `go test -fuzz` locally; each target for 10 minutes in the weekly Fuzz workflow | parsers and escapers |

Integration and e2e tests must be runnable locally with `make dev-up` first. Flaky tests are
disabled with an issue, not retried. [docs/testing.md](../testing.md) has three more levels,
OCI conformance, upgrade and load, and the commands, needs and CI jobs of all eight.

## 5. Make targets

`make` alone lists the targets (`make help`).

| Target | Does |
| --- | --- |
| `make build` | `go build ./...`, then `go build -trimpath -ldflags "-X …/internal/version.{Version,Commit,Date}=…" -o bin/lajstrom ./cmd/lajstrom` (from `git describe`, `git rev-parse`, `date -u`) |
| `make test` | `go test -race ./...`; it includes the check of guarantees.md (`tools/guarantees`) and the fuzz seed corpus |
| `make guarantees` | what each guarantee of guarantees.md proves, as a Markdown table |
| `make vet` | `go vet ./...` |
| `make lint` | golangci-lint at the version CI pins (`GOLANGCI_LINT_VERSION`, v2.15.0) |
| `make api-lint` | vacuum on `internal/api/openapi.yaml`, at the pinned version |
| `make vulncheck` | govulncheck |
| `make licenses` | go-licenses v1.6.0 against the allowlist of CONTRIBUTING.md |
| `make fmt-check` | fails when `gofmt -l .` lists a file |
| `make check` | fmt-check, build, vet, test, lint, api-lint, vulncheck, licenses: the CI jobs that need no stack (`build-test`, `lint`, `vulncheck`, `licenses`), plus gofmt, at the pinned tool versions; run before every pull request. `integration`, `e2e`, `snippet-scan` and `dco` run in CI only |
| `make fuzz` | each fuzz target for `FUZZTIME` (30s), or those of `FUZZ_TARGETS` |
| `make dev-up` / `make dev-down` / `make dev-ps` / `make dev-logs` | compose stack in `deploy/compose/compose.yaml` (issue #15); `PROFILES=` adds a profile (s3, s3-minio, gitlab, ha) |
| `make test-integration` | `go test -race -tags integration ./...` against the dev stack; needs `LAJSTROM_TEST_REGISTRY` |
| `make e2e` | `test/e2e/run.sh`: builds the binary, starts `serve` on a configuration of its own against the registry `$LAJSTROM_E2E_REGISTRY` (default the Zot of `make dev-up`) and runs every scenario of `test/e2e/`, or those of `E2E='raw oras'` (docs/testing.md) |
| `make load` | `test/load/run.sh`: 1 GiB through raw and `/v2` against the dev stack, the server's memory sampled; not part of e2e |
| `make registry-matrix` | `lajstrom registry probe` against the dev stack's registries (docs/registries/README.md) |
| `make run` | build, then `lajstrom serve --config deploy/compose/lajstrom.yaml` (the example file when that does not exist) |
| `make snapshot` / `make release-check` | the release without publishing or signing, in `dist/`; `goreleaser check` of `.goreleaser.yaml` |
| `make clean` | removes `bin/` and `dist/` |


## 6. Configuration (shape; every key in [docs/configuration.md](../configuration.md))

```yaml
server:
  listen: ":8080"
  externalURL: "https://art.example.com"   # the token realm, iss and aud; npm's dist.tarball
  shutdownTimeout: 30s                      # graceful shutdown bound
log:
  level: info                               # debug | info | warn | error
  format: text                              # text | json
database:
  driver: sqlite                            # sqlite | postgres
  dsn: "file:lajstrom.db"
registries:
  - name: local
    type: generic                           # generic | ecr
    url: "http://zot:5000"
    prefix: "art/"
    auth: { username: "lajstrom", passwordEnv: "ZOT_PASSWORD" }
auth:
  anonymousRead: false
  bootstrap:                                # the one-time token of the first admin (#440)
    tokenFile: /run/secrets/bootstrap-token # a token you made; empty: no token, a warning
    ttl: 24h
  oidc:                                     # OIDC login (#69, rbac.md §8); off while issuer is empty
    issuer: "https://idp.example.com/realms/main" # https; http only with server.insecureHTTP
    clientID: lajstrom
    clientSecretFile: /run/secrets/oidc-client-secret # env LAJSTROM_AUTH_OIDC_CLIENT_SECRET_FILE
    scopes: [openid, profile, email, groups] # openid is sent whatever this says
    usernameClaim: preferred_username       # the name of a user made at its first login
    groupsClaim: groups                     # empty: no groups
    tokenLifetime: 24h                      # the personal access token of a login; its groups count as long
keys:
  masterKeyEnv: LAJSTROM_MASTER_KEY         # encrypts the apt keys and the bearer-token key at rest
  tokenAlgorithm: ES256                     # ES256 | EdDSA; the bearer-token key (ADR 0010)
  algorithm: ed25519                        # ed25519 | rsa4096 (apt before 2.0)
  retireGrace: 720h                         # a retired key still signs and stays in key.asc this long
repos:                                      # optional declarative bootstrap: created when missing, never changed
  - { name: ubuntu-internal, format: apt, registry: local, anonymousRead: false }
```

The shape is a sketch: later keys and sections (`upstreams`, `ingest`, `reconcile`,
`server.tls`, …) are in configuration.md, which has every key.

Precedence: defaults < file < the nine `LAJSTROM_*` variables of configuration.md's Env column.
Secrets never go into the file: `passwordEnv` and `masterKeyEnv` name environment variables,
and `keys.masterKeyFile`, `upstreams[].auth.passwordFile` and `tokenFile` and
`auth.bootstrap.tokenFile` name files.

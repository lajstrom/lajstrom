# Spec: repository layout, package contracts, approved dependencies

- Status: normative for the MVP (2026-11-06). The Go interfaces in §2 are the shapes merged with
  the skeleton (issue #7, M0-01) and are contracts: changing an exported interface in
  `internal/{format,provider,index,ingest}` needs an issue labelled `contract` and a
  maintainer's approval.
- Why this exists: two people work in parallel with AI assistants. Agreed package boundaries
  and interfaces are what keeps their pull requests from colliding.

## 1. Layout

```
cmd/lajstrom/                 main; subcommands: serve, reindex, migrate, version, admin (user/token/repo/binding), healthcheck
internal/config/              config file (YAML) + env overrides; validation; example in deploy/
internal/server/              http.Server, routing (net/http ServeMux, Go 1.22 patterns), middleware:
                              request id, logging, metrics, auth, panic recovery; mounts formats, facade, api
internal/facade/              OCI distribution API (/v2): proxy, token endpoint, manifest PUT hook
internal/format/              Plugin interface, registry of plugins, shared helpers (Router, Deps)
internal/format/raw/          raw plugin: parse, routes, index
internal/format/pypi/         pypi plugin: wheel/sdist metadata, simple index, upload API
internal/format/deb/          deb/apt plugin: control parsing, Packages/Release rendering, GPG, pool routes
internal/format/maven/        maven plugin: coordinates, POM, sessions, maven-metadata.xml, checksums
internal/format/formattest/   test doubles: Fake plugin, partial index.Store; imported only by tests
internal/oci/profile/         media types, annotations, Escape/EscapeComponent, manifest build+validate (pure)
internal/ingest/              pipeline (validate → parse → push → index), publication sessions
internal/provider/            Provider interface, Capabilities, errors
internal/provider/generic/    oras-go based provider for distribution, Zot, Harbor, GHCR (+ small adapters)
internal/provider/ecr/        ECR adapter: auth token, CreateRepository, DescribeRepositories
internal/index/               Store interface, models, migrations (embedded SQL) and their runner
internal/index/reindex/       reindex worker: walk the registry, mark and sweep, runs, the report
internal/index/migrations/    <dialect>/NNNN_<name>.sql for sqlite and postgres, embedded; the same versions in both
internal/index/internal/sqlstore/ the SQL both engines share: index.Store over database/sql and a Dialect
internal/index/sqlite/        modernc.org/sqlite implementation
internal/index/postgres/      pgx implementation
internal/index/storetest/     conformance suite of index.Store, run by both engines; imported only by tests
internal/proxy/               proxy repos (ADR 0013): the settings, the upstream client, the metadata cache, fetch on miss
internal/auth/                principals, password hashing, PAT/deploy tokens, RBAC evaluation, bearer JWT
internal/api/                 /api/v1 management REST (repos, users, tokens, bindings), JSON
internal/keys/                server signing key, per-repo GPG keys (apt), encryption at rest
internal/audit/               audit log writer
internal/metrics/             the Prometheus collectors (fixed labels) and the handler of the metrics listener
internal/testutil/            registry/DB fixtures, fixture builders (make a .deb / .whl / .jar in-process)
internal/version/             build information (version, commit, date), set with -ldflags; /-/version
deploy/compose/               dev stack: zot, distribution, postgres; lajstrom.example.yaml
docs/                         architecture, specs, ADRs, plan, registries (spike results), quickstarts
tools/                        bootstrap scripts, dev helpers
```

Conventions:

- `internal/` only; nothing is a public Go API in v0.x.
- A format plugin never imports another format plugin. Plugins import `oci/profile`, `format`,
  `index` (read), `ingest` (write). Frontend HTTP handlers live inside the plugin package
  (`routes.go`); parsing lives in `parse.go` and is pure (no I/O beyond the reader passed in).
- Only `provider/*` talks to the backend registry. Only `index/*` talks to the database.
- Errors: wrap with `%w`, sentinel errors per package (`ErrNotFound`, `ErrConflict`,
  `ErrUnsupported`). HTTP handlers map sentinel errors to status codes in one place per package.
- Context everywhere; no global state except the plugin registry, and no state in process
  memory that outlives one request: several replicas serve the same registry and the same
  database, so anything that must be coordinated across requests (tag claims, upload sessions,
  background jobs) goes through the database, never through memory or local files.
- Logging with `log/slog`; metrics with Prometheus client; no other observability deps in the MVP.

## 2. Contracts

The exported shapes as merged with issue #7 (M0-01) and, for the format helpers, issue #11,
grouped by package. Signatures are exact; comments are shortened. The doc comments in the code
are the full contract (`go doc -all ./internal/<package>`).

### Format plugin (`internal/format`)

```go
// Plugin is one package format. Implementations are stateless; dependencies come in via Deps.
type Plugin interface {
    Name() string                           // first URL segment and lajstrom.format: raw, pypi, apt, maven
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

// Router takes ServeMux patterns relative to /<format>/, e.g. "GET /{repo}/simple/{project}/".
type Router interface {                     // implementation: NewRouter
    Handle(pattern string, h http.Handler)
    HandleFunc(pattern string, h http.HandlerFunc)
}

// Publisher is the write path as plugins see it; *ingest.Pipeline implements it.
type Publisher interface {
    Upload(ctx context.Context, repo index.Repo, up Upload, principal auth.Principal) (*Published, error)
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
// in a repo, held by one principal. Add pushes the file and records it with its checksums;
// Commit publishes the files through Upload, once at a time; a session whose time is up (30
// minutes after its last request) is dropped, never published in part.
type Sessions interface {
    Open(ctx context.Context, repo index.Repo, name, version string, principal auth.Principal) (*index.Session, error)
    Add(ctx context.Context, sessionID int64, name string, r io.Reader, size int64) (SessionFile, error)
    Files(ctx context.Context, sessionID int64) ([]SessionFile, error)
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
}
type UploadedFile struct {
    Name        string                      // maven: the path below /maven/<repo>/
    Size        int64                       // -1 when unknown
    ContentType string
    Open        func() (io.ReadCloser, error) // re-openable: Build reads it, ingest pushes it
    // Lowercase hex by md5, sha1, sha256, sha512, computed once as the bytes streamed in; nil
    // when none. All four and a known Size: the file is in the backend (a session pushed it,
    // #53), Build need not open it and leaves it out of Built.Blobs.
    Checksums   map[string]string
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
type IndexScope string                      // jammy/main/amd64, simple/requests, com.example/lib-core
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
}
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

// WriteError answers StatusOf(err) as text/plain (nosniff): err.Error() for 4xx, the status
// text for 5xx; a 401 carries WWW-Authenticate: Basic realm="lajstrom", a 405 Allow: GET, HEAD,
// a 503 of provider.ErrUnavailable or of an *UnavailableError Retry-After (SetRetryAfter).
func WriteError(w http.ResponseWriter, err error) int
func StatusOf(err error) int
// SetRetryAfter sets Retry-After from provider.RetryAfter(err), or from the *UnavailableError
// err wraps: whole seconds, rounded up, at least 1; nothing for an error without one.
// WriteError, the facade and the API call it.
func SetRetryAfter(h http.Header, err error)

// CachedDocument returns the index_cache row of (repo, scope, kind), or renders the scope with
// p.RenderIndex, stores every document (SHA256, RenderedAt) and returns the one of kind; lookup,
// render and store run in one st.Tx. index.ErrNotFound when no document of kind is rendered.
func CachedDocument(ctx context.Context, st index.Store, p Plugin, repo index.Repo, scope IndexScope, kind string) (*index.Cache, error)
// ServeDocument: http.ServeContent with ETag "<sha256>" and Last-Modified RenderedAt.
func ServeDocument(w http.ResponseWriter, r *http.Request, c *index.Cache, contentType string)

// ServeBlob is the one read path of blob bytes, for the facade and every frontend (ADR 0009):
// GET and HEAD of the blob d with one request to the backend at most. If-None-Match is 304
// before any request; HEAD is Provider.StatBlob; GET is Provider.OpenBlob with the client's
// Range when it is one byte range, a redirect answered 307 with Location, a body streamed with
// the backend's status, Content-Length, Content-Range and Accept-Ranges. Errors before the
// response go to opts.Fail (nil: WriteError); the error returned is a stream that broke after
// the headers, for the caller's log.
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

var ErrInvalid, ErrUnsupported, ErrUnauthenticated, ErrForbidden error
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
| `format.ErrInvalid` | 400 |
| `format.ErrUnauthenticated` | 401, `WWW-Authenticate: Basic realm="lajstrom"` |
| `format.ErrForbidden` | 403 |
| `format.ErrMethodNotAllowed` | 405, `Allow: GET, HEAD` |
| `index.ErrNotFound`, `provider.ErrNotFound` | 404 |
| `index.ErrConflict`, `provider.ErrConflict` | 409 |
| `*http.MaxBytesError` | 413 |
| `format.ErrUnsupported`, `provider.ErrUnsupported` | 501 |
| `provider.ErrUnauthorized` (the backend rejected Lajstrom's own credentials) | 502 |
| `provider.ErrUnavailable` (the circuit breaker of the registry is open, the request was not sent) | 503, `Retry-After`: the seconds until the breaker's trial request; under `/v2` the code `UNAVAILABLE`, on the API `UNAVAILABLE` |
| `format.ErrBadGateway` | 502 |
| `format.ErrUnavailable` (the upstream answered `429`) | 503, `Retry-After`: the `*format.UnavailableError`'s, whole seconds, at least 1 |
| `format.ErrGatewayTimeout` | 504 |
| anything else, `nil` included | 500 |

### Proxy and group repos (`internal/format`, ADR 0013)

What a plugin provides for proxy and group repos of its format, and what its handlers ask the
core (proxy-and-group.md §10). A format has the kind `proxy` when its plugin implements
`Proxier`, and the kind `group` when it implements `Grouper`; creating a repo of a kind the
format does not have is `400`. The core, `internal/proxy`, does everything else: the allow and
deny lists, `proxy_meta` and its TTLs, the fetch claims, the upstream client, the spool and the
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

// Proxy is the core as the plugins' handlers see it; *proxy.Core implements it. Document and
// File take a repo of kind proxy, or of kind group of a format whose plugin is a Grouper;
// Forward a repo of kind proxy; another kind wraps ErrUnsupported.
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
    Location string
    ID       string
    Offset   int64
}

// Pinger is optional: a Provider that has it is pinged by serve for /-/readyz (GET /v2/ with the
// configured credentials); one without it counts as answering (#23).
type Pinger interface {
    Ping(ctx context.Context) error
}

type Set interface {
    Provider(name string) (Provider, bool)
}
type Map map[string]Provider                // implements Set

type Capabilities struct {
    CreateOnPush, Referrers, Catalog, DeleteManifest, ImmutableTags bool
    BlobRedirect bool                       // the operator's switch (capabilities.blobRedirect): false, OpenBlob never returns a URL
    MaxBlobSize int64                       // 0 = unknown
}

var ErrNotFound, ErrConflict, ErrUnsupported, ErrUnauthorized error
// ErrUnavailable: the provider did not send the request, its registry's circuit breaker is
// open. An *UnavailableError wraps it with the time until the breaker's trial request.
var ErrUnavailable error
type UnavailableError struct{ RetryAfter time.Duration }
func RetryAfter(err error) (time.Duration, bool) // of the *UnavailableError err wraps
```

### Index store (`internal/index`)

```go
type Store interface {
    Reader
    Writer
    Migrate(ctx context.Context) error
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
    ListUsers(ctx context.Context) ([]User, error)
    GetTokenByHash(ctx context.Context, hash string) (*Token, error)
    ListTokens(ctx context.Context, userID, repoID *int64) ([]Token, error)
    ListBindings(ctx context.Context, principal string) ([]Binding, error)
    GetKey(ctx context.Context, repoID *int64, kind string) (*Key, error)
    ListKeys(ctx context.Context, repoID *int64) ([]Key, error)
    GetSession(ctx context.Context, repoID int64, key string) (*Session, error)
    GetSessionByID(ctx context.Context, id int64) (*Session, error)
    ListAudit(ctx context.Context, f AuditFilter) ([]AuditEvent, error)
    // coordination
    GetTagClaim(ctx context.Context, repoID int64, pkg, tag string) (*TagClaim, error)
    GetFetchClaim(ctx context.Context, repoID int64, pkg, tag string) (*FetchClaim, error)
    GetReindexRun(ctx context.Context, id int64) (*ReindexRun, error)
    // LatestReindexRun: the newest run that covered repoID, its own or a run of every repo;
    // repoID 0 selects the runs of every repo only
    LatestReindexRun(ctx context.Context, repoID int64) (*ReindexRun, error)
    // derived
    GetPackage(ctx context.Context, repoID int64, name string) (*Package, error)
    GetPackageByNameOCI(ctx context.Context, repoID int64, nameOCI string) (*Package, error)
    ListPackages(ctx context.Context, repoID int64, prefix string) ([]Package, error)
    GetVersion(ctx context.Context, packageID int64, version, variant string) (*Version, error)
    GetVersionByTag(ctx context.Context, packageID int64, tag string) (*Version, error)
    GetVersionByDigest(ctx context.Context, repoID int64, d digest.Digest) (*Version, error)
    ListVersions(ctx context.Context, packageID int64) ([]Version, error)
    // ListUnseenVersions: the versions of a repo whose IndexedAt is before seenBefore, with
    // their packages (the sweep of reindex)
    ListUnseenVersions(ctx context.Context, repoID int64, seenBefore time.Time) ([]VersionMatch, error)
    ListFiles(ctx context.Context, versionID int64) ([]File, error)
    FindFiles(ctx context.Context, repoID int64, name string) ([]FileMatch, error)
    GetIndexCache(ctx context.Context, repoID int64, scope, kind string) (*Cache, error)
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
    CreateUser(ctx context.Context, u *User) error
    UpdateUser(ctx context.Context, u *User) error
    CreateToken(ctx context.Context, t *Token) error
    RevokeToken(ctx context.Context, id int64, at time.Time) error
    TouchToken(ctx context.Context, id int64, lastUsed time.Time) error
    CreateBinding(ctx context.Context, b *Binding) error
    DeleteBinding(ctx context.Context, id int64) error
    CreateKey(ctx context.Context, k *Key) error
    RetireKey(ctx context.Context, id int64, at time.Time) error
    UpsertSession(ctx context.Context, s *Session) error // a second open session of a key: ErrConflict
    // TouchSession moves the expiry of an open, unexpired session and returns it, holding the
    // row until the transaction ends: a change of its files starts with it.
    TouchSession(ctx context.Context, id int64, at, expiresAt time.Time) (*Session, error)
    DeleteSession(ctx context.Context, id int64) error
    ExpireSessions(ctx context.Context, before time.Time) (int, error)
    AppendAudit(ctx context.Context, ev *AuditEvent) error
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
    ExpireFetchClaims(ctx context.Context, before time.Time) (int, error)  // HeartbeatAt before before; reindex, reconcile
    // derived (written only by ingest, reindex and delete); every version write and delete keeps
    // packages.created_at the earliest created_at of the package's versions
    UpsertPackage(ctx context.Context, p *Package) error
    UpsertVersion(ctx context.Context, v *Version) error           // *TagConflictError on a tag with another digest
    TouchVersions(ctx context.Context, ids []int64, at time.Time) error // IndexedAt only: rows reindex saw
    ReplaceFiles(ctx context.Context, versionID int64, files []File) error
    DeleteVersion(ctx context.Context, id int64) error
    DeletePackage(ctx context.Context, id int64) error
    DeleteEmptyPackages(ctx context.Context, repoID int64) (int, error) // packages without versions
    PutIndexCache(ctx context.Context, c *Cache) error
    InvalidateIndexCache(ctx context.Context, repoID int64, scope string) error
    UpsertOCIRepo(ctx context.Context, r *OCIRepo) error
    PruneOCIRepos(ctx context.Context, repoID int64, seenBefore time.Time) (int, error)
    ResetDerived(ctx context.Context, repoID int64) error // leaves the claims of both kinds and proxy_meta
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
// control plane Repo, User, Token, Binding, Key, Session, AuditEvent; coordination TagClaim,
// ReindexRun, FetchClaim; derived Package, Version, File, Cache (index_cache), OCIRepo,
// ProxyMeta, JournalEntry, JournalState; query types FileMatch (file + version + package),
// VersionMatch (version + package) and AuditFilter. Ids are int64, timestamps UTC, nullable columns are pointers.

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
type FetchClaim struct {                    // a row of fetch_claims
    ID, RepoID             int64
    Package, Tag           string           // the package path (Plugin.EscapeName), profile.Tag of version and variant
    Principal              string           // of the request whose miss started the fetch
    Replica                string           // host name and process id
    ClaimedAt, HeartbeatAt time.Time
}

var ErrNotFound, ErrConflict error
type TagConflictError struct {              // errors.Is(err, ErrConflict) is true; UpsertVersion, ClaimTag
    Tag      string
    Existing digest.Digest
}
type ReindexConflictError struct {          // errors.Is(err, ErrConflict) is true; StartReindexRun
    Running ReindexRun                      // the live run that overlaps: replica, StartedAt
}

// Migrations: migrations/<dialect>/NNNN_<name>.sql, embedded (index-schema.md, Migrations).
const DialectSQLite, DialectPostgres = "sqlite", "postgres"
func MigrationVersions(dialect string) ([]int, error)                 // embedded, ascending
// ApplyMigrations applies the pending migrations, each in a transaction under the migration
// lock that records it in schema_migrations, and returns those it applied (empty: current).
func ApplyMigrations(ctx context.Context, db *sql.DB, dialect string) (applied []int, err error)
func AppliedMigrations(ctx context.Context, db *sql.DB) ([]int, error) // from schema_migrations

// Drivers (internal/index/sqlite, internal/index/postgres): *Store implements index.Store and
// adds Ping(ctx) error, ApplyMigrations(ctx) ([]int, error), AppliedMigrations(ctx) ([]int, error).
func Open(ctx context.Context, dsn string) (*Store, error)            // does not migrate
```

Both engines (SQLite, Postgres) pass the same conformance test suite in `internal/index/storetest`.

### Reindex (`internal/index/reindex`)

```go
type Options struct {
    Index       index.Store
    Providers   provider.Set
    Inspector   Inspector                     // *ingest.Pipeline
    Audit       audit.Writer
    Now         func() time.Time              // nil: time.Now
    Log         *slog.Logger                  // nil: slog.Default()
    Concurrency int                           // manifest reads at once per registry; zero: 8
    ClaimTTL    time.Duration                 // stale tag claims are deleted; zero: ingest's default
}
type Inspector interface {
    Inspect(ctx context.Context, repo index.Repo, pkgPath, tag string, m ocispec.Manifest, raw []byte, o ingest.InspectOptions) (*ingest.Rows, error)
}
func New(o Options) *Worker

type Request struct {
    Repo      *index.Repo                     // nil: every repo
    Prune     bool                            // sweep rows not seen (default on for a run of every repo)
    Verify    bool                            // read and check every manifest, known digests too
    Source    string                          // cli, api, reconcile
    Principal string
}
// Start takes the lock (StartReindexRun) and writes repo.reindex; *index.ReindexConflictError
// when a run overlaps. Run reads the registry, beats, and finishes the run whatever happens; its
// error is the run's own (Report.Repos[].Errors hold the manifests that failed).
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
    Degraded        int       `json:"degraded"`
    Referrers       int       `json:"referrers"`
    Foreign         []Finding `json:"foreign"`
    OrphanReferrers []Finding `json:"orphanReferrers"`
    OrphanJournal   []Finding `json:"orphanJournal"`   // npm journal entries of a repository without versions
    Drift           []Drift   `json:"drift"`
    Errors          []Finding `json:"errors"`
    Truncated       bool      `json:"truncated"`       // a list stopped at 1 000 entries
}
type Finding struct {
    Repository   string        `json:"repository"`    // OCI repository below the repo's base
    Tag          string        `json:"tag,omitempty"`
    Digest       digest.Digest `json:"digest"`
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
    SessionTimeout time.Duration                       // zero: 30 minutes after a session's last request
}
func New(o Options) *Pipeline

// Ingest stores a manifest the facade received for <format>/<repo>/<pkgPath>:<tag>; raw defines
// the digest. Errors wrap ErrInvalid (profile, plugin, missing blobs, a lajstrom.origin.* key,
// which only Upload with an Origin writes) or ErrConflict (tag, or the package path of another
// spelling). Inspect reads the lajstrom.origin.* keys as any annotation.
func (p *Pipeline) Ingest(ctx context.Context, repo index.Repo, pkgPath, tag string, m ocispec.Manifest, raw []byte, principal auth.Principal) (*Result, error)
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
// (ErrInvalid otherwise). Without Capabilities.Referrers the push runs under
// Store.WithReferrersLock of the subject. One audit event: push, Subject the subject's digest,
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
// #61). Capabilities.DeleteManifest false is provider.ErrUnsupported and changes nothing. On a
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
// the audit row (delete, Details {"referrers": n}). Errors wrap provider.ErrUnsupported,
// index.ErrNotFound or provider.ErrConflict (the backend refused this one delete).
func (p *Pipeline) Delete(ctx context.Context, repo index.Repo, del format.Deletion, principal auth.Principal) (*format.Deleted, error)

// Inspect runs the steps of Ingest that only read (1 to 3) on a manifest found in the registry
// at <repo base>/<pkgPath>:<tag> and returns the rows step 6 writes; nothing is pushed or
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

// Pipeline implements format.Sessions (Open, Add, Files, Commit, Abort). The files column of a
// session is {"name", "version", "files": [SessionFile…], "committing"}; Commit marks the session
// committing before it publishes, so that one Commit runs at a time, and a mark older than
// ClaimTTL is a Commit that died. ExpireSessions marks the open sessions of every kind whose time
// is up; serve calls it every minute, and nothing depends on it.
func (p *Pipeline) ExpireSessions(ctx context.Context) (int, error)

var ErrInvalid = format.ErrInvalid
var ErrConflict = index.ErrConflict
var ErrNotImplemented error                 // until issue #12
```

### Auth and audit (`internal/auth`, `internal/audit`)

```go
type Kind string                            // KindUser, KindToken, KindGroup, KindAnonymous
type Principal struct {
    Kind Kind
    Name string                             // user or group name, deploy-token id; empty for anonymous
}
func (p Principal) String() string          // user:<name>, token:<id>, group:<name>, anonymous
func ParsePrincipal(s string) (Principal, error) // errors wrap ErrInvalidPrincipal
var Anonymous Principal
var ErrInvalidPrincipal error

// The request's principal; the authentication middleware (#43 Basic, #26 bearer) stores it.
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
```

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
- **UploadedFile is re-openable via a request spool**: Build reads the file to parse and digest
  it, ingest reads it again to push it. The spool lives for one request and is never served
  from; artifact bytes live only in the registry.
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
- **The principal comes from the context, not from a header parser.** Until the
  authentication middleware stores one with `auth.WithPrincipal` (#43 Basic, #26 bearer), every
  request is anonymous: parsing Basic credentials without verifying them would be a hole. A nil
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
  `ErrConflict`; a row that refers to a missing row wraps `ErrNotFound`. `UpsertVersion`
  resolves a race on a tag with `INSERT … ON CONFLICT DO NOTHING` and a second read, so the
  loser gets the `TagConflictError`. `serve` migrates on start under the same lock as
  `lajstrom migrate`.
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
  (SQLite).
- **Proxy repos plug in through `format.Proxier`** (ADR 0013, #309). It is the sketch of
  proxy-and-group.md §10 without `MergeGroup`, which comes with the groups of #311, and with
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
  the client's `400`, and its error body is never passed on. A group's forward is #311's.
- **Groups plug in through `format.Grouper`** (#311), a `Proxier` with `MergeGroup`, rather
  than a sixth method of `Proxier`: a format may have proxies before groups (#67, #68), and
  the implementers of `Proxier` keep compiling. `MergeGroup` takes the members' documents as
  they serve them, links under their own bases, and writes every link under the group's. The
  hosted member's document is its `index_cache` row of scope `Key(name)`, so no hook maps a name
  to a hosted scope; a format whose hosted scope differs adds one with its groups.
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
| Passwords | `golang.org/x/crypto` (argon2id) | BSD-3 |
| OpenPGP (apt signing) | `github.com/ProtonMail/go-crypto` | BSD-3 |
| Compression | `github.com/klauspost/compress` (zstd, gzip), `github.com/ulikunitz/xz` | BSD-3 / Apache-2.0 |
| AWS (ECR adapter) | `github.com/aws/aws-sdk-go-v2` (`ecr` only) | Apache-2.0 |
| Metrics | `github.com/prometheus/client_golang` | Apache-2.0 |
| Tests | `testing` + `github.com/google/go-cmp`; `github.com/stretchr/testify` acceptable | BSD-3 / MIT |

Not approved without discussion: ORMs, chi/gin/echo, testcontainers (we use compose and CI
service containers), any GPL/LGPL/AGPL/SSPL/BSL dependency, cgo.

## 4. Testing strategy

| Level | Where | Runs | How |
| --- | --- | --- | --- |
| Unit | next to the code | `go test ./...`, always | pure functions, table tests, fixtures built in-process (`internal/testutil` builds a `.deb`, a `.whl`, a `.jar` + POM in memory; no vendored binaries from distributions) |
| Store conformance | `internal/index/storetest` | always for SQLite; Postgres when `LAJSTROM_TEST_POSTGRES_DSN` is set | same suite, both engines |
| Integration | `//go:build integration` | `make test-integration`; CI job with service containers | real registry (Zot and distribution) via `LAJSTROM_TEST_REGISTRY=host:port`; provider, facade, ingest, reindex |
| End-to-end | `test/e2e/` | `make e2e`; CI job | compose stack + real clients in containers: `oras`, `pip`/`twine`, `apt-get`, `mvn`. One script per format, exit code = pass/fail |
| Fuzz | `FuzzEscape`, `FuzzParseControl`, … | `go test -fuzz` locally, short corpus run in CI | parsers and escapers |

Integration and e2e tests must be runnable locally with `make dev-up` first. Flaky tests are
disabled with an issue, not retried.

## 5. Make targets

`make` alone lists the targets (`make help`).

| Target | Does |
| --- | --- |
| `make build` | `go build ./...`, then `go build -ldflags "-X …/internal/version.{Version,Commit,Date}=…" -o bin/lajstrom ./cmd/lajstrom` (from `git describe`, `git rev-parse`, `date -u`) |
| `make test` | `go test -race ./...` |
| `make vet` | `go vet ./...` |
| `make lint` | golangci-lint at the version CI pins (`GOLANGCI_LINT_VERSION`, v2.15.0) |
| `make vulncheck` | govulncheck |
| `make licenses` | go-licenses v1.6.0 against the allowlist of CONTRIBUTING.md |
| `make fmt-check` | fails when `gofmt -l .` lists a file |
| `make check` | fmt-check, build, vet, test, lint, vulncheck, licenses: exactly what CI runs; run before every pull request |
| `make dev-up` / `make dev-down` / `make dev-logs` | compose stack in `deploy/compose/compose.yaml` (issue #15) |
| `make test-integration` | `go test -race -tags integration ./...` against the dev stack; needs `LAJSTROM_TEST_REGISTRY` |
| `make e2e` | `test/e2e/run.sh`: builds the binary, starts `serve` on a configuration of its own against the registry `$LAJSTROM_E2E_REGISTRY` (default the Zot of `make dev-up`) and runs every scenario of `test/e2e/`, or those of `E2E='raw oras'` (docs/testing.md) |
| `make run` | build, then `lajstrom serve --config deploy/compose/lajstrom.yaml` (the example file when that does not exist) |
| `make clean` | removes `bin/` |


## 6. Configuration (shape; every key in [docs/configuration.md](../configuration.md))

```yaml
server:
  listen: ":8080"
  externalURL: "https://art.example.com"   # used in tokens, redirects, rendered indexes
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
keys:
  masterKeyEnv: LAJSTROM_MASTER_KEY         # encrypts the apt keys and the bearer-token key at rest
  tokenAlgorithm: ES256                     # ES256 | EdDSA; the bearer-token key (ADR 0010)
  algorithm: ed25519                        # ed25519 | rsa4096 (apt before 2.0)
  retireGrace: 720h                         # a retired key still signs and stays in key.asc this long
repos:                                      # optional declarative bootstrap, merged into the control plane
  - { name: ubuntu-internal, format: apt, registry: local, anonymousRead: false }
```

Precedence: defaults < file < `LAJSTROM_*` environment variables (scalar keys only). Secrets
never go into the file: `passwordEnv` and `masterKeyEnv` name environment variables.

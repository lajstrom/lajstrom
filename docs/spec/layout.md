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
                              admin (repo/user/token/binding/key/bootstrap/retention/deb/trusted-root/webhook/snapshot)
internal/config/              config file (YAML) + env overrides; validation; example in deploy/
internal/server/              http.Server, routing (net/http ServeMux, Go 1.22 patterns), middleware:
                              request id, logging, metrics, auth, panic recovery; mounts formats, facade, api
internal/facade/              OCI distribution API (/v2): proxy, token endpoint, manifest PUT hook
internal/format/              Plugin interface, registry of plugins, shared helpers (Router, Deps)
internal/format/raw/          raw plugin: parse, routes, index
internal/format/pypi/         pypi plugin: wheel/sdist metadata, simple index, upload API
internal/format/deb/          deb/apt plugin: control and .dsc parsing, Packages/Sources/Contents/Release rendering, GPG, pool routes
internal/format/maven/        maven plugin: coordinates, POM, sessions, maven-metadata.xml, checksums
internal/format/npm/          npm plugin: packuments, publish, dist-tags journal, proxy and group hooks, audit
internal/format/rpm/          rpm plugin: header parsing, repodata (repomd, primary, filelists, other), signed repomd, Packages routes
internal/format/terraform/    terraform plugin: module registry protocol (discovery, versions, download), archive checks, signed archive URLs, publish
internal/format/formattest/   test doubles: Fake plugin, partial index.Store; imported only by tests
internal/oci/profile/         media types, annotations, Escape/EscapeComponent, manifest build+validate, the change of an
                              encrypted manifest and its undoing (pure)
internal/blobcrypt/           the blob cipher of encrypted repos (ADR 0009): AES_256_CTR_HMAC_SHA256 streaming and resumable,
                              the private and public options, their JWE wrapping with go-jose (pure)
internal/ingest/              pipeline (validate → parse → push → index), publication sessions
internal/provider/            Provider interface, Capabilities, errors
internal/provider/generic/    oras-go based provider for distribution, Zot, Harbor, GHCR (+ small adapters)
internal/provider/ecr/        ECR adapter: auth token, CreateRepository, DescribeRepositories; Tokens, an oci upstream's token
internal/provider/harbor/     Harbor adapter: the project of the prefix, the REST API's listing and repository delete, immutability rules
internal/provider/probe/      lajstrom registry probe: measures a registry's capabilities (docs/registries)
internal/provider/providertest/ test doubles: an in-memory Provider; imported only by tests
internal/index/               Store interface, models, migrations (embedded SQL) and their runner
internal/index/reindex/       reindex worker: walk the registry, mark and sweep, runs, the report
internal/index/migrations/    <dialect>/NNNN_<name>.sql for sqlite, postgres and cockroach, embedded; the same versions in each
internal/index/internal/sqlstore/ the SQL both engines share: index.Store over database/sql and a Dialect
internal/index/sqlite/        modernc.org/sqlite implementation
internal/index/postgres/      pgx implementation
internal/index/storetest/     conformance suite of index.Store, run by both engines; imported only by tests
internal/proxy/               proxy repos (ADR 0013): the settings, the upstream client, the metadata cache, fetch on miss
internal/egress/              where an upstream's client may connect (Guard): the configured hosts and the egress proxies
                              as they are, any other host over https at public addresses only (proxy-and-group.md §9.2)
internal/policy/              policy (#101): rules, the decision over rules and verdicts, the view a render reads through
internal/policy/denylist/     the deny list: the document, the signed artifact in the registry, the flat JSON
internal/auth/                principals, password hashing, PAT/deploy tokens, RBAC evaluation
internal/auth/token/          the bearer JWT of /v2: scopes, issue, verify; the CI token (rbac.md §11)
internal/auth/oidc/           OIDC relying party: discovery, JWKS, ID token verification, code exchange (#69); the
                              verification of a Kubernetes service-account token of auth.kubernetes (rbac.md §9)
internal/auth/clientcert/     client certificates (rbac.md §9): the bundles, a chain verified against them, the name
                              of the leaf, the header an edge forwards a certificate in
internal/auth/dpop/           the DPoP proofs of a web UI's token (RFC 9449, #1170): verify, used once on any replica
internal/auth/ci/             token exchange for CI (rbac.md §11): the trust policies and their claims, those of the
                              API with the steps of one that writes and its expiry, the exchange at /-/oidc/token,
                              the CI token as a credential
internal/api/                 /api/v1 management REST (repos, users, tokens, bindings), JSON
internal/keys/                server signing key, per-repo GPG keys (apt, rpm), encryption at rest, keys held in a KMS (ADR 0020),
                              the keys of encrypted repos and their data keys (ADR 0009),
                              the master keys, the one the index names to seal, the rewrap (#1288),
                              the data keys of keys.masterKeyKMS, wrapped by a KMS key (#1388)
internal/keys/awskms/         AWS KMS for keys: DescribeKey, GetPublicKey, Sign, Decrypt, Encrypt over net/http, SigV4 of
                              aws-sdk-go-v2
internal/keys/gcpkms/         GCP Cloud KMS for keys: cryptoKeyVersions.get, getPublicKey, asymmetricSign, asymmetricDecrypt,
                              cryptoKeys.get, encrypt, decrypt over net/http
internal/keys/keystest/       test doubles: a KMS in memory; imported only by tests
internal/gcpauth/             the access token of the workload's service account from the GCP metadata server
                              (an oci upstream's auth.gcp, gcpkms)
internal/snapshot/            snapshots of the control plane (#76): the document, sealed or not, written to and read from
                              the registry through the provider (manifest-profile.md §9)
internal/audit/               audit log writer
internal/attest/              the verification of attachments (#89): the trust of public keys and keyless identities, DSSE,
                              sigstore bundles, cosign's simple signing; no I/O: keys with the standard library, keyless
                              with sigstore-go against an offline trusted root (manifest-profile.md §6.3)
internal/webhook/             webhooks (webhooks.md): events written in the audit row's transaction, their delivery
                              by the workers of every replica (signature, retries, egress.Guard), the management service
internal/metrics/             the Prometheus collectors (fixed labels) and the handler of the metrics listener
internal/testutil/            fixture builders, in memory: .deb, .dsc + source tarballs, .rpm, .whl/sdist, .jar + POM, npm tarball, archives, bombs, fuzz seeds
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
  write), and raw, npm and terraform `server`; they publish through `format.Deps.Ingest`. Frontend HTTP
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
// Streamers; maven's deploys stream through Sessions.Add instead, and maven is a Streamer for
// the one file a proxy fetches.
type Streamer interface {
    Plugin
    // the checksums Build needs besides sha256, and the inspector the bytes pass through
    Inspect(repo index.Repo, up StreamUpload) (Inspection, error)
}

// Admitter is a Plugin that checks a version against its repo's policy on a write: ingest calls
// Admit with the entry of Validate, after Validate, on every publish (an upload, the commit of a
// publication session, a manifest of /v2), and never on Inspect: reindex keeps what the registry
// has, whatever the policy says now. maven refuses a SNAPSHOT build in a release repo and a
// release in a snapshot repo (policy.maven.versionPolicy, #80). An error wraps ErrInvalid (400).
type Admitter interface {
    Plugin
    Admit(repo index.Repo, e Entry) error
}

// LatestPicker is a Plugin whose format's client installs one version of a package by default,
// the package's latestVersion (http-routing.md §4): LatestVersion returns it among versions, the
// package's distinct versions newest first by VersionKey, with state, its journal state; "" when
// the client installs none. npm, maven, pypi, apt and rpm; without it (raw), the newest.
type LatestPicker interface {
    Plugin
    LatestVersion(versions []string, state []index.JournalState) string
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
    ArtifactType string // application/vnd.lajstrom.npm.dist-tags.v1, …npm.deprecation.v1, …pypi.yank.v1
    Op           string // add, rm, deprecate, undeprecate; yank, unyank
    Tag          string // lajstrom.npm.tag
    Version      string // lajstrom.npm.version, lajstrom.pypi.version
    Message      string // lajstrom.npm.message, deprecate only; lajstrom.pypi.reason, yank only and optional
}

// Sessions are the publication sessions of a format whose upload is several files (maven,
// flow B; an apt source package, #79); *ingest.Pipeline implements them (#53). A key,
// <name>:<version>, has one open session in a repo, held by one principal. Add streams the file
// into the registry (ADR 0014) and records it with its checksums; Refuse marks a file the
// frontend refused after it arrived (maven: a checksum file that disagrees), which keeps Commit
// from publishing until Add replaces it; Commit publishes the files through Upload, once at a
// time, with the session's name, version and uploaded-by and the params the frontend passes
// (apt: the suite and the component; maven: none), which cannot replace those three; a session
// whose time is up (30 minutes after its last request, or the repo's maven.sessionTimeout) is
// dropped, never published in part.
type Sessions interface {
    Open(ctx context.Context, repo index.Repo, name, version string, principal auth.Principal) (*index.Session, error)
    Add(ctx context.Context, sessionID int64, name string, r io.Reader, size int64) (SessionFile, error)
    Files(ctx context.Context, sessionID int64) ([]SessionFile, error)
    Refuse(ctx context.Context, sessionID int64, name, reason string) error
    Commit(ctx context.Context, sessionID int64, params map[string]string) (*Published, error) // Existed: the idempotent repeat
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
    Encryption Encryption                   // the keys of encrypted repos (internal/keys, ADR 0009); nil: no master key
}

// Encryption wraps and opens the data keys of encrypted repos (manifest-profile.md §11);
// internal/keys implements it over the index, the master key and the KMS, format does not
// import keys. Unwrap caches what it opened for 15 minutes; a KMS that does not answer is
// ErrUnavailable (503).
type Encryption interface {
    // Wrap wraps opts for the repo's active key, made on first use from policy.encryption.key,
    // and for the policy's recipients: the value of org.opencontainers.image.enc.keys.jwe and
    // the fingerprint of the active key.
    Wrap(ctx context.Context, repo index.Repo, opts blobcrypt.Private) (jwe, fingerprint string, err error)
    // Unwrap opens jwe with the repo's key of fingerprint, active or retired.
    Unwrap(ctx context.Context, repo index.Repo, fingerprint, jwe string) (blobcrypt.Private, error)
    // Seal and Open keep the data key of a facade upload in its row, sealed with the master key
    // the index names; Open tries every key the process holds, that one first. Seal fails with
    // keys.ErrNoSealingKey while the process does not hold it (index-schema.md, #1288).
    Seal(aad string, key []byte) ([]byte, error)
    Open(aad string, sealed []byte) ([]byte, error)
}
// Decryption returns what ServeBlob needs to decrypt the file f of the version at the package
// path path of an encrypted repo, from its row of blob_map; nil, nil for a plain layer.
func Decryption(ctx context.Context, d Deps, repo index.Repo, path string, f index.File) (*BlobKey, error)

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
    Checksum string                         // <algorithm>:<lowercase hex>, the upstream's checksum that was verified; "" for a file fetched unchecked
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
    // ContentTag: Tag is made of the bytes of the one file (raw), so a claim of Tag by another
    // digest publishes the same file, and Upload waits for it (index-schema.md, Coordination).
    ContentTag bool
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
    Path      string                        // package path below <format>/<repo>/ of the manifest's OCI repository (index.ManifestPath of a version)
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
var Reserved = []string{"-", ".well-known", "api", "lajstrom", "oci", "ui", "v2"} // read-only; oci: pass-through repos (#24); lajstrom: snapshots (#76)
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
// WithVia(ctx, repo); for a proxy, or a group without a hosted member or with several (an apt
// group, proxy-and-group.md §8.6), an error wrapping ErrMethodNotAllowed once the principal may
// read it. The handler then authorizes the route's
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
// a 5xx followed by the words of the first Explainer in err (errors.As) and by what a
// *PartialError in err deleted before it failed.
func ClientMessage(err error) string
// Explainer: an error with words for the client, which ClientMessage adds to a 5xx (#667). They
// name only what the client sent and Lajstrom's own names (an OCI name below the instance
// prefix, a digest, a tag), never a URL, the instance prefix, a credential or the registry's
// words. *ingest.NotVisibleError has one.
type Explainer interface {
    error
    Explain() string
}
// SetRetryAfter sets Retry-After from provider.RetryAfter(err), or from the *UnavailableError
// err wraps: whole seconds, rounded up, at least 1; 5 for index.ErrUnavailable; nothing for an
// error without one. WriteError, the facade, the API and the authentication middleware call it.
func SetRetryAfter(h http.Header, err error)

// CachedDocument returns the index_cache row of (repo, scope, kind), or renders the scope with
// p.RenderIndex, stores every document (SHA256, RenderedAt) and returns the one of kind; lookup,
// render and store run in one st.Tx, and the documents are stored only while the repo's
// IndexCacheGeneration is the one read before the render. index.ErrNotFound when no document
// of kind is rendered. Of a repo of kind group, p is an IndexGrouper (else ErrUnsupported): the
// scope is rendered with RenderGroupIndex over the group's members (ListGroupMembers, in the
// transaction), and kept under the group with the group's generation, which an invalidation of
// a member's documents increments (proxy-and-group.md §8.6).
func CachedDocument(ctx context.Context, st index.Store, p Plugin, repo index.Repo, scope IndexScope, kind string) (*index.Cache, error)
// ServeDocument: http.ServeContent with ETag "<sha256>" and Last-Modified RenderedAt; the ETag is
// the one validator: If-Modified-Since is never 304, and an If-Range date gives the whole document.
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
    Decrypt        *BlobKey                 // an encrypted layer: decrypted as it streams, never redirected
}
// BlobKey is an opened encrypted layer: its data key, nonce, HMAC and plaintext digest
// (manifest-profile.md §11). ServeBlob decrypts a whole blob checking the HMAC and the digest,
// its last byte held back, and a range from the 16-byte boundary before it.
type BlobKey struct {
    Private blobcrypt.Private
    HMAC    []byte
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
| `format.ErrForbidden`; `provider.ErrLimit` (the registry refused the write for a limit or a quota of its own, which the message names, #357) | 403 |
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
format does not have is `400`. A format whose groups render documents of their own (apt) has
`IndexGrouper` instead of `Grouper` (below). The core, `internal/proxy`, does everything else: the allow and
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
    // <server.externalURL>/<format>/<repo>/, or is relative to the document, as a hosted repo's
    // (pypi). The Document's Kind is kind.
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
    Unchecked bool                          // no Checksums: the format takes the file unchecked (Unlisted)
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
    // starts with base, <server.externalURL>/<format>/<group>/, or is relative to the document
    // (pypi); a member's links are under its own base, or relative. docs has at least one
    // element. The Document's Kind is kind.
    MergeGroup(ctx context.Context, group index.Repo, name, kind string, docs []MemberDocument, base string) (Document, error)
}

// HostedScoper is implemented by a Grouper whose hosted repos render the document of a name
// under another index_cache scope than its key (#68): a group reads its hosted member's
// document at HostedScope(Key(name)). Without it, the scope is the key.
type HostedScoper interface {
    Grouper
    // HostedScope is the scope of a hosted repo's document of the name whose key is key: pypi
    // simple/<project>.
    HostedScope(key string) IndexScope
}
type MemberDocument struct {
    Repo    index.Repo                       // the member; its Kind tells the hosted member from a proxy
    Content []byte                           // its document of the kind, as it serves it
}

// Unlisted is implemented by a Proxier with files that the format takes unchecked when the
// upstream has no document that lists them (§5, steps 5 and 8): maven's signatures without a
// .sha1 (ADR 0013, Amendment).
type Unlisted interface {
    Proxier
    // UnlistedEntry returns the entry of the file that ref names, its one file Unchecked and
    // without Checksums, when the upstream does not have the document of ref.Kind; false for a
    // file the format does not take so. The core then fetches the file as any other, bounded,
    // without a check, and Origin.Checksum is "".
    UnlistedEntry(ref ArtifactRef) (UpstreamEntry, bool)
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

// IndexGrouper is implemented by a plugin whose group repos have documents of their own,
// rendered from the versions of their members as a hosted repo's from its own (proxy-and-group.md
// §8.6, #68): a format has the kind group when its plugin implements Grouper or IndexGrouper.
// The group's members are hosted repos, any number of them; its handlers serve it as a hosted
// repo, its documents through CachedDocument, and the core (Proxy) does not serve it.
type IndexGrouper interface {
    // RenderGroupIndex renders every document of scope of group, as RenderIndex renders a
    // hosted repo's, from the versions of members, the group's members in order, read through
    // st; of the entries that several members have, the format's rules say which the
    // documents list (apt: http-routing.md, apt, Group). index.ErrNotFound when no member has
    // the scope.
    RenderGroupIndex(ctx context.Context, group index.Repo, members []index.Repo, scope IndexScope, st index.Reader) ([]Document, error)
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
    // Key(name), or HostedScope(Key(name)) of a HostedScoper, and kind, a proxy member's what
    // Document answers for it with WithVia(ctx, group), a 403 of its lists or a 404 meaning the
    // member does not have the name, and another error that it does not take part (§8.1). The
    // documents are merged (MergeGroup) on every call and nothing is stored: the Cache has
    // RepoID the group's, Scope Key(name), SHA256 the group's ETag (§8.5), RenderedAt the newest
    // member document's, no ID. When no member has the name: the first proxy member's error,
    // else index.ErrNotFound.
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
    registry.ReferrerLister                 // Referrers: the API, or the tag schema without it; both, each digest once, the API's first, with Capabilities.ReferrersAlwaysTag
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

// Reporter is optional: what a Provider knows of its registry without a request, for the admin's
// GET /api/v1/registries (http-routing.md §4). The generic and ecr providers have it.
type Reporter interface {
    Report() Report
}
type Report struct {
    Vendor       string                     // the vendor its probe detected (docs/registries/README.md, Support tiers); "" before a probe answered
    Capabilities *Capabilities              // as Capabilities answers them; nil before a probe succeeded
    Breaker      BreakerState               // its registry's circuit breaker
}
type BreakerState string
const (
    BreakerClosed   BreakerState = "closed"
    BreakerOpen     BreakerState = "open"
    BreakerHalfOpen BreakerState = "half-open"
)

type Set interface {
    Provider(name string) (Provider, bool)
}
type Map map[string]Provider                // implements Set

// The implementations Register a Constructor per registries[].type from init (generic, ecr, harbor);
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
    ReferrersAlwaysTag bool                 // the operator's switch (referrers.alwaysTag): a referrer push writes the sha256-<digest> index with the referrers API too, and Referrers lists both
    MaxBlobSize int64                       // 0 = unknown
    Consistency Consistency                 // when reads see an acknowledged write: the vendor table's or capabilities.consistency
    ConsistencyBound time.Duration          // the longest lag known of an eventual backend (capabilities.consistencyBound); 0 = none known
    MaxNameDepth int                        // components of a name below the instance prefix; 0 = no limit
    MaxNameLength int                       // bytes of a name below the instance prefix; 0 = no limit
    TagsFoldCase bool                       // the backend stores a tag lower-cased and resolves tags without case (Gitea, Forgejo): the vendor table's or capabilities.tagsFoldCase
    BuildRoutes bool                        // creates no repository whose name ends in a build route (Quay): capabilities.buildRoutes
}
// CheckName: nil, or an error naming the limit of c that the name breaks; callers answer 4xx before a write.
func (c Capabilities) CheckName(name string) error
// CheckNew: CheckName for a repository the write would create, which the caller's index does not
// have; it also refuses, naming the rule, a name that ends in a build route where BuildRoutes.
func (c Capabilities) CheckNew(name string) error
// BuildRoute: the name ends in a build route of Quay's web UI, a component `build` that is not
// the first, followed by nothing, one component, or one and `logs` or `status` (quay/quay@77b369a).
func BuildRoute(name string) bool
// TagSchema: the provider keeps the sha256-<digest> tag schema of referrers, the index of a
// subject that a referrer push rewrites: without the referrers API, and with it when
// ReferrersAlwaysTag is on. Ingest takes the referrers lock for such a push and rewrites the index
// on a referrer's delete, and reindex has it repaired, wherever it is true.
func (c Capabilities) TagSchema() bool                 // !c.Referrers || c.ReferrersAlwaysTag
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
// ErrLimit: the backend refused a write for a limit or a quota of its own that the write would
// pass, which the same write meets again until the operator raises it or frees room: not
// retried, not ErrUnauthorized (#357). The generic provider reads as one a 403 or DENIED whose
// message speaks of a quota or of exceeding a limit (Harbor's project quota, Quay's), and a 403
// to a PATCH or to the closing PUT of an upload session the registry had opened (Gitea's and
// Forgejo's LIMIT_SIZE_CONTAINER, an empty body), but one with UNAUTHORIZED or a message about
// the token or the credentials; the ecr provider LimitExceededException (repositories a
// region, images a repository). An oci upstream's answers keep their rule (NewUpstream). The
// message names the limit, and nothing of the registry's own: no URL, instance prefix, account
// or message; the provider logs the refusal at warn. A 413 stays a *RejectedError, a 429
// ErrUnavailable.
var ErrLimit error
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
    // ListTokenPage: the tokens the filter keeps, by id, above f.After, at most f.Limit
    // (index-schema.md, Pages of tokens and of open sessions)
    ListTokenPage(ctx context.Context, f TokenFilter) ([]Token, error)
    GetBootstrapToken(ctx context.Context) (*BootstrapToken, error) // used and expired too; the caller checks
    GetConfigState(ctx context.Context) (*ConfigState, error)       // last recorded (rbac.md §5); none: ErrNotFound
    ListBindings(ctx context.Context, principal string) ([]Binding, error)
    // ReadAccess: the rows of the access report (rbac.md §14, #1279) in one read-only
    // transaction, at the database's time, ReadAt (index-schema.md, The access report)
    ReadAccess(ctx context.Context) (*Access, error)
    // the requests that wait for a second admin (rbac.md §13, index-schema.md, Approvals):
    // ListApprovals by id, newest first, below before (0: from the newest), of state ("": any),
    // at most limit; CountPendingApprovals the pending rows whose ExpiresAt is after at and whose
    // RequestedBy is not notBy, the count of GET /api/v1/info (#1336)
    GetApproval(ctx context.Context, id int64) (*Approval, error)
    ListApprovals(ctx context.Context, state string, before int64, limit int) ([]Approval, error)
    CountPendingApprovals(ctx context.Context, at time.Time, notBy string) (int, error)
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
    // ListSealed (#1288, index-schema.md, The master key and its replacement): the values that
    // the master keys seal in table (SealedKeys: keys.private_enc; SealedWebhooks:
    // webhooks.secret_enc; SealedRefresh: oidc_refresh.refresh_enc, #1282) of the rows whose id
    // is above afterID, by id, at most limit
    ListSealed(ctx context.Context, table string, afterID int64, limit int) ([]Sealed, error)
    // GetSealingKey: the row of sealing_key, the master key that every process seals with
    // (#1288); none: ErrNotFound
    GetSealingKey(ctx context.Context) (*SealingKey, error)
    // ListWrappedMasterKeys: every row of wrapped_master_keys, the data keys of a KMS, by KeyID
    // (#1388, index-schema.md, A master key in a KMS)
    ListWrappedMasterKeys(ctx context.Context) ([]WrappedMasterKey, error)
    GetSession(ctx context.Context, repoID int64, key string) (*Session, error)
    GetSessionByID(ctx context.Context, id int64) (*Session, error)
    // ListOpenSessions: the repo's open publication sessions that expire after at, by id, above
    // after, at most limit; no blob upload of the facade
    ListOpenSessions(ctx context.Context, repoID int64, at time.Time, after int64, limit int) ([]Session, error)
    // ListAudit: newest first; with f.Feed, the rows whose id is above f.After, by id ascending
    // (rbac.md §5, #1278). SpanAudit: the count and the first and last ids and times of the
    // rows that PruneAudit would delete with that cut-off (index-schema.md, The audit feed and
    // its prune)
    ListAudit(ctx context.Context, f AuditFilter) ([]AuditEvent, error)
    SpanAudit(ctx context.Context, before time.Time) (AuditSpan, error)
    // webhooks (webhooks.md, index-schema.md, Webhooks): ListWebhooks by id; the delivery log
    // of a webhook newest first, the ids below before (0: from the newest), at most limit
    GetWebhook(ctx context.Context, id int64) (*Webhook, error)
    ListWebhooks(ctx context.Context) ([]Webhook, error)
    ListWebhookDeliveries(ctx context.Context, webhookID, before int64, limit int) ([]WebhookDelivery, error)
    // coordination
    GetTagClaim(ctx context.Context, repoID int64, pkg, tag string) (*TagClaim, error)
    GetFetchClaim(ctx context.Context, repoID int64, pkg, tag string) (*FetchClaim, error)
    GetReindexRun(ctx context.Context, id int64) (*ReindexRun, error)
    // the flows of rbac.md §8 and §10 (index-schema.md, oidc_logins); state, deviceCode and
    // handoff are the sha256 hex of the secret, userCode as shown without its hyphen
    GetOIDCLogin(ctx context.Context, id int64) (*OIDCLogin, error)
    GetOIDCLoginByState(ctx context.Context, state string) (*OIDCLogin, error)
    GetOIDCLoginByDeviceCode(ctx context.Context, deviceCode string) (*OIDCLogin, error)
    GetOIDCLoginByUserCode(ctx context.Context, userCode string) (*OIDCLogin, error)
    GetOIDCLoginByHandoff(ctx context.Context, handoff string) (*OIDCLogin, error)
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
    // the package list, one package and search (index-schema.md, The package list, one package
    // and search): the facts of packages, one per id in their order, the latest version picked by
    // latest (nil: the newest by VersionKey); a page of a repo's packages that q keeps, in its
    // order after its key; the values of an attr key with their counts of packages; one rank of
    // the search after its key
    ListPackageFacts(ctx context.Context, packageIDs []int64, latest LatestFunc) ([]PackageFacts, error)
    ListPackagePage(ctx context.Context, repoID int64, q PackageQuery) ([]PackagePlace, error)
    ListAttrValues(ctx context.Context, repoID int64, key string) ([]AttrValue, error)
    Search(ctx context.Context, q SearchQuery) ([]SearchHit, error)
    GetVersion(ctx context.Context, packageID int64, version, variant string) (*Version, error)
    GetVersionByTag(ctx context.Context, packageID int64, tag string) (*Version, error)
    GetVersionByDigest(ctx context.Context, repoID int64, d digest.Digest) (*Version, error)
    ListVersions(ctx context.Context, packageID int64) ([]Version, error)
    // ListVersionPage: the rows of the limit versions of a package after `after` (nil: from the
    // newest), newest first by VersionKey, every variant of each, a version's rows by variant;
    // with f.Without (attachment kinds, #89), the variants that have no attachment of any of
    // them; with f.Unmet (#1287), the variants that the requirement would refuse (policy.md §11)
    ListVersionPage(ctx context.Context, packageID int64, after *VersionKey, limit int, f VersionFilter) ([]Version, error)
    // ListUnseenVersions: the versions of a repo whose IndexedAt is before seenBefore, with
    // their packages (the sweep of reindex)
    ListUnseenVersions(ctx context.Context, repoID int64, seenBefore time.Time) ([]VersionMatch, error)
    // ListVersionsWithoutSummary: at most limit rows after the id afterID, in id order, whose
    // summary is NULL and whose metadata is not (index-schema.md, The summaries of the rows
    // before 0029)
    ListVersionsWithoutSummary(ctx context.Context, afterID int64, limit int) ([]Version, error)
    ListFiles(ctx context.Context, versionID int64) ([]File, error)
    FindFiles(ctx context.Context, repoID int64, name string) ([]FileMatch, error)
    GetIndexCache(ctx context.Context, repoID int64, scope, kind string) (*Cache, error)
    // IndexCacheGeneration: 0 at first, incremented by every invalidation of the repo's
    // index_cache; a render reads it first (index-schema.md, The generation of a repo's documents)
    IndexCacheGeneration(ctx context.Context, repoID int64) (int64, error)
    ListOCIRepos(ctx context.Context, repoID int64) ([]OCIRepo, error)
    // a cache of the upstream (ADR 0013): the row of a proxy repo's name key and document kind
    GetProxyMeta(ctx context.Context, repoID int64, name, kind string) (*ProxyMeta, error)
    // the journal of an npm or a pypi package (ADR 0012, index-schema.md, the journal): a
    // package's entries in seq order, and its current state ordered by kind and key
    ListJournal(ctx context.Context, packageID int64) ([]JournalEntry, error)
    ListJournalState(ctx context.Context, packageID int64) ([]JournalState, error)
    // attachments (#89, index-schema.md, Attachments): the rows of the versions ids, newest
    // first (CreatedAt descending, those without one last), then by Digest and Layer
    ListAttachments(ctx context.Context, versionIDs []int64) ([]Attachment, error)
    // ListAttested (#1287, index-schema.md, Attachments): each distinct (VersionID,
    // PredicateType, SignedBy) of a verified attachment of the repo's versions, or of
    // versionIDs when not nil; a signature's PredicateType https://sigstore.dev/cosign/sign/v1
    ListAttested(ctx context.Context, repoID int64, versionIDs []int64) ([]Attested, error)
    // RepoStats: the counts of the derived tables of each repo of repoIDs that has a package, in
    // no order; a repo without one has none (index-schema.md, A repo's stats)
    RepoStats(ctx context.Context, repoIDs []int64) ([]RepoStats, error)
    // ListVersionFacts (#1132, index-schema.md, Pages of packages and versions): of each of the
    // versions ids, in their order, its files counted and their sizes added up, and its
    // attachments counted by kind
    ListVersionFacts(ctx context.Context, versionIDs []int64) ([]VersionFacts, error)
    // blob_map (#114, index-schema.md, The mapping of an encrypted repo): the row of the stored
    // digest d at the package path, the rows of the plaintext digest d there (kind "" for both
    // kinds), every row of a repo's path
    GetBlobMap(ctx context.Context, repoID int64, path string, d digest.Digest) (*BlobMap, error)
    FindBlobMap(ctx context.Context, repoID int64, path string, d digest.Digest, kind string) ([]BlobMap, error)
    ListBlobMap(ctx context.Context, repoID int64, path string) ([]BlobMap, error)
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
    // that auth_version moves with the members. It invalidates the group's index_cache, as
    // InvalidateIndexCache of every scope does
    SetGroupMembers(ctx context.Context, groupID int64, memberIDs []int64) error
    // ConvertRepo makes the proxy repo id a hosted one (proxy-and-group.md §2.6): kind hosted,
    // upstream NULL, UpdatedAt now, auth_version incremented. A repo that is not a proxy, or
    // has a fetch claim that beat at or after staleBefore, wraps ErrConflict; no repo
    // ErrNotFound. The one change of a kind
    ConvertRepo(ctx context.Context, id int64, staleBefore time.Time) error
    CreateUser(ctx context.Context, u *User) error
    UpdateUser(ctx context.Context, u *User) error
    // TouchUser sets LastSeenAt (users.last_seen_at) to seenAt when it is null or earlier, else
    // changes nothing, as TouchToken does; it moves neither Revision nor auth_version; no user
    // ErrNotFound (#1283)
    TouchUser(ctx context.Context, id int64, seenAt time.Time) error
    // the lockout (#1280, index-schema.md, The lockout). FailPassword adds one to
    // PasswordFailures (users.password_failures) unless LockedUntil is later than at, which
    // changes nothing and returns false; the failure that reaches limit sets LockedUntil to
    // lockUntil and the count to 0, in the same statement, and returns true.
    // ResetPasswordFailures sets the count to 0 while LockedUntil is not later than at, else
    // changes nothing and wraps ErrConflict. UnlockUser sets the count to 0 and LockedUntil to
    // nil, and returns the LockedUntil it ended when that was later than at (nil: none). None
    // moves Revision or auth_version; no user ErrNotFound
    FailPassword(ctx context.Context, id int64, at, lockUntil time.Time, limit int) (bool, error)
    ResetPasswordFailures(ctx context.Context, id int64, at time.Time) error
    UnlockUser(ctx context.Context, id int64, at time.Time) (*time.Time, error)
    CreateToken(ctx context.Context, t *Token) error
    // RevokeToken sets RevokedAt while it is null; a token revoked already keeps its time and
    // wraps ErrConflict, so that of callers that revoke it at once one does (#1277)
    RevokeToken(ctx context.Context, id int64, at time.Time) error
    // TouchToken sets LastUsedAt to lastUsed when it is null or earlier, else changes nothing:
    // it only moves forward (#1277); no token ErrNotFound
    TouchToken(ctx context.Context, id int64, lastUsed time.Time) error
    // PutBootstrapToken makes t the bootstrap token while the stored one is the one of hash old
    // ("": none), UsedAt cleared; another row there wraps ErrConflict, so that replicas that
    // start together make one. UseBootstrapToken sets UsedAt when hash matches, it is unused
    // and not expired at at, else ErrNotFound, holding the row until the transaction ends
    PutBootstrapToken(ctx context.Context, t *BootstrapToken, old string) error
    UseBootstrapToken(ctx context.Context, hash string, at time.Time) error
    // PutConfigState makes c the configuration recorded while the stored one is the one of
    // fingerprint old ("": none); another row there wraps ErrConflict, so that of replicas that
    // start together one records a change. Its caller appends config.change in the transaction
    PutConfigState(ctx context.Context, c *ConfigState, old string) error
    CreateBinding(ctx context.Context, b *Binding) error
    DeleteBinding(ctx context.Context, id int64) error
    // approvals (#1285). CreateApproval fills ID and inserts a pending request; one of the same
    // Kind and Subject pending wraps ErrConflict. DecideApproval stores State (approved or
    // rejected), DecidedBy, DecidedAt, DecisionReason and Result while the row is pending, else
    // changes nothing and wraps ErrConflict, so that of two decisions one is made; its caller
    // makes the change in the same transaction. ExpireApprovals marks expired the pending rows
    // whose ExpiresAt is not after at, DecidedBy anonymous, and returns them for their rows
    CreateApproval(ctx context.Context, a *Approval) error
    DecideApproval(ctx context.Context, a *Approval) error
    ExpireApprovals(ctx context.Context, at time.Time) ([]Approval, error)
    // OIDC: CreateIdentity fills ID and CreatedAt when zero; (Issuer, Subject) taken wraps
    // ErrConflict. SetIdentityGroups makes the identity_groups rows of the identity exactly
    // names, stores loginAt and groupsExpireAt, and increments auth_version
    CreateIdentity(ctx context.Context, i *Identity) error
    DeleteIdentity(ctx context.Context, id int64) error
    SetIdentityGroups(ctx context.Context, id int64, names []string, loginAt, groupsExpireAt time.Time) error
    CreateKey(ctx context.Context, k *Key) error
    RetireKey(ctx context.Context, id int64, at time.Time) error
    // Reseal writes sealed as the value of the row id of table (ListSealed) where the row still
    // holds old, a compare-and-set, and answers whether it did (#1288)
    Reseal(ctx context.Context, table string, id int64, old, sealed []byte) (bool, error)
    // PutSealingKey makes k the sealing key while the row names the key id old ("": no row,
    // inserted if absent); another id there wraps ErrConflict, so that of replicas that start
    // together one records its key, and of two rewraps one switches (#1288). SetAt is the
    // database's time; a rewrap's caller appends key.rewrap in the transaction
    PutSealingKey(ctx context.Context, k *SealingKey, old string) error
    // PutWrappedMasterKey inserts k where old is nil and there is no row of k.KeyID, and otherwise
    // replaces its Wrapped, WrappedAt and AuditID where the row still holds old, a compare-and-set;
    // another state wraps ErrConflict. DeleteWrappedMasterKey deletes the row of keyID
    // (ErrNotFound). Both in the caller's transaction, which appends key.wrap or key.drop (#1388)
    PutWrappedMasterKey(ctx context.Context, k *WrappedMasterKey, old []byte) error
    DeleteWrappedMasterKey(ctx context.Context, keyID string) error
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
    // PruneAudit deletes at most limit rows that SpanAudit counts with before, whose id is at most
    // upTo, the lowest ids first; the number deleted. A transaction of its own (#1278)
    PruneAudit(ctx context.Context, before time.Time, upTo int64, limit int) (int, error)
    // webhooks. CreateWebhook fills ID, UID (NewUID) when empty, CreatedAt and UpdatedAt;
    // UpdateWebhook stores URL, Events, SecretEnc, Active and Description and sets UpdatedAt
    // (Mask, UID and CreatedBy never change); DeleteWebhook takes its deliveries with it
    CreateWebhook(ctx context.Context, h *Webhook) error
    UpdateWebhook(ctx context.Context, h *Webhook) error
    DeleteWebhook(ctx context.Context, id int64) error
    // CreateWebhookDeliveries inserts ds, the outbox rows of an event, in the caller's
    // transaction: State pending when "", CreatedAt now when zero, NextAt CreatedAt when zero;
    // fills their IDs
    CreateWebhookDeliveries(ctx context.Context, ds []WebhookDelivery) error
    // ClaimWebhookDeliveries claims for replica at most limit pending deliveries of active
    // webhooks whose NextAt is at or before now and that are unclaimed or claimed before
    // staleBefore, in NextAt order: ClaimedBy replica, ClaimedAt now; each with its webhook. On
    // Postgres FOR UPDATE SKIP LOCKED: two replicas never claim one row at once
    ClaimWebhookDeliveries(ctx context.Context, replica string, now, staleBefore time.Time, limit int) ([]WebhookClaim, error)
    // FinishWebhookDelivery stores State, Attempts, NextAt, LastAt, LastStatus, LastError and
    // DeliveredAt of d and clears its claim, while the row's claim is still d's ClaimedBy and
    // ClaimedAt; else ErrNotFound and nothing changes (the claim went stale and was taken over)
    FinishWebhookDelivery(ctx context.Context, d *WebhookDelivery) error
    DeleteWebhookDeliveries(ctx context.Context, before time.Time) (int, error) // CreatedAt before before, any state
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
    // CreateOIDCLogin inserts l and fills ID; a state, device code, user code or handoff taken
    // wraps ErrConflict. UpdateOIDCLogin stores Status, UserID, PolledAt and Handoff of l while
    // the row's status is from, else ErrConflict and nothing changes: one redemption of a state,
    // one approval, one device code, one handoff. ExpireOIDCLogins deletes the rows whose
    // ExpiresAt is before before
    CreateOIDCLogin(ctx context.Context, l *OIDCLogin) error
    UpdateOIDCLogin(ctx context.Context, l *OIDCLogin, from string) error
    ExpireOIDCLogins(ctx context.Context, before time.Time) (int, error)
    // the refresh of OIDC logins (rbac.md §8, #1282; index-schema.md, oidc_refresh).
    // CreateOIDCRefresh inserts r, sealed, and fills ID; JoinOIDCRefresh gives the row of
    // loginID its token and its NextAt, in the transaction of the mint. ClaimOIDCRefresh claims
    // for replica until until at most limit rows whose NextAt is not after at and whose claim is
    // free or ended, and answers them. FinishOIDCRefresh stores Sealed, RefreshedAt, NextAt and
    // Failures of r and frees its claim while the claim is still replica's, else ErrConflict and
    // nothing changes. DeleteOIDCRefresh deletes a row; ClearOIDCRefresh every row
    CreateOIDCRefresh(ctx context.Context, r *OIDCRefresh) error
    JoinOIDCRefresh(ctx context.Context, loginID, tokenID int64, nextAt time.Time) error
    ClaimOIDCRefresh(ctx context.Context, replica string, at, until time.Time, limit int) ([]OIDCRefresh, error)
    FinishOIDCRefresh(ctx context.Context, r *OIDCRefresh, replica string) error
    DeleteOIDCRefresh(ctx context.Context, id int64) error
    ClearOIDCRefresh(ctx context.Context) (int, error)
    // UseDPoPProof stores key, the sha256 hex of a DPoP proof's key thumbprint and jti
    // (rbac.md §10, index-schema.md, dpop_proofs), until expiresAt; a key stored that expires at
    // now or later wraps ErrConflict, a replay, and nothing changes. ExpireDPoPProofs deletes
    // the keys whose expiresAt is before before
    UseDPoPProof(ctx context.Context, key string, expiresAt, now time.Time) error
    ExpireDPoPProofs(ctx context.Context, before time.Time) (int, error)
    ExpireFetchClaims(ctx context.Context, before time.Time) (int, error)  // HeartbeatAt before before; reindex, reconcile
    // StartReplica deletes the rows whose HeartbeatAt is before staleBefore and inserts r, a
    // serve process (index-schema.md, Coordination); on SQLite a fresh row of another replica
    // is *ReplicaConflictError naming it and nothing changes, on Postgres the rows coexist
    StartReplica(ctx context.Context, r *Replica, staleBefore time.Time) error
    HeartbeatReplica(ctx context.Context, id string, at time.Time) error // ErrNotFound: deleted as stale
    // SetReplicaMasterKeys writes MasterKeys of the row id: at the beat that follows the unwrap of
    // a data key of a KMS (#1388); ErrNotFound: deleted as stale
    SetReplicaMasterKeys(ctx context.Context, id string, keyIDs []string) error
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
    DeleteVersion(ctx context.Context, id int64) error // and sets packages.last_delete_at (index-schema.md)
    DeletePackage(ctx context.Context, id int64) error
    DeleteEmptyPackages(ctx context.Context, repoID int64) (int, error) // packages without versions
    // SetVersionSummaries writes the summary of each id, "" as '', to those of the rows whose
    // summary is still NULL, in one transaction, and returns how many it wrote
    SetVersionSummaries(ctx context.Context, summaries map[int64]string) (int, error)
    // PutIndexCache stores c only while generation, read before the render, is the repo's, and
    // says whether it did; InvalidateIndexCache increments the generation, then deletes, of the
    // repo and of every group it is a member of (index-schema.md), as ResetDerived does
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
    // the journal of npm and pypi (derived; written by ingest, reindex and delete). UpsertJournal inserts e
    // and fills ID, or, when the row of (PackageID, Seq) has e's tag and digest, sets its
    // IndexedAt and fills e from it; another tag or digest there is a *TagConflictError and
    // nothing changes: an entry is never rewritten. CreatedAt and IndexedAt are now when zero
    UpsertJournal(ctx context.Context, e *JournalEntry) error
    TouchJournal(ctx context.Context, ids []int64, at time.Time) error // IndexedAt only: entries reindex saw
    DeleteJournal(ctx context.Context, id int64) error                 // the reindex sweep
    // ReplaceJournalState makes the package's journal_state rows exactly state, PackageID
    // filled: the fold of its entries, written in the transaction of the entries it folds
    ReplaceJournalState(ctx context.Context, packageID int64, state []JournalState) error
    // attachments (derived; written by ingest, reindex and delete; they cascade with versions).
    // ReplaceAttachments makes the rows of the referrer d of the version exactly rows, filling
    // their ID, VersionID and Digest (none: removes them); a row with a Tag also removes the
    // version's rows under that tag of another referrer (cosign moved its legacy tag). IndexedAt
    // is now when zero. A version that does not exist wraps ErrNotFound and nothing is written
    ReplaceAttachments(ctx context.Context, versionID int64, d digest.Digest, rows []Attachment) error
    TouchAttachments(ctx context.Context, ids []int64, at time.Time) error // IndexedAt only: rows reindex saw
    // DeleteAttachments removes the rows of the referrer d among the versions of a repo, the
    // delete of a referrer, and returns how many
    DeleteAttachments(ctx context.Context, repoID int64, d digest.Digest) (int, error)
    // SweepAttachments removes the rows of a version whose IndexedAt is before seenBefore, the
    // sweep of reindex once it listed the version's referrers, and returns how many
    SweepAttachments(ctx context.Context, versionID int64, seenBefore time.Time) (int, error)
    // PutBlobMap inserts the rows, or replaces those of the same (repo, path, stored digest),
    // with their IndexedAt; DeleteBlobMap removes one; SweepBlobMap removes the rows of the path
    // whose IndexedAt is before seenBefore, but those of kind blob indexed after keepBlobsAfter
    PutBlobMap(ctx context.Context, rows []BlobMap) error
    DeleteBlobMap(ctx context.Context, repoID int64, path string, d digest.Digest) error
    SweepBlobMap(ctx context.Context, repoID int64, path string, seenBefore, keepBlobsAfter time.Time) (int, error)
}

// Models, one struct per table of index-schema.md (fields in internal/index/models.go):
// control plane Repo, User, Token, BootstrapToken, Binding, Identity, Key, Session, AuditEvent,
// Webhook, WebhookDelivery, Approval, SealingKey (KeyID, Source, Host, AuditID, SetAt; #1288),
// WrappedMasterKey (KeyID, KMS, Wrapped, CreatedAt, WrappedAt, AuditID; #1388);
// coordination TagClaim, ReindexRun, FetchClaim, Replica, OIDCLogin, OIDCRefresh (LoginID,
// TokenID, IdentityID, Sealed, CreatedAt, RefreshedAt, NextAt, Failures, ClaimedBy,
// ClaimedUntil; #1282); derived Package, Version,
// File, Cache (index_cache), OCIRepo, ProxyMeta, JournalEntry, JournalState, Attachment; query types
// FileMatch (file + version + package), VersionMatch (version + package), VersionKey,
// PackageFacts, PackageQuery, PackageSort, PackageKey, PackagePlace, AttrValue, SearchQuery,
// SearchRank, SearchKey, SearchHit (index-schema.md, The package list, one package and search),
// TokenFilter, RepoStats, IdentityGroup (a row of identity_groups with its identity's UserID and
// GroupsExpireAt and its user's UID), AuditFilter (Feed and After: the feed, #1278), AuditSpan
// (Rows, FirstID, LastID, FirstAt, LastAt), Access (ReadAt, Users, IdentityGroups, Bindings,
// Tokens, TokenRepos, CIPolicies, AnonymousRepos: the access report's read, #1279),
// VersionFilter (Without, the attachment kinds of ?without=; Unmet *Requirement, of ?unmet=),
// Requirement (PredicateType, SignedBy []string: one of policy.requireAttachments, #1287),
// Attested (VersionID, PredicateType, SignedBy: a row of ListAttested) and Sealed (Table, ID, AAD,
// Value: a value of ListSealed, AAD its additional data, #1288); Replica has MasterKeys
// []string, the ids of the master keys its process holds, nil for none or a release before 0040
// (#1288). Ids are int64, timestamps UTC, nullable columns are pointers, but nullable text, which
// is "" (Version.OCIRepo: versions.oci_repo, "" for NULL).
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

// LatestFunc picks the latest version of a package for ListPackageFacts among versions, its
// distinct versions newest first by VersionKey, with state, its journal state; "" for none.
type LatestFunc func(packageID int64, versions []string, state []JournalState) string

// What ListTokenPage keeps: each field but Limit narrows when it is set; a Limit below 1 keeps none.
type TokenFilter struct {
    Kind     string                         // pat or deploy
    UserID   *int64                         // the personal access tokens of the user
    RepoID   *int64                         // the deploy tokens of the repo, not those over a mask
    RepoMask string                         // the deploy tokens over the mask, as written
    LiveAt   time.Time                      // those neither revoked nor expired then
    After    int64                          // the id of the page before's last token
    Limit    int
}

// The counts of a repo's derived rows (index-schema.md, A repo's stats).
type RepoStats struct {
    RepoID                                  int64
    Packages, Versions, Manifests, Files    int64 // Versions: distinct (package, version); Manifests: rows of versions
    Size                                    int64 // the sum of files.size
    LastPublishedAt                         *time.Time // the newest versions.created_at
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
// The journal of npm and pypi (ADR 0012, manifest-profile.md §8, index-schema.md, the journal).
// Both tables cascade with packages: DeletePackage, DeleteEmptyPackages and ResetDerived take a
// package's rows.
const JournalDistTag, JournalDeprecated, JournalYanked = "dist-tag", "deprecated", "yanked" // journal_state.kind
type JournalEntry struct {                  // a row of journal: one entry in the registry
    ID, PackageID int64
    Seq           int64                     // lajstrom.npm.seq, lajstrom.pypi.seq
    Tag           string                    // _j00000007
    Digest        digest.Digest             // the entry's manifest
    ArtifactType  string
    Attrs         map[string]string         // the entry's lajstrom.npm.* or lajstrom.pypi.* annotations
    CreatedAt     time.Time                 // org.opencontainers.image.created
    IndexedAt     time.Time                 // when ingest or reindex last wrote or saw the row
}
// The attachments of a version (#89; manifest-profile.md §6.1 to §6.3, index-schema.md,
// Attachments). Kind and Verification are fixed sets that a binary reads tolerantly: a value it
// does not know is shown as it is and never rewritten (index-schema.md, Upgrades).
const AttachmentSignature, AttachmentSBOM, AttachmentScan, AttachmentProvenance, AttachmentAttestation, AttachmentOther =
    "signature", "sbom", "scan", "provenance", "attestation", "other" // attachments.kind
const AttachmentVerified, AttachmentUntrusted, AttachmentUnsigned, AttachmentInvalid, AttachmentUnsupported =
    "verified", "untrusted", "unsigned", "invalid", "unsupported" // attachments.verification
type Attachment struct {                    // a row of attachments: one layer of a referrer of a version
    ID, VersionID  int64
    Digest         digest.Digest             // the referrer manifest
    Tag            string                    // cosign's legacy tag; "" for a referrer pushed by digest
    Layer          digest.Digest             // the attachment's layer; "" for a manifest without layers
    MediaType      string                    // the layer's; the artifact type for a manifest without layers
    ArtifactType   string                    // the manifest's artifactType, else its config's media type
    Kind           string                    // Attachment{Signature,SBOM,Scan,Provenance,Attestation,Other}
    PredicateType  string
    Size           int64                     // the layer's
    Annotations    map[string]string         // the manifest's
    Tool, ToolVersion, Database string       // lajstrom.attachment.* (manifest-profile.md §6.2)
    Input          digest.Digest             // lajstrom.attachment.input when it is a digest
    Verification   string                    // Attachment{Verified,Untrusted,Unsigned,Invalid,Unsupported}
    SignedBy       string                    // the trust entry whose key verified it; "" (NULL) unless verified
    Reason         string                    // why it is not verified; "" when it is
    Trust          string                    // attest.Trust.Hash of the trust that labelled it
    CreatedAt      *time.Time                // the manifest's org.opencontainers.image.created
    IndexedAt      time.Time                 // when ingest or reindex last wrote or saw the row
}
type VersionFacts struct {                  // what ListVersionFacts answers of a versions row
    VersionID      int64
    Files, Size    int64                     // its files rows, and their sizes added up
    Attachments    map[string]int64          // its attachments rows by Kind; a kind without rows is absent
}
type JournalState struct {                  // a row of journal_state: the fold of a package's entries
    PackageID  int64
    Kind       string                       // JournalDistTag, JournalDeprecated or JournalYanked
    Key, Value string                       // a dist-tag and its version; a version and its message; a version and its reason ("" for none)
    Seq        int64                        // the entry that set it
}
type Sighting struct {                      // a row of prune_sightings
    RepoID          int64
    Path            string                  // the OCI repository, as oci_repos.path
    Digest          digest.Digest           // the manifest
    FirstAt, LastAt time.Time               // the first answer since the row was cleared, the latest
}
type Webhook struct {                       // a row of webhooks (webhooks.md §5)
    ID                   int64
    UID                  string             // NewUID: the additional data of SecretEnc
    Mask                 string             // a repo mask of rbac.md §3
    URL                  string
    Events               []string           // the types of webhooks.md §1; nil: every type
    SecretEnc            []byte             // nonce ‖ AES-256-GCM(secret) with the master key
    Active               bool
    Description          string
    CreatedBy            string             // the principal that created it
    CreatedAt, UpdatedAt time.Time
}
const DeliveryPending, DeliveryDelivered, DeliveryFailed = "pending", "delivered", "failed" // WebhookDelivery.State
type WebhookDelivery struct {               // a row of webhook_deliveries: the outbox and the delivery log
    ID, WebhookID int64
    EventID       string                    // the CloudEvents id, a UUID v7: one per event
    EventType     string                    // a type of webhooks.md §1, or webhook.ping
    Payload       []byte                    // the exact body sent
    State         string
    Attempts      int
    NextAt        time.Time                 // when a pending delivery is due
    ClaimedBy     string                    // the replica of a claim; "" when none
    ClaimedAt     *time.Time
    LastAt        *time.Time
    LastStatus    int                       // the status of the last answer; 0 when none came
    LastError     string                    // Lajstrom's words, at most 1024 bytes
    CreatedAt     time.Time
    DeliveredAt   *time.Time
}
type WebhookClaim struct {                  // what ClaimWebhookDeliveries returns: the delivery and its webhook
    Delivery WebhookDelivery
    Webhook  Webhook
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
// PackageOf is GetPackageByNameOCI at the package path of an OCI repository, path, or at each
// other form of it (profile.PackagePathForms): the package whose versions may be there, its
// NameOCI or, in a split, the OCI repository of some of its versions (index-schema.md,
// Invariants). ErrNotFound when none is.
func PackageOf(ctx context.Context, r Reader, repoID int64, path string) (*Package, error)
// ManifestPath is the package path of the OCI repository that holds v's manifest: v.OCIRepo
// (versions.oci_repo, a split), or else p.NameOCI. Downloads, deletes, the /v2 tag list and
// reindex's recheck go there.
func ManifestPath(p Package, v Version) string

// WithRepeatableRead returns ctx for a Store.Tx that does not lose a concurrent update (#791):
// fn reads one snapshot, and when a row it writes was changed by a transaction that committed
// after that snapshot, the database aborts the transaction and Tx runs fn again from the start,
// up to four times in all; the last error wraps ErrUnavailable. fn must start from what it
// reads on its Writer and leave nothing outside the transaction. Postgres and YugabyteDB:
// REPEATABLE READ, run again on 40001 and 40P01; CockroachDB: its REPEATABLE READ, or
// SERIALIZABLE where the cluster has that off, run again alike; SQLite serializes its writes,
// and fn runs once. Without it Tx is read committed on Postgres and CockroachDB and runs fn once. The update of a repo
// uses it. IsRepeatableRead reports it, for the implementations of Tx.
func WithRepeatableRead(ctx context.Context) context.Context
func IsRepeatableRead(ctx context.Context) bool

// Migrations: migrations/<dialect>/NNNN_<name>.sql, embedded (index-schema.md, Migrations).
// DialectCockroach is not a value of database.driver: the postgres driver selects it on
// CockroachDB.
const DialectSQLite, DialectPostgres, DialectCockroach = "sqlite", "postgres", "cockroach"
func MigrationVersions(dialect string) ([]int, error)                 // embedded, ascending
// ApplyMigrations applies the pending migrations, each in a transaction under the migration
// lock that records it in schema_migrations (on CockroachDB one statement at a time, under a
// lock held across them), and reports those it applied (empty: current).
// It refuses a schema that requires a newer binary (ErrSchemaTooNew) and, unless
// opts.Breaking, a pending migration that requires more than the schema has
// (ErrBreakingMigration), applying nothing (index-schema.md, Upgrades). A migration whose
// transaction the database aborts on a catalog conflict (40001; 42710 from the create of
// schema_migrations: YugabyteDB) runs again from the start, up to five times in all, with a
// warning on opts.Log each time (index-schema.md, Migrations).
func ApplyMigrations(ctx context.Context, db *sql.DB, dialect string, opts MigrateOptions) (MigrationReport, error)
type MigrateOptions struct {
    Breaking bool                           // apply a migration that requires more than the schema has
    Log      *slog.Logger                   // the retries of a migration; nil: slog.Default()
}
type MigrationReport struct {
    Applied []int                           // applied by this run, ascending
    Version int                             // newest in schema_migrations after the run
    Known   int                             // newest embedded; Version > Known: a newer schema it runs on
}
var ErrSchemaTooNew, ErrBreakingMigration error
func AppliedMigrations(ctx context.Context, db *sql.DB) ([]int, error) // from schema_migrations

// Snapshots of the control plane (#76; index-schema.md, Snapshots of the control plane;
// manifest-profile.md §9). Not part of Store: lajstrom admin snapshot uses it, serve does not.
type Snapshotter interface {
    // ExportControlPlane: one transaction, SERIALIZABLE READ ONLY DEFERRABLE on Postgres; Schema the newest
    // migration applied, ReadAt the database's time; the audit rows of opts only
    ExportControlPlane(ctx context.Context, opts ExportOptions) (*ControlPlane, error)
    // ImportControlPlane: the rows with their ids, in one transaction, into empty tables
    // (ErrNotEmpty, naming them; opts.Force empties every table but schema_migrations,
    // auth_version and replicas first); id sequences past the greatest id (nextval), auth_version
    // incremented, opts.Event appended last; a Schema newer than the binary's ErrSchemaTooNew
    ImportControlPlane(ctx context.Context, cp *ControlPlane, opts ImportOptions) error
}
type ExportOptions struct {
    WithAudit  bool      // the audit rows whose At is at or after AuditSince (zero: every row)
    AuditSince time.Time
}
type ImportOptions struct {
    Force bool        // empty the database first instead of refusing it
    Event *AuditEvent // appended in the transaction of the import, after the rows
}
type ControlPlane struct {                  // every row with its id; no secret in clear
    Schema         int
    ReadAt         time.Time
    Repos          []Repo
    GroupMembers   []GroupMember
    Users          []User                   // live and deleted
    Identities     []Identity
    IdentityGroups []IdentityGroup          // IdentityID and Name are the row
    Tokens         []Token                  // deploy tokens over a mask too (RepoMask)
    Bindings       []Binding
    Keys           []Key
    Webhooks       []Webhook                // SecretEnc as the table has it; not their deliveries
    SealingKey     *SealingKey              // nil: none, or a snapshot before 0040 (#1288)
    WrappedMasterKeys []WrappedMasterKey    // the KMS's ciphertexts; none before 0041 (#1388)
    WithAudit      bool                     // the ExportOptions of the read
    AuditSince     time.Time
    Audit          []AuditEvent
}
type GroupMember struct {                   // a row of group_members
    GroupID, MemberID int64
    Position          int
}
var ErrNotEmpty error                       // ImportControlPlane into a control plane that has rows

// Drivers (internal/index/sqlite, internal/index/postgres): *Store implements index.Store and
// index.Snapshotter, and adds Ping(ctx) error, ApplyMigrations(ctx, MigrateOptions)
// (MigrationReport, error), AppliedMigrations(ctx) ([]int, error), and ObserveAudit(fn
// func([]AuditEvent)): fn, set before the store is used, gets after each commit the audit rows
// that the transaction inserted or whose state it changed, as committed, by id; never after a
// rollback or a commit whose result is unknown, nor for the rows of ImportControlPlane but
// opts.Event (rbac.md §5, a line per row; #1278).
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
    Attacher    Attacher                      // *ingest.Pipeline; nil: a run reads no attachments (#89)
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
// Attacher reads the attachments of a referrer as PutReferrer indexes them (#89; index-schema.md,
// Attachments): once the tags of an OCI repository are read, the run lists the referrers of each
// version of its package, reads the referrers whose rows it lacks or whose Trust is not
// AttachmentTrust(), replaces their rows, touches the others and sweeps the version's rows it
// did not see.
type Attacher interface {
    InspectAttachments(ctx context.Context, repo index.Repo, v index.Version, r ingest.Referrer) ([]index.Attachment, error)
    AttachmentTrust() string
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
// whose provider keeps the tag schema (Capabilities.TagSchema), asks ingest to repair the repo's sha256-<hex> indexes
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
    Attachments     int       `json:"attachments"`     // attachment rows seen or written (#89)
    Foreign         []Finding `json:"foreign"`
    OrphanReferrers []Finding `json:"orphanReferrers"`
    OrphanJournal   []Finding `json:"orphanJournal"`   // npm and pypi journal entries of a repository without versions
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
    IndexedTags []string `json:"indexedTags"` // the tags of its versions at Indexed
    Other       string   `json:"other"`       // another form of it, below the repo's base
    OtherTags   []string `json:"otherTags"`   // the tags of the profile manifests there: indexed with oci_repo, but one also in IndexedTags
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
    Trust             *attest.Trust                    // attachments.trust (#89); nil: no key is trusted
    MaxAttachmentSize int64                            // attachments.maxBlobSize: the largest layer read to judge an attachment; zero: 16 MiB
    Webhooks  *webhook.Outbox                          // nil: no events; else the events of each publish and version delete, in the transaction of its audit row (webhooks.md §4)
}
// New returns a Pipeline, which implements format.Publisher (Upload, UploadStream, Delete,
// Journal) and format.Sessions.
func New(o Options) *Pipeline

// Ingest stores a manifest the facade received for <format>/<repo>/<pkgPath>:<tag>; raw defines
// the digest. Errors wrap ErrInvalid (profile, plugin, the plugin's Admit, missing blobs, a
// lajstrom.origin.* key, which only Upload with an Origin writes) or ErrConflict (tag, or the
// package path of another spelling). On a registry with Capabilities.TagsFoldCase the claim on
// the tag is the tag lower-cased, and a tag the registry resolves to a version of the package
// whose tag differs from it in case only is ErrConflict naming that version (manifest-profile.md
// §4.2). Inspect reads the lajstrom.origin.* keys as any annotation. A push the registry
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
// Explain (format.Explainer): "the registry took <Name>@<Digest> and does not serve <Missing>
// yet; the same request again completes it", naming at most 20 blobs and then "<n> more
// blobs", as the facade's answer does (#693): the frontends' and the API's 503 (#667).
func (e *NotVisibleError) Explain() string
// Upload is the frontend path: Plugin.Build → push missing blobs → store as Ingest does. With
// up.Origin (the proxy fetcher, ADR 0013) it sets lajstrom.origin.url and lajstrom.origin.checksum
// (none for a file fetched unchecked) on the built manifest and removes lajstrom.uploaded-by, and the audit row is proxy.fetch with
// details {"upstream", "url", "checksum", "size"}, size the sum of the file layers'; without one,
// a built manifest with a lajstrom.origin.* key is ErrInvalid. Upload, Delete and Journal made
// with a context of format.WithVia (a write through a group, a fetch of a group's proxy member)
// add details.via, <format>/<group>, to their audit rows (rbac.md §7); nothing else changes.
func (p *Pipeline) Upload(ctx context.Context, repo index.Repo, up format.Upload, principal auth.Principal) (*format.Published, error)

// PutReferrer stores a referrer (a signature, an SBOM, an attestation: profile.IsReferrer, or a
// manifest with a subject on an oci repo) as it came, and indexes nothing but the attachments of
// a version (#89). On a managed repo the
// subject must be a manifest of the repository, the blobs in the backend, a tag cosign's
// (ErrInvalid otherwise). On a managed repo the push runs under Store.WithReferrersLock of the
// subject, with Capabilities.Referrers or without, and the subject is resolved again under it
// (ErrInvalid when it was deleted meanwhile, #806); on an oci repo where the provider keeps the
// tag schema (Capabilities.TagSchema: no referrers API, or referrers.alwaysTag). One audit event: push, Subject the subject's digest,
// Details {"kind", "artifactType"}. Approved by the maintainer on 2026-11-09 (#28). Once the
// registry serves the referrer back, when its subject is a version of the repo, its attachments
// (InspectAttachments) replace the rows of that referrer (Store.ReplaceAttachments; for a legacy
// tag, those of the version under the tag), best effort: a failure is logged and counted, and the
// push is a success all the same (index-schema.md, Attachments).
func (p *Pipeline) PutReferrer(ctx context.Context, repo index.Repo, r Referrer, principal auth.Principal) (digest.Digest, error)
// InspectAttachments returns the rows of the referrer r of the version v of repo, as PutReferrer
// writes them, without writing them: profile.Attachments of its manifest, each layer read through
// the provider (at most MaxAttachmentSize; a larger one is unsupported) and judged by the trust
// (attest.Trust.Verify), the predicate type and kind those of its signed statement where there is
// one. A layer the registry does not serve is an error. AttachmentTrust is the Hash of the trust.
func (p *Pipeline) InspectAttachments(ctx context.Context, repo index.Repo, v index.Version, r Referrer) ([]index.Attachment, error)
func (p *Pipeline) AttachmentTrust() string
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
// sha256-<hex>, the referrers the backend lists; where the provider keeps the tag schema
// (Capabilities.TagSchema) under Store.WithReferrersLock of the subject. The first request that
// changes anything is a plain delete, so a backend that refuses deletes changes nothing (a
// referrer deleted on its own where the provider keeps the tag schema: its subject's
// sha256-<hex> index goes first and is pushed again without it). Then the manifest, then one index
// transaction: the version's rows, its package's when it was the last, index_cache of the repo,
// the audit row (delete, Details {"referrers": n}); a referrer's attachment rows in the
// transaction of its audit row (Store.DeleteAttachments). On an oci repo a delete by tag of a manifest
// that other tags also name is refused before any request. With del.Condition it is a
// compare-and-delete of a version (format.Condition), checked under the locks: a failed one
// deletes nothing and writes no audit row. Errors wrap provider.ErrUnsupported,
// index.ErrNotFound, index.ErrConflict (other tags name the manifest; the error's OtherTags()
// []string lists them, #284; or an attachment of the same version names the referrer as its
// input, which the error's ReferencedBy() []digest.Digest lists, #89), provider.ErrConflict (the backend refused this one delete),
// provider.ErrUnavailable (the registry did not take a referrer's subject index again, and the
// referrer stays), or a *format.RefusedError (a condition failed).
func (p *Pipeline) Delete(ctx context.Context, repo index.Repo, del format.Deletion, principal auth.Principal) (*format.Deleted, error)

// RepairReferrers is how reindex repairs the registry (#628; guarantees.md INV-35): where the
// provider keeps the tag schema (Capabilities.TagSchema), it pushes again the sha256-<hex> index of each subject of found
// (manifest digest → package paths of the OCI repositories a run found it in) that lacks
// referrers the audit log has as pushed to repo (the push rows whose subject is a digest, read
// once) and the registry still has with that subject, or that is missing while there are such:
// read again under Store.WithReferrersLock of the subject, which must still be in the
// repository, then pushed with them after those it lists, as Delete's repair does. One audit
// event per index pushed: referrers.repair by principal, Subject the subject's digest, Digest the
// index's, Details {"path", "added"}, pending before the push and completed after it. A backend
// with the API and without referrers.alwaysTag, or an Options.Audit writer, repairs nothing. One entry per index found lacking:
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

### Attachment verification (`internal/attest`, #89)

This section describes and does not bind, as the next one: ingest's `Options.Trust` is the one
use. Keys are verified with the standard library (`crypto/ecdsa`, `crypto/ed25519`,
`crypto/rsa`, `crypto/x509`, `encoding/pem`), keyless bundles with `sigstore-go` (§3; its
`verify`, `bundle` and `root` packages). The package does no I/O: the caller reads the layer and
the trusted root, and a verification makes no network request.

```go
type Entry struct {                         // one attachments.trust entry
    Name           string                   // signedBy of what its key verifies
    PublicKey      []byte                   // PEM PUBLIC KEY (PKIX): ECDSA P-256/384/521, Ed25519, RSA
    PredicateTypes []string                 // empty: every predicate type, signatures included
}
type Identity struct {                      // one attachments.keyless.identities entry
    Name           string                   // signedBy of what it verifies; unique among keys and identities
    Issuer         string                   // the OIDC issuer of the Fulcio certificate, exactly
    Subject        string                   // its subject alternative name, exactly; or
    SubjectRegex   string                   // a Go regular expression it matches; one of the two
    PredicateTypes []string                 // empty: every predicate type, signatures included
}
type Keyless struct {                       // attachments.keyless
    TrustedRoot    []byte                   // a trusted_root.json (sigstore's TrustedRoot, media type v0.1)
    Identities     []Identity
    SignedCertificateTimestamps int         // the SCTs a certificate must carry; 0 for a Fulcio without a CT log
}
// NewTrust parses the entries, and with WithKeyless the trusted root and the identities: a key
// or a trusted root that does not parse, a regular expression that does not compile, a name
// given twice or empty is an error. No entry is a trust in which nothing verifies.
func NewTrust(entries []Entry, opts ...Option) (*Trust, error)
func WithKeyless(k Keyless) Option
// Hash is a hex digest of the entries (names, keys' PKIX bytes, predicate types), the identities,
// the trusted root's bytes and the SCT threshold, the trust column of attachments: rows labelled
// under another hash are judged again by reindex.
func (t *Trust) Hash() string
// PredicateTypes answers the predicate types the key or identity name is trusted for (nil:
// every type), and false when the trust has no such name (policy.md §11, #1287).
func (t *Trust) PredicateTypes(name string) ([]string, bool)
type Input struct {
    Subject   digest.Digest                 // the version's manifest
    Layer     ocispec.Descriptor            // with its annotations: a legacy signature's is there
    Content   []byte                        // the layer's bytes, as its digest says
    Annotated string                        // the predicate type cosign's annotations name, "" when none
}
type Result struct {
    Verification  string                    // index.Attachment{Verified,Untrusted,Unsigned,Invalid,Unsupported}
    SignedBy      string                    // the key's or identity's name when verified
    Reason        string                    // why not, in words
    PredicateType string                    // the statement's when there is one, else Annotated
    Predicate     json.RawMessage           // the statement's predicate, nil when there is none
}
// Verify judges one attachment (manifest-profile.md §6.3). It never returns an error: what does
// not parse is invalid, what it cannot check unsupported. It makes no network request.
func (t *Trust) Verify(in Input) Result
```

`profile.PredicateTool(predicateType, predicate)` returns the tool, its version and the database
a predicate names (manifest-profile.md §6.2), for the rows whose annotations leave them out.
`lajstrom admin trusted-root fetch --out <file> [--tuf-url <url>]` fetches a `trusted_root.json`
through TUF, sigstore-go's embedded root of the public-good instance by default, validates it and
writes it: the one place where anything about keyless verification touches the network.

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

type Action string                          // ActionRead, ActionWrite, ActionAttach, ActionDelete, ActionManage
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
    // Refresh redeems a login's refresh token at the token endpoint (rbac.md §8, #1282) and
    // verifies the answer's ID token, when there is one, but for the nonce, against the issuer
    // and subject of the identity. Errors wrap ErrRefused for invalid_grant only, which ends the
    // login; every other failure, an ID token that does not verify included, ends nothing.
    Refresh(ctx context.Context, refreshToken, subject string) (*Refreshed, error)
}
type Identity struct {
    Issuer, Subject string                  // iss and sub of the ID token
    Username        string                  // the claim auth.oidc.usernameClaim, unchecked
    Groups          []string                // the claim auth.oidc.groupsClaim, the names of rbac.md §8 kept
    RefreshToken    string                  // the exchange's refresh_token, "" without one (#1282)
}
type Refreshed struct {
    RefreshToken string                     // the new refresh token, "" when the IdP sent none
    Identity     *Identity                  // the answer's ID token, nil without one
}
var ErrRefused, ErrUnavailable error
```

The group of a principal is `auth.KindGroup`; the credential of an OIDC login's request is
`auth.CredOIDC`, which may mint a personal access token as a password may (`MayCreatePAT`).

### Webhooks (`internal/webhook`)

What the contract packages use of it; like the sections on `internal/auth` and
`internal/audit`, it describes and does not bind.

```go
// package webhook (webhooks.md): what ingest and api.Repos use of it. A nil *Outbox writes
// nothing, so that a caller without webhooks needs no check.
type Outbox struct {
    Source string                           // server.externalURL, the base of every event's source
    Now    func() time.Time                 // nil: time.Now
}
// Enqueue writes on w, in the caller's transaction (the one of the event's audit row), one
// delivery per active webhook whose mask matches ev.Repo and whose events include ev.Type,
// with the body of webhooks.md §2; nothing when none matches.
func (o *Outbox) Enqueue(ctx context.Context, w index.Writer, ev Event) error
type Event struct {
    Type    string                          // TypeVersionPublished, TypeVersionDeleted, TypeRepoCreated, TypeRepoDeleted
    Repo    index.Repo                      // Format, Name and Kind are read
    Subject string                          // the audit row's subject; "" for a repo
    Data    any                             // the data of webhooks.md §3 (VersionData, RepoData), marshalled as it is
}
```

### Policy (`internal/policy`, #101)

What the contract packages use of it ([policy.md](policy.md)). `format`, `ingest`, `proxy`,
`facade` and `api` import it; it imports `index`, `auth`, `audit`, `attest`, `config` and
`oci/profile` (`profile.IsType`, the match of a predicate type, #1287), and nothing of theirs: the
order of a format's versions comes in as a function.

```go
// package policy
type Subject struct {
    Repo      index.Repo                    // the repo that holds it: a group's member
    Name      string                        // lajstrom.name; an oci repo's path
    Version   string                        // an oci repo's tag, "" for a digest
    Variant   string
    Digest    digest.Digest                 // the manifest's; "" for a coordinate the repo does not hold
    Published time.Time                     // the quarantine's clock as the caller knows it; zero: now
    NoRequirements bool                     // an upload, a push, a producer's read of /v2 (policy.md §3)
}
type Verdict struct {                       // a decision (policy.md §3)
    Effect      string                      // EffectAllow, EffectDeny, EffectQuarantine
    Reason      string
    Rule        int64                       // the rule that decided; 0 when none
    Attestation digest.Digest               // the referrer of the verdict that decided; "" when none
    Source      string
    Lifts       time.Time                   // the end of a quarantine
    Until       time.Time                   // when it may change by the clock alone; zero: never
    Requirement *index.Requirement          // the requirement not met that decided (#1287)
}
func (v Verdict) Allowed() bool
// DeniedError is a refusal; it wraps ErrDenied, which format.StatusOf answers 403 and the facade
// DENIED. Refusal(s, v) is nil for an allow.
type DeniedError struct{ Subject Subject; Verdict Verdict }
var ErrDenied = errors.New("policy: denied")

type VersionOrder func(a, b string) (int, error)
type Options struct {
    Index  index.Store
    Order  func(format string) VersionOrder  // format.VersionComparer of a format; nil: none
    Audit  audit.Writer                      // nil: Index.AppendAudit
    Origin func(ctx context.Context) (addr, requestID string) // server.ClientAddr, server.RequestID
    Now    func() time.Time
    Log    *slog.Logger
    Trust  *attest.Trust                     // attachments' trust: a name it lacks meets nothing (#1287)
}
func New(o Options) *Decider                // a nil *Decider allows everything
// Decide reads the rules once per policy_state.generation, the groups of the subject's repo when
// a mask may name one, the verdicts of its digest, and for a quarantine its policy_seen row.
func (d *Decider) Decide(ctx context.Context, s Subject) (Verdict, error)
func (d *Decider) Check(ctx context.Context, s Subject) error // Decide as a *DeniedError
// View reads the rules, the groups and the verdicts that may hold in repo once, through r.
func (d *Decider) View(ctx context.Context, r index.Reader, repo index.Repo) (*View, error)
func (v *View) Decide(s Subject) Verdict
func (v *View) Reader(r index.Reader) index.Reader // without the versions it refuses (policy.md §6.1)
func (v *View) Until() *time.Time                  // index_cache.expires_at; nil: none
// Refuse appends the audit row policy.refuse of a refusal on surface (policy.md §6.6).
func (d *Decider) Refuse(ctx context.Context, surface string, err *DeniedError, principal auth.Principal)
func NewContext(ctx context.Context, d *Decider) context.Context
func FromContext(ctx context.Context) *Decider
// ParseRule checks a rule as policy.md §1.3 does; the error is a *RuleError naming the field.
func ParseRule(r index.PolicyRule, now time.Time, order func(format string) VersionOrder, formats func(string) bool) error
// Invalidate invalidates the index_cache of every repo mask matches, a group's members included.
func Invalidate(ctx context.Context, w index.Writer, mask string) error
func Quarantine(repo index.Repo) time.Duration  // policy.quarantineNewVersions of a proxy repo
func EnforcePull(repo index.Repo) bool          // policy.enforcePull
func RequireAttachments(repo index.Repo) []index.Requirement // policy.requireAttachments (#1287); nil: none
// CheckRequirements checks requirements as policy.md §11 does for repo, against trust; the error
// is a *RuleError naming the field (requireAttachments[1].signedBy[0])
func CheckRequirements(reqs []index.Requirement, repo index.Repo, trust *attest.Trust) error
// CheckPredicateType checks a requirement's predicate type, and that of ?unmet= (http-routing.md
// §4): an absolute URI of at most 256 bytes; MaxSigners bounds the names of both
func CheckPredicateType(t string) error
const KeyRequireAttachments, PolicyKeySigner = "requireAttachments", "lajstrom"
const MaxRequirements, MaxSigners = 16, 16
```

The contract packages grow these, each nil or absent for no policy:

- `format.Deps.Policy *policy.Decider`, which the repo middleware of `NewRouter` puts in the
  request's context (`policy.NewContext`). `format.CachedDocument` renders through the View of
  the context's Decider and stores `View.Until` as each document's `ExpiresAt`;
  `format.ServeBlobOptions.Subject *policy.Subject` is decided first by `ServeBlob`
  (`format.CheckVersion`, which writes the audit row of a refusal); `format.VersionSubject`
  makes the subject of a version.
- `format.VersionComparer`, a `Plugin` with `CompareVersions(a, b string) (int, error)` (npm,
  maven, pypi, apt, rpm); `format.Trimmer`, a `Proxier` with `Trim` (proxy-and-group.md §10; npm);
  `format.Locator`, a `Plugin` with `DownloadPrefix(repo index.Repo, name, version string)
  (string, bool)`, the path of policy.md §8 (pypi, maven, npm).
- `index.PolicyRule`, `index.Verdict`, `index.VerdictFilter`, `index.PolicySeen`;
  `Attachment.Verdict *Verdict` (a verified verdict's row, written by `ReplaceAttachments` with
  its attachment); `Cache.ExpiresAt`. The Reader: `ListPolicyRules`, `GetPolicyRule`,
  `PolicyGeneration`, `ListVerdicts`, `GetPolicySeen`. The Writer: `CreatePolicyRule`,
  `DeletePolicyRule`, `ReplaceOCIVerdicts`, `DeleteOCIVerdicts`, `TouchOCIVerdicts`,
  `SweepOCIVerdicts`, `PutPolicySeen` (insert if absent). `ControlPlane.PolicyRules`.
- `ingest.Options.Policy`, decided in `ingest.decide` between `validate` and `claimTag`;
  `ingest.ErrDenied` is `policy.ErrDenied`. `Pipeline.Attach` (a referrer with its blobs),
  `AttachVerdict` (the bundle and the legacy attestation), `RemoveVerdicts` and `LiftQuarantine`
  store and remove the verdicts the server makes, signed by an
  `ingest.VerdictSigner` (`Sign`, `PublicDER`: the policy key); `InspectVerdicts` reads an oci
  repo's, which `reindex.VerdictInspector` asks for.
- `proxy.Options.Policy` and `Options.Lifted` (the allow verdict of a quarantine over);
  `facade.Options.Policy`; `api.Options.Policy`.
- `keys.Service.PolicyKey(ctx, uri)` returns the `*keys.PolicyKey` (`Sign`, `PEM`, `PublicDER`,
  `Fingerprint`); `attest.BuildVerdict` and `attest.ParseVerdict`; `webhook.Event.Mask` and
  `webhook.Covers` for the events of a rule.

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
  of one new package deadlock on them. On CockroachDB, which the Postgres driver tells by the
  `crdb_version` it reports (#617, experimental), the dialect differs in four things: its
  migrations are its own (`migrations/cockroach`, `index.DialectCockroach`), applied one
  statement at a time (index-schema.md, Migrations); every transaction the store begins asks
  for read committed, which CockroachDB's are not by default, and `Open` refuses a cluster that
  would run them serializable instead; the transaction of a store method of its own, not
  `Store.Tx`'s, that CockroachDB aborts (`40001`) runs again, four times in all, because its
  read committed locks the row of a unique key that `INSERT … ON CONFLICT DO NOTHING` finds,
  and two writers that then delete or update that row deadlock, where Postgres takes no such
  lock (measured: `ClaimFetch` taking over a stale claim, 8 writers); and `40003`, a commit
  whose result is unknown, wraps `ErrUnavailable` and is never run again.
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
  marks it (#577). A key may be held in a KMS (ADR 0020): the `Signer` sends the digest there,
  checks the answer with the stored public key, and fails with `format.ErrUnavailable` (503)
  while the KMS does not answer, so that a route never serves an unsigned document.
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
  hosted member's document is its `index_cache` row of scope `Key(name)`, or of the scope that
  `format.HostedScoper` maps it to: pypi keeps a project's page under `simple/<project>` beside
  the project list under `simple`, which is also the key of a project named `simple`, so its
  groups (#68) add the hook. It is an interface of its own, a `Grouper` with one method, so that
  npm's plugin, whose scope is the key, stays as it is.
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
  An `oci` proxy has no plugin, and the core prefetches its images itself (proxy-and-group.md §11.10).
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
- **An apt group renders its own documents, through `format.IndexGrouper`** (#68), rather than
  through `Grouper` and the core. A suite is one render whose `Release` lists the hash of every
  index and a key signs; the merge of a Packages per request would compress and hash every index
  of the suite, and sign, on each `GET` (a suite of 20,000 packages: about 0.8 s of CPU for its
  Packages and Contents of one architecture, and a KMS call). And a pool file names no suite, so
  `FileRequest` cannot name the version that holds it. `IndexGrouper` is not a `Proxier`, since
  apt has no proxies: the core's hooks need an upstream, and a group that the core does not
  serve needs none of them. The documents are kept under the group, and the store invalidates
  them with a member's, so that no handler has to know the groups of a repo.
- **The npm journal has store methods of its own** (ADR 0012, #307). `ListJournal` and
  `ListJournalState` read one package: the journal writer reads the last `seq` and the state
  under the package's journal lock, a packument reads both for its `dist-tags`, `deprecated`,
  `_rev` and `time.modified`, a pypi project page and the API's version resource read the state
  for the yanks (#81), and reindex reads a package's entries once per OCI repository and
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
- **The admin's lists and the repo's counts are methods of their own** (#957, #959, #960).
  `ListTokenPage` comes beside `ListTokens`, whose callers want one owner's tokens whole, and
  `ListOpenSessions` beside `GetSession`; `RepoStats` takes the ids of every repo of a list, so
  that the list costs a fixed number of statements, not one per repo. `provider.Reporter` is
  optional, as `Pinger` is: the API answers a provider without it with no vendor and no
  capabilities, and a closed breaker.

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
| AWS (ECR adapter, KMS keys) | `github.com/aws/aws-sdk-go-v2` (`ecr`; `config` for the credential chain; `aws/signer/v4`, the SigV4 of the KMS client of `internal/keys/awskms`, which needs no `service/kms`: ADR 0020), `github.com/aws/smithy-go` (its API errors) | Apache-2.0 |
| Metrics | `github.com/prometheus/client_golang`; `github.com/prometheus/client_model` in tests | Apache-2.0 |
| Tests | `testing` + `github.com/google/go-cmp`; `github.com/stretchr/testify` acceptable | BSD-3 / MIT |
| JWE (the wrapped keys of encrypted repos, #114) | `github.com/go-jose/go-jose/v4`, in the module graph already (sigstore-go and grpc require it): no new module. Only `internal/blobcrypt` imports it. The blob cipher is the standard library's (ADR 0009, Amendment) | Apache-2.0 |
| Sigstore verification (keyless attachments, #89) | `github.com/sigstore/sigstore-go` (v1.4.0, 2026-10-29) and what it brings: `sigstore/protobuf-specs`, `sigstore/sigstore`, `sigstore/rekor`, `sigstore/rekor-tiles/v2`, `sigstore/timestamp-authority/v2`, `theupdateframework/go-tuf/v2`, `in-toto/in-toto-golang`, `in-toto/attestation`, `secure-systems-lab/go-securesystemslib`, `google/certificate-transparency-go`, `transparency-dev/*`, `digitorus/timestamp`, the `go-openapi` modules of Rekor's client types, `google.golang.org/protobuf` and `grpc`. Only `internal/attest` imports it (and the `trusted-root` command its `tuf` and `root`). Approved by the maintainer on 2026-11-14 (#89, option A) | Apache-2.0; its tree Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause (`make licenses`) |

GCP has no client library: the metadata server (`internal/gcpauth`) and Cloud KMS
(`internal/keys/gcpkms`) are called over `net/http` (ADR 0020).

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
    type: generic                           # generic | ecr | harbor
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
  masterKeyDir: ""                          # a key per file; the index names which seals (#1288)
  masterKeyKMS: []                          # {id, kms}: data keys wrapped by a KMS key (#1388)
  tokenAlgorithm: ES256                     # ES256 | EdDSA; the bearer-token key (ADR 0010)
  algorithm: ed25519                        # ed25519 | rsa4096 (apt before 2.0)
  retireGrace: 720h                         # a retired key still signs and stays in key.asc this long
  kms: ""                                   # awskms:///<key> | gcpkms://<version>: repo keys made from now on (ADR 0020)
attachments:                                # the verification of attachments (#89, manifest-profile.md §6.3)
  maxBlobSize: 16777216                     # the largest layer read to judge one, in bytes
  trust:
    - name: scanner                         # signedBy of what its key verifies; unique
      publicKeyFile: /etc/lajstrom/scanner.pub # PEM PKIX, or publicKey: inline PEM; one of the two
      predicateTypes: [https://cyclonedx.org/bom, https://cosign.sigstore.dev/attestation/vuln/v1] # empty: every type
  keyless:                                  # cosign keyless (Fulcio and Rekor), verified offline
    trustedRoot: /etc/lajstrom/trusted_root.json # required with identities; lajstrom admin trusted-root fetch writes one
    signedCertificateTimestamps: 1          # SCTs a certificate must carry; 0 for a Fulcio without a CT log
    identities:
      - name: release                       # signedBy of what it verifies; unique among keys and identities
        issuer: https://token.actions.githubusercontent.com
        subject: https://github.com/acme/app/.github/workflows/release.yml@refs/heads/main # or subjectRegex
        predicateTypes: [https://slsa.dev/provenance/v1]
policy:                                     # policy.md (#101)
  signingKey: local                         # the policy key: local (sealed with the master key) or a KMS key URI of ADR 0020
  denylist:
    registry: ""                            # where the deny list is written; "": the first of registries
webhooks:                                   # webhooks.md (#77): the delivery of every replica
  allowedHosts: []                          # hosts dialed as they are: http and private addresses allowed
  timeout: 10s                              # one attempt; at most 60s
  maxAttempts: 8                            # then failed
  retention: 168h                           # delivery-log rows older are deleted
  workers: 4                                # deliveries in flight per replica; 0: none on this replica
repos:                                      # optional declarative bootstrap: created when missing, never changed
  - { name: ubuntu-internal, format: apt, registry: local, anonymousRead: false }
```

The shape is a sketch: later keys and sections (`upstreams`, `ingest`, `reconcile`,
`server.tls`, …) are in configuration.md, which has every key.

Precedence: defaults < file < the nine `LAJSTROM_*` variables of configuration.md's Env column.
Secrets never go into the file: `passwordEnv` and `masterKeyEnv` name environment variables,
`keys.masterKeyFile`, `upstreams[].auth.passwordFile` and `tokenFile` and `auth.bootstrap.tokenFile`
name files, and `keys.masterKeyDir` a directory of them.

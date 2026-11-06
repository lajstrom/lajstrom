# Spec: repository layout, package contracts, approved dependencies

- Status: normative for the MVP (2026-11-06). The Go interfaces below are sketches; the skeleton
  pull request (M0-01) fixes their final shape, after which they are contracts: changing an
  exported interface in `internal/{format,provider,index,ingest}` needs an issue labelled
  `contract` and a maintainer's approval.
- Why this exists: three people work in parallel with AI assistants. Agreed package boundaries
  and interfaces are what keeps their pull requests from colliding.

## 1. Layout

```
cmd/lajstrom/                 main; subcommands: serve, reindex, migrate, version, admin (user/token/repo/binding)
internal/config/              config file (YAML) + env overrides; validation; example in deploy/
internal/server/              http.Server, routing (net/http ServeMux, Go 1.22 patterns), middleware:
                              request id, logging, metrics, auth, panic recovery; mounts formats, facade, api
internal/facade/              OCI distribution API (/v2): proxy, token endpoint, manifest PUT hook
internal/format/              Plugin interface, registry of plugins, shared helpers (Router, Deps)
internal/format/raw/          raw plugin: parse, routes, index
internal/format/pypi/         pypi plugin: wheel/sdist metadata, simple index, upload API
internal/format/deb/          deb/apt plugin: control parsing, Packages/Release rendering, GPG, pool routes
internal/format/maven/        maven plugin: coordinates, POM, sessions, maven-metadata.xml, checksums
internal/oci/profile/         media types, annotations, Escape/EscapeComponent, manifest build+validate (pure)
internal/ingest/              pipeline (validate → parse → push → index), publication sessions
internal/provider/            Provider interface, Capabilities, errors
internal/provider/generic/    oras-go based provider for distribution, Zot, Harbor, GHCR (+ small adapters)
internal/provider/ecr/        ECR adapter: auth token, CreateRepository, DescribeRepositories
internal/index/               Store interface, models, migrations (embedded SQL), reindex worker
internal/index/sqlite/        modernc.org/sqlite implementation
internal/index/postgres/      pgx implementation
internal/auth/                principals, password hashing, PAT/deploy tokens, RBAC evaluation, bearer JWT
internal/api/                 /api/v1 management REST (repos, users, tokens, bindings), JSON
internal/keys/                server signing key, per-repo GPG keys (apt), encryption at rest
internal/audit/               audit log writer
internal/testutil/            registry/DB fixtures, fixture builders (make a .deb / .whl / .jar in-process)
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
- Context everywhere; no global state except the plugin registry.
- Logging with `log/slog`; metrics with Prometheus client; no other observability deps in the MVP.

## 2. Contracts (sketches)

### Format plugin (`internal/format`)

```go
// Plugin is one package format. Implementations are stateless; dependencies come in via Deps.
type Plugin interface {
    // Name is the URL segment and the value of lajstrom.format: "raw", "pypi", "apt", "maven".
    Name() string
    // ArtifactType is application/vnd.lajstrom.<type>.v1 ("deb" for the apt plugin).
    ArtifactType() string
    // EscapeName maps a package name to the OCI repository path below <format>/<repo>/.
    EscapeName(name string) (string, error)
    // Validate inspects a manifest that is about to be stored (oras push or Build output),
    // reads what it needs through fetch, and returns the index entry. It must detect
    // disagreement between bytes, metadata layer and annotations.
    Validate(ctx context.Context, repo index.Repo, m ocispec.Manifest, fetch BlobFetcher) (*Entry, error)
    // Build assembles a manifest (and the blobs to upload) from files received by the
    // protocol frontend (twine upload, curl -T, mvn deploy session).
    Build(ctx context.Context, repo index.Repo, in Upload) (*Built, error)
    // Routes mounts the protocol frontend under /<format>/{repo}/.
    Routes(r Router, deps Deps)
    // RenderIndex produces the protocol's index documents for a repo scope
    // (apt: suite/component/arch; pypi: project; maven: g/a) from the index DB.
    RenderIndex(ctx context.Context, repo index.Repo, scope IndexScope, st index.Reader) ([]Document, error)
}

type Entry struct {
    Name, Version, Variant string
    Attrs    map[string]string   // format-specific annotations (lajstrom.<format>.*)
    Files    []FileEntry         // one per file layer
    Metadata json.RawMessage     // content of the metadata layer
}
type FileEntry struct{ Name, MediaType string; Digest digest.Digest; Size int64; Attrs map[string]string }
type Upload struct{ Files []UploadedFile; Params map[string]string; Principal string }
type Built struct{ Manifest ocispec.Manifest; Blobs []BlobSource; Entry Entry; Tag string; RepoPath string }
type BlobFetcher func(ctx context.Context, d digest.Digest) (io.ReadCloser, error)
type Deps struct{ Index index.Store; Ingest *ingest.Pipeline; Provider provider.Provider; Auth auth.Authorizer; Keys keys.Service; Log *slog.Logger }
```

### Registry provider (`internal/provider`)

```go
type Provider interface {
    Name() string
    Capabilities(ctx context.Context) (Capabilities, error)
    // Repository returns an oras-go compatible target for one OCI repository (full path incl. prefix).
    Repository(ctx context.Context, name string) (Repository, error)
    EnsureRepository(ctx context.Context, name string) error      // no-op where create-on-push works
    ListRepositories(ctx context.Context, prefix string) ([]string, error)
    DeleteRepository(ctx context.Context, name string) error
    // BlobRedirectURL returns a URL a client may be redirected to (307) for a blob, if supported.
    BlobRedirectURL(ctx context.Context, name string, d digest.Digest) (string, bool, error)
}

// Repository is the data path; the generic implementation wraps oras-go's remote.Repository.
type Repository interface {
    content.Storage                     // Fetch, Push, Exists (blobs and manifests)
    content.TagResolver                 // Tag, Resolve
    registry.TagLister                  // Tags
    registry.ReferrerLister             // Referrers (falls back to the tag schema when unsupported)
    Delete(ctx context.Context, d ocispec.Descriptor) error
}

type Capabilities struct {
    CreateOnPush, Referrers, Catalog, DeleteManifest, ImmutableTags, BlobRedirect bool
    MaxBlobSize int64 // 0 = unknown
}
```

### Index store (`internal/index`)

```go
type Store interface {
    Reader
    Writer
    Migrate(ctx context.Context) error
    Tx(ctx context.Context, fn func(Writer) error) error
    Close() error
}
// Reader is what frontends use. Writer is what ingest, reindex and the API use.
// Models (see index-schema.md): Repo, Package, Version, File, IndexCache, Session,
// User, Token, Binding, Key, AuditEvent.
```

Both engines (SQLite, Postgres) pass the same conformance test suite in `internal/index/storetest`.

### Ingest (`internal/ingest`)

```go
type Pipeline struct{ /* provider, index, plugins, audit, clock */ }
// Ingest stores a manifest that already exists as bytes (facade hook path): validate, check blobs
// exist, PUT manifest+tag, upsert index, invalidate caches. Returns ErrConflict on tag collision.
func (p *Pipeline) Ingest(ctx context.Context, repo index.Repo, m ocispec.Manifest, raw []byte, tag string, principal auth.Principal) (*Result, error)
// Upload is the frontend path: plugin.Build → push blobs → Ingest.
func (p *Pipeline) Upload(ctx context.Context, repo index.Repo, up format.Upload, principal auth.Principal) (*Result, error)
// Sessions accumulate files for formats whose upload protocol is multi-request (maven).
type Sessions interface{ Open, Add, Commit, Abort, Expire }
```

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

## 5. Make targets (M0-01 creates the Makefile)

| Target | Does |
| --- | --- |
| `make build` | `go build -o bin/lajstrom ./cmd/lajstrom` |
| `make test` | `go test -race ./...` |
| `make lint` | golangci-lint (pinned version from CI) |
| `make check` | build + vet + test + lint + govulncheck + go-licenses, exactly what CI runs; run before every pull request |
| `make dev-up` / `make dev-down` | compose stack in `deploy/compose` |
| `make test-integration` | integration tests against the dev stack |
| `make e2e` | end-to-end suite against the dev stack |
| `make run` | `lajstrom serve --config deploy/compose/lajstrom.yaml` against the dev stack |

## 6. Configuration (shape; details in `docs/configuration.md` once M0-01 lands)

```yaml
server:
  listen: ":8080"
  externalURL: "https://art.example.com"   # used in tokens, redirects, rendered indexes
database:
  driver: sqlite                            # sqlite | postgres
  dsn: "file:lajstrom.db"
registries:
  - name: local
    type: generic                           # generic | ecr
    url: "http://zot:5000"
    prefix: "art/"
    auth: { username: "...", passwordEnv: "ZOT_PASSWORD" }
auth:
  anonymousRead: false
  tokenSigningKeyFile: /etc/lajstrom/jwt.key
keys:
  masterKeyEnv: LAJSTROM_MASTER_KEY         # encrypts GPG keys at rest
repos:                                      # optional declarative bootstrap, merged into the control plane
  - { name: ubuntu-internal, format: apt, registry: local }
```

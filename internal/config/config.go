package config

import (
	"fmt"
	"time"
)

// Config is the whole configuration file. The YAML keys are camelCase, as in
// docs/spec/layout.md §6; docs/configuration.md describes every key.
type Config struct {
	Server     Server     `yaml:"server"`
	Log        Log        `yaml:"log"`
	Database   Database   `yaml:"database"`
	Registries []Registry `yaml:"registries"`
	Upstreams  []Upstream `yaml:"upstreams"`
	Ingest     Ingest     `yaml:"ingest"`
	Reconcile  Reconcile  `yaml:"reconcile"`
	Auth       Auth       `yaml:"auth"`
	Keys       Keys       `yaml:"keys"`
	Repos      []Repo     `yaml:"repos"`
}

// Server configures the HTTP server.
type Server struct {
	// Listen is the TCP address to listen on, host:port; ":8080" listens on every interface.
	Listen string `yaml:"listen"`
	// ExternalURL is the base of every absolute URL Lajstrom renders (docs/spec/http-routing.md
	// §1): https://<host>[:port], no path, query or fragment; http only for a loopback host with
	// InsecureHTTP. Optional until a feature renders absolute URLs.
	ExternalURL string `yaml:"externalURL"`
	// InsecureHTTP allows an http ExternalURL on a loopback host, for the dev stack: clients see
	// Lajstrom over TLS only everywhere else.
	InsecureHTTP bool `yaml:"insecureHTTP"`
	// TLS makes Lajstrom terminate TLS itself, for a single host; empty when TLS terminates at
	// the edge.
	TLS TLS `yaml:"tls"`
	// ShutdownTimeout bounds the graceful shutdown: in-flight requests get this long to finish.
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout"`
	// MaxManifestSize is the largest manifest a PUT to /v2 may carry, in bytes; the facade reads
	// a manifest into memory. Load sets DefaultMaxManifestSize; zero means the default too,
	// negative is invalid.
	MaxManifestSize int64 `yaml:"maxManifestSize"`
	// MaxCreatedAhead and MaxCreatedBehind bound org.opencontainers.image.created of a version
	// pushed to /v2: at most this far after and before the server's clock, else the manifest is
	// refused (a version's publish time, which retention reads). Load sets
	// DefaultMaxCreatedAhead and DefaultMaxCreatedBehind; zero means the default too, negative
	// is invalid.
	MaxCreatedAhead  time.Duration `yaml:"maxCreatedAhead"`
	MaxCreatedBehind time.Duration `yaml:"maxCreatedBehind"`
	// MetricsListen is the address of the metrics listener, host:port: GET /metrics, and
	// /debug/pprof/ with Pprof (docs/spec/http-routing.md §5). Never the public host; empty
	// turns it off.
	MetricsListen string `yaml:"metricsListen"`
	// Pprof serves /debug/pprof/ on the metrics listener.
	Pprof bool `yaml:"pprof"`
	// StallTimeout is how long a request body or a response may make no progress, no byte read
	// from the client or taken by it, before the connection is closed. A transfer of any
	// length passes while it moves. Zero turns it off; negative is invalid.
	StallTimeout time.Duration `yaml:"stallTimeout"`
	// MaxConcurrentUploads is how many uploads (POST, PUT and PATCH to the frontends and /v2)
	// one replica runs at once; the next one is answered 503 with Retry-After. Load sets
	// DefaultMaxConcurrentUploads; zero in the file means no limit; negative is invalid.
	MaxConcurrentUploads int `yaml:"maxConcurrentUploads"`
	// UploadSessions bounds the live /v2 upload sessions of one principal on one replica.
	UploadSessions UploadSessions `yaml:"uploadSessions"`
	// TrustedProxies are the peers whose X-Forwarded-For is believed, as CIDR prefixes; a bare
	// address is one host (ADR 0011). Empty: the peer of the connection is the client.
	TrustedProxies []string `yaml:"trustedProxies"`
}

// UploadSessions bounds the live upload sessions of /v2 that one principal holds on one replica
// (docs/spec/http-routing.md §2, #246): a session is live while a request on it runs and for 5
// minutes after the last one ended. Load sets the defaults; zero in the file means no bound;
// negative is invalid.
type UploadSessions struct {
	// PerRepo bounds the live sessions of a principal in one repo.
	PerRepo int `yaml:"perRepo"`
	// PerPrincipal bounds the live sessions of a principal in all repos.
	PerPrincipal int `yaml:"perPrincipal"`
}

// TLS is the certificate of the listener when Lajstrom terminates TLS itself. Both files are
// PEM; set both or neither.
type TLS struct {
	// CertFile is the certificate chain, the server's certificate first.
	CertFile string `yaml:"certFile"`
	// KeyFile is the private key of the certificate.
	KeyFile string `yaml:"keyFile"`
}

// Log configures the process logger. The --log-level and --log-format flags override it.
type Log struct {
	// Level is debug, info, warn or error.
	Level string `yaml:"level"`
	// Format is text or json.
	Format string `yaml:"format"`
}

// Database selects the index database (docs/spec/index-schema.md).
type Database struct {
	// Driver is sqlite or postgres.
	Driver string `yaml:"driver"`
	// DSN is the data source name of the driver, e.g. file:lajstrom.db or postgres://…. Load
	// sets DefaultDatabaseDSN for sqlite when it is empty; postgres has no default.
	DSN string `yaml:"dsn"`
}

// Registry is one backend OCI registry; repos refer to it by Name.
type Registry struct {
	// Name identifies the registry in repos[].registry.
	Name string `yaml:"name"`
	// Type is generic (distribution, Zot, Harbor, GHCR) or ecr.
	Type string `yaml:"type"`
	// URL is the registry's base URL, http(s)://<host>[:port], without a path, a query or a
	// fragment: a project or an owner goes in Prefix.
	URL string `yaml:"url"`
	// Prefix is the instance prefix prepended to every OCI repository name, e.g. "art/";
	// empty for none. Only the provider sees it (docs/spec/http-routing.md §2).
	Prefix string `yaml:"prefix"`
	// Auth holds Lajstrom's own credentials for the registry.
	Auth RegistryAuth `yaml:"auth"`
	// TokenRealmHosts are the host names, besides the registry's own, that the token realm of
	// its bearer challenge may be on, so that the credentials go there: exact names or IP
	// addresses, no port and no wildcard. A realm on one of them must be https. The generic
	// provider adds the hosts of its vendor table for the registries it knows by host name
	// (GitLab.com, Docker Hub).
	TokenRealmHosts []string `yaml:"tokenRealmHosts"`
	// Timeout bounds one request up to the response headers: dial, TLS handshake and the wait
	// for the headers. It does not bound streaming a body. Load sets DefaultRegistryTimeout
	// when it is zero; negative is invalid.
	Timeout time.Duration `yaml:"timeout"`
	// Retries is the maximum number of retries of a failed idempotent request (429, 5xx but
	// 501 and 505, timeouts, connections refused, reset or closed without an answer; never
	// another 4xx), and of a blob upload from content that can be rewound, as a whole. Load
	// sets DefaultRegistryRetries when it is zero; negative disables retries.
	Retries int `yaml:"retries"`
	// Capabilities overrides what probing finds, for backends that lie or cannot be probed.
	Capabilities CapabilityOverrides `yaml:"capabilities"`
	// Uploads changes how the chunks of a /v2 upload are sent to the registry.
	Uploads RegistryUploads `yaml:"uploads"`
	// ECR configures a registry of type ecr; it must stay empty for the other types.
	ECR ECR `yaml:"ecr"`
}

// ECR configures a registry of type ecr (internal/provider/ecr, docs/registries/ecr.md): where
// its API is and how the repositories Lajstrom creates are set up. The credentials come from
// the AWS SDK's default chain (environment, shared profile, web identity as with IRSA, the
// instance or task role), never from the file: registries[].auth stays empty.
type ECR struct {
	// Region is the AWS region of the registry. Empty: the region in the URL's host
	// (<account>.dkr.ecr.<region>.amazonaws.com), else the SDK's (AWS_REGION, the profile).
	Region string `yaml:"region"`
	// Endpoint replaces the endpoint of the ECR API, an http(s) URL: a VPC endpoint, or a mock
	// such as moto in the tests. Empty: the SDK's endpoint for the region.
	Endpoint string `yaml:"endpoint"`
	// ImageTagMutability of the repositories Lajstrom creates: IMMUTABLE (empty means it),
	// IMMUTABLE_WITH_EXCLUSION, MUTABLE or MUTABLE_WITH_EXCLUSION.
	ImageTagMutability string `yaml:"imageTagMutability"`
	// ImageTagMutabilityExclusions are the wildcard filters of a *_WITH_EXCLUSION mutability,
	// such as "sha256-*"; required with one, not allowed with the others.
	ImageTagMutabilityExclusions []string `yaml:"imageTagMutabilityExclusions"`
	// Encryption of the repositories Lajstrom creates: AES256 (empty means it), KMS or
	// KMS_DSSE.
	Encryption string `yaml:"encryption"`
	// KMSKey is the KMS key of KMS or KMS_DSSE encryption, an ARN, ID or alias; empty is the
	// AWS managed key of ECR.
	KMSKey string `yaml:"kmsKey"`
	// ForceDelete makes a repository delete remove a repository that still holds images;
	// without it, ECR refuses that delete.
	ForceDelete bool `yaml:"forceDelete"`
}

// IsZero reports whether e sets nothing, as for a registry of another type.
func (e ECR) IsZero() bool {
	return e.Region == "" && e.Endpoint == "" && e.ImageTagMutability == "" && len(e.ImageTagMutabilityExclusions) == 0 &&
		e.Encryption == "" && e.KMSKey == "" && !e.ForceDelete
}

// CapabilityOverrides are the capabilities a registry configuration sets by hand. A set value
// replaces what the provider found by probing; an unset one (nil, or 0 for MaxBlobSize) leaves
// it. The fields are those of provider.Capabilities.
type CapabilityOverrides struct {
	// CreateOnPush: pushing to a new repository creates it.
	CreateOnPush *bool `yaml:"createOnPush"`
	// Referrers: the backend serves the OCI referrers API; false makes the provider use the
	// sha256-<digest> tag schema without asking.
	Referrers *bool `yaml:"referrers"`
	// Catalog: the backend lists repositories; false makes ListRepositories unsupported
	// without probing, for backends whose _catalog is missing or useless.
	Catalog *bool `yaml:"catalog"`
	// DeleteManifest: manifests can be deleted.
	DeleteManifest *bool `yaml:"deleteManifest"`
	// ImmutableTags: the backend itself refuses to re-point a tag.
	ImmutableTags *bool `yaml:"immutableTags"`
	// BlobRedirect: blob downloads may redirect; false makes OpenBlob follow every redirect
	// and stream, so that a client never gets a URL of the backend.
	BlobRedirect *bool `yaml:"blobRedirect"`
	// MaxBlobSize is the largest blob the backend accepts, in bytes; 0 leaves it unset.
	MaxBlobSize int64 `yaml:"maxBlobSize"`
	// Consistency is the backend's read-after-write model, ConsistencyStrong or
	// ConsistencyEventual; empty leaves it unset.
	Consistency string `yaml:"consistency"`
	// ConsistencyBound is the longest lag measured on an eventual backend, from the 2xx of a
	// write until every read sees it (lajstrom registry probe); 0 leaves it unset. Must not be
	// negative, nor set with consistency strong.
	ConsistencyBound time.Duration `yaml:"consistencyBound"`
	// MaxNameDepth is the most components the backend takes in a repository name below the
	// instance prefix; 0 leaves it unset. Must not be negative.
	MaxNameDepth int `yaml:"maxNameDepth"`
}

// The models of registries[].capabilities.consistency.
const (
	ConsistencyStrong   = "strong"
	ConsistencyEventual = "eventual"
)

// RegistryUploads is how the provider sends the chunks of a /v2 upload session to a registry
// (docs/configuration.md, Uploads).
type RegistryUploads struct {
	// SpoolUnknownLength spools a chunk of unknown length into a session that already holds
	// bytes to a temporary file for its one request, to send it with Content-Range, for the
	// registries that refuse it streamed: Gitea and Forgejo (400), JFrog (416). Off by default:
	// Lajstrom runs on ephemeral disks, and nothing on /v2 writes to disk unless the operator
	// asks for it (#425).
	SpoolUnknownLength bool `yaml:"spoolUnknownLength"`
	// ChunkSize is the size of the chunks, each a PATCH, of the uploads that Lajstrom streams
	// into the registry itself (ADR 0014); 0 takes the vendor table's: 4 MiB on GHCR, else
	// 8 MiB. Must not be negative nor above MaxUploadChunkSize.
	ChunkSize int `yaml:"chunkSize"`
}

// MaxUploadChunkSize bounds registries[].uploads.chunkSize, 256 MiB: what one upload holds in
// memory at most (provider.NewChunkedWriter).
const MaxUploadChunkSize = 256 << 20

// RegistryAuth holds the credentials Lajstrom uses towards a registry. The password never
// comes from the file: PasswordEnv names the environment variable that holds it, and Load
// copies its value into Password. String and GoString leave the password out, and so does
// JSON encoding.
type RegistryAuth struct {
	// Username is the user name for HTTP Basic or token authentication.
	Username string `yaml:"username"`
	// Password is the resolved value of PasswordEnv. It cannot be set in the file.
	Password string `yaml:"-" json:"-"`
	// PasswordEnv is the name of the environment variable that holds the password.
	PasswordEnv string `yaml:"passwordEnv"`
}

// String describes a without the password.
func (a RegistryAuth) String() string {
	return fmt.Sprintf("{username: %q, passwordEnv: %q}", a.Username, a.PasswordEnv)
}

// GoString implements fmt.GoStringer for %#v, without the password.
func (a RegistryAuth) GoString() string {
	return fmt.Sprintf("config.RegistryAuth{Username:%q, PasswordEnv:%q}", a.Username, a.PasswordEnv)
}

// Upstream is a registry that proxy repos of one format serve and keep what they fetched from
// (docs/spec/proxy-and-group.md §2.1, ADR 0013). It is the operator's, like a Registry: a proxy
// repo names it by Name, and the management API never takes its URL or its credentials.
type Upstream struct {
	// Name identifies the upstream in repos[].upstream.name; the grammar of registry names.
	Name string `yaml:"name"`
	// Format is the plugin whose proxy repos it serves; serve checks that the plugin has the
	// kind proxy.
	Format string `yaml:"format"`
	// URL is the base of every request, http(s), with a path or not; no credentials, query or
	// fragment. http is accepted, with a warning at start.
	URL string `yaml:"url"`
	// Auth holds Lajstrom's own credentials for the upstream, as files that serve reads at start.
	Auth UpstreamAuth `yaml:"auth"`
	// Timeout bounds the wait for the response headers (connecting and TLS at most 10 s each)
	// and every wait for the next bytes of a body. Load sets DefaultUpstreamTimeout when it is
	// zero; negative is invalid.
	Timeout time.Duration `yaml:"timeout"`
	// Retries is the number of retries of a request, as for a registry. Load sets
	// DefaultUpstreamRetries when it is zero; negative disables retries.
	Retries int `yaml:"retries"`
	// MaxMetadataSize is the largest document the upstream may answer for one name, in bytes;
	// a larger one is not cached. Load sets DefaultUpstreamMaxMetadataSize when it is zero;
	// negative is invalid.
	MaxMetadataSize int64 `yaml:"maxMetadataSize"`
}

// UpstreamAuth names the files that hold Lajstrom's credentials for an upstream: HTTP Basic
// with Username and PasswordFile, or a bearer token with TokenFile. Load does not read them;
// serve does, at start, as it reads keys.masterKeyFile.
type UpstreamAuth struct {
	// Username is the user of HTTP Basic, with PasswordFile.
	Username string `yaml:"username"`
	// PasswordFile is the file that holds the password of Username.
	PasswordFile string `yaml:"passwordFile"`
	// TokenFile is the file that holds a bearer token (Authorization: Bearer), npm's
	// _authToken. Not with Username.
	TokenFile string `yaml:"tokenFile"`
}

// Ingest configures the write path (internal/ingest).
type Ingest struct {
	// ClaimTTL is how long a publish's claim on a tag holds off a publish of other bytes to the
	// same tag; a claim older than this is stale, and the next publish takes it over
	// (docs/spec/index-schema.md, Coordination). It must outlast a manifest PUT and one index
	// transaction, not the blob uploads, which come before the claim. Must not be negative.
	ClaimTTL time.Duration `yaml:"claimTTL"`
	// VisibilityTimeout is how long a publish reads back what it pushed until the registry
	// serves it, before it answers 503 (the visibility barrier, docs/registries/README.md,
	// Consistency), twice that for a referrer; a registry's capabilities.consistencyBound
	// raises it. Must not be negative; zero means the default.
	VisibilityTimeout time.Duration `yaml:"visibilityTimeout"`
}

// Reconcile configures the reconcile loop: a reindex of every repo with prune, run by serve
// on a schedule (docs/operations.md, Reconcile), and what every prune keeps.
type Reconcile struct {
	// Interval is the time between two runs on one replica; the first comes after a random
	// part of it. 0 turns the loop off. Must not be negative.
	Interval time.Duration `yaml:"interval"`
	// GraceAge is how long after ingest wrote a row, or a run last saw its manifest in the
	// registry, prune keeps the row whatever the registry answers: the read-after-write lag of
	// the registry and of what is between (docs/spec/guarantees.md, INV-5). It holds for the
	// loop, lajstrom reindex and the API's reindex. 0 means the default; must not be negative.
	GraceAge time.Duration `yaml:"graceAge"`
}

// Auth configures authentication (docs/spec/rbac.md).
type Auth struct {
	// AnonymousRead is the server-wide switch for anonymous read: when false, no repo is
	// readable without credentials, whatever its own anonymousRead (RBAC, issue #45).
	AnonymousRead bool `yaml:"anonymousRead"`
	// Bootstrap is the one-time token that makes the first admin (docs/auth.md, Bootstrap).
	Bootstrap Bootstrap `yaml:"bootstrap"`
}

// Bootstrap configures the bootstrap token: while no enabled user has an admin binding, serve
// makes the operator's token valid on every replica, which lajstrom admin bootstrap exchanges
// once for the first admin. Once an admin exists, the token is dead.
type Bootstrap struct {
	// TokenFile is the path of a file that holds the token, 32 to 256 bytes that the operator
	// made (openssl rand -hex 32), mounted from a Kubernetes Secret or a Compose secret; env
	// LAJSTROM_BOOTSTRAP_TOKEN_FILE. serve reads it at start while no admin exists, never Load;
	// once one exists, serve warns while the file exists.
	// Empty, or a file that does not exist: there is no token, and serve warns how to make the
	// first admin. serve never makes a token and never logs one.
	TokenFile string `yaml:"tokenFile"`
	// TTL is how long a token is valid once serve made it valid; DefaultBootstrapTTL when the
	// file leaves it out. Must be positive.
	TTL time.Duration `yaml:"ttl"`
}

// DefaultBootstrapTTL is the default of auth.bootstrap.ttl: a day to exchange the token.
const DefaultBootstrapTTL = 24 * time.Hour

// Keys configures signing keys (issue #41, internal/keys): the apt keys of the repos and the
// key that signs the bearer tokens of /v2 (issue #26, ADR 0010).
type Keys struct {
	// MasterKeyEnv names the environment variable that holds the master key, which encrypts
	// signing keys at rest: 64 hexadecimal characters (32 bytes). It has two readers: the apt
	// signing keys and the bearer-token signing key. The value is read by serve and admin key
	// (keys.LoadMaster), never by Load, so it is not part of Config. Empty, or a variable that
	// is not set while the index holds no apt signing key: apt signing is off, and bearer
	// tokens are signed with an ephemeral key.
	MasterKeyEnv string `yaml:"masterKeyEnv"`
	// MasterKeyFile is the path of a file that holds the master key, in the encoding of the
	// variable, for a secret mounted as a file (a Compose secret, a Kubernetes Secret volume);
	// env LAJSTROM_MASTER_KEY_FILE. Like the variable, it is read by serve and admin key, never
	// by Load. Setting it while the variable of MasterKeyEnv is set is a configuration error.
	MasterKeyFile string `yaml:"masterKeyFile"`
	// Algorithm is the algorithm of the repo keys made from now on: ed25519 (apt 2.0 and later)
	// or rsa4096 (older apt). Keys made before keep theirs.
	Algorithm string `yaml:"algorithm"`
	// RetireGrace is how long a retired key stays in a repo's key.asc and keeps signing beside
	// the new one, after a rotation; DefaultKeysRetireGrace when the file leaves it out. Zero
	// drops a retired key at once (a compromised key). Must not be negative.
	RetireGrace time.Duration `yaml:"retireGrace"`
	// TokenAlgorithm is the algorithm of the bearer-token signing keys made from now on: ES256
	// (ECDSA P-256) or EdDSA (Ed25519). A key made before keeps its own; a rotation changes it.
	TokenAlgorithm string `yaml:"tokenAlgorithm"`
}

// The algorithms of keys.algorithm.
const (
	KeysAlgorithmEd25519 = "ed25519"
	KeysAlgorithmRSA4096 = "rsa4096"
)

// The algorithms of keys.tokenAlgorithm.
const (
	KeysTokenES256 = "ES256"
	KeysTokenEdDSA = "EdDSA"
)

// Repo declares a repo to create at startup (the config bootstrap of issue #14).
type Repo struct {
	// Name is the repo name, unique within its format.
	Name string `yaml:"name"`
	// Format is the plugin name (raw, pypi, apt, maven, npm), or oci, which has none.
	Format string `yaml:"format"`
	// Registry is the name of the registry that stores the repo's artifacts; a group has none.
	Registry string `yaml:"registry"`
	// Kind is hosted (empty means hosted), proxy or group (docs/spec/proxy-and-group.md §1).
	Kind string `yaml:"kind"`
	// AnonymousRead makes the repo readable without credentials, when auth.anonymousRead allows it.
	AnonymousRead bool `yaml:"anonymousRead"`
	// Policy is the repo's policy as the management API takes it (docs/api.md, the policy
	// keys), checked when the bootstrap creates the repo; nil means none.
	Policy map[string]any `yaml:"policy"`
	// Upstream is a proxy repo's upstream settings as the management API takes them
	// (docs/spec/proxy-and-group.md §2.2): name, one of upstreams[].name of the repo's
	// format, and the TTLs, offline, allow and deny, which the bootstrap checks. Required on a
	// proxy, refused on another kind.
	Upstream map[string]any `yaml:"upstream"`
	// Group is a group's settings as the management API takes them (docs/spec/proxy-and-group.md
	// §2.3): members, the names of 1 to 16 repos of the group's format in order, and merge, which
	// the bootstrap checks with the members' kinds. Required on a group, refused on another kind.
	Group map[string]any `yaml:"group"`
}

// The kinds of repos[].kind (docs/spec/proxy-and-group.md §1).
const (
	KindHosted = "hosted"
	KindProxy  = "proxy"
	KindGroup  = "group"
)

// Defaults that Load applies before reading the file.
const (
	DefaultShutdownTimeout  = 30 * time.Second
	DefaultMaxManifestSize  = 4 << 20          // the least the distribution spec asks a registry to take
	DefaultMaxCreatedAhead  = 5 * time.Minute  // a client's clock a few minutes off
	DefaultMaxCreatedBehind = time.Hour        // oras writes the time before it uploads the layers
	DefaultMetricsListen    = "127.0.0.1:9464" // the port of the OpenTelemetry Prometheus exporter
	DefaultStallTimeout     = time.Minute
	// DefaultMaxConcurrentUploads bounds the spooled uploads of a replica, and so its $TMPDIR,
	// out of the box (U-6 of the threat model, #246).
	DefaultMaxConcurrentUploads = 32
	DefaultLogLevel             = "info"
	DefaultLogFormat            = "text"
	DefaultDatabaseDriver       = "sqlite"
	DefaultDatabaseDSN          = "file:lajstrom.db" // sqlite's only: Load leaves a postgres DSN empty
	DefaultRegistryType         = "generic"
	DefaultRegistryTimeout      = 30 * time.Second
	DefaultRegistryRetries      = 3
	DefaultIngestClaimTTL       = 10 * time.Minute
	DefaultReconcileInterval    = time.Hour
	DefaultReconcileGraceAge    = time.Hour
	DefaultKeysAlgorithm        = KeysAlgorithmEd25519
	DefaultKeysRetireGrace      = 720 * time.Hour
	DefaultKeysTokenAlgorithm   = KeysTokenES256
)

// The defaults of server.uploadSessions (#246): a principal's live sessions of one repo, about 50
// CI runners of one credential that send five blobs each at once, and of all its repos.
const (
	DefaultUploadSessionsPerRepo      = 256
	DefaultUploadSessionsPerPrincipal = 1024
)

// DefaultIngestVisibilityTimeout is ingest.visibilityTimeout by default: it covers the lags
// reported of ECR and GHCR.
const DefaultIngestVisibilityTimeout = 30 * time.Second

// The defaults of upstreams[] that Load applies to the keys an entry leaves out
// (docs/spec/proxy-and-group.md §2.1).
const (
	DefaultUpstreamTimeout         = 30 * time.Second
	DefaultUpstreamRetries         = 2
	DefaultUpstreamMaxMetadataSize = 64 << 20
)

// Default returns the configuration before the file is read. server.listen has no default.
func Default() *Config {
	return &Config{
		Server: Server{ShutdownTimeout: DefaultShutdownTimeout, MaxManifestSize: DefaultMaxManifestSize, MetricsListen: DefaultMetricsListen,
			StallTimeout: DefaultStallTimeout, MaxConcurrentUploads: DefaultMaxConcurrentUploads,
			MaxCreatedAhead: DefaultMaxCreatedAhead, MaxCreatedBehind: DefaultMaxCreatedBehind,
			UploadSessions: UploadSessions{PerRepo: DefaultUploadSessionsPerRepo, PerPrincipal: DefaultUploadSessionsPerPrincipal}},
		Log:       Log{Level: DefaultLogLevel, Format: DefaultLogFormat},
		Database:  Database{Driver: DefaultDatabaseDriver},
		Ingest:    Ingest{ClaimTTL: DefaultIngestClaimTTL, VisibilityTimeout: DefaultIngestVisibilityTimeout},
		Reconcile: Reconcile{Interval: DefaultReconcileInterval, GraceAge: DefaultReconcileGraceAge},
		Auth:      Auth{Bootstrap: Bootstrap{TTL: DefaultBootstrapTTL}},
		Keys:      Keys{Algorithm: DefaultKeysAlgorithm, RetireGrace: DefaultKeysRetireGrace, TokenAlgorithm: DefaultKeysTokenAlgorithm},
	}
}

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
	Ingest     Ingest     `yaml:"ingest"`
	Auth       Auth       `yaml:"auth"`
	Keys       Keys       `yaml:"keys"`
	Repos      []Repo     `yaml:"repos"`
}

// Server configures the HTTP server.
type Server struct {
	// Listen is the TCP address to listen on, host:port; ":8080" listens on every interface.
	Listen string `yaml:"listen"`
	// ExternalURL is the base of every absolute URL Lajstrom renders (docs/spec/http-routing.md
	// §1). Optional until a feature renders absolute URLs.
	ExternalURL string `yaml:"externalURL"`
	// ShutdownTimeout bounds the graceful shutdown: in-flight requests get this long to finish.
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout"`
	// MaxManifestSize is the largest manifest a PUT to /v2 may carry, in bytes; the facade reads
	// a manifest into memory. Load sets DefaultMaxManifestSize; zero means the default too,
	// negative is invalid.
	MaxManifestSize int64 `yaml:"maxManifestSize"`
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
	// DSN is the data source name of the driver, e.g. file:lajstrom.db or postgres://….
	DSN string `yaml:"dsn"`
}

// Registry is one backend OCI registry; repos refer to it by Name.
type Registry struct {
	// Name identifies the registry in repos[].registry.
	Name string `yaml:"name"`
	// Type is generic (distribution, Zot, Harbor, GHCR) or ecr.
	Type string `yaml:"type"`
	// URL is the registry's base URL, http:// or https://.
	URL string `yaml:"url"`
	// Prefix is the instance prefix prepended to every OCI repository name, e.g. "art/";
	// empty for none. Only the provider sees it (docs/spec/http-routing.md §2).
	Prefix string `yaml:"prefix"`
	// Auth holds Lajstrom's own credentials for the registry.
	Auth RegistryAuth `yaml:"auth"`
	// Timeout bounds one request up to the response headers: dial, TLS handshake and the wait
	// for the headers. It does not bound streaming a body. Load sets DefaultRegistryTimeout
	// when it is zero; negative is invalid.
	Timeout time.Duration `yaml:"timeout"`
	// Retries is the maximum number of retries of a failed request (429, 5xx, network errors);
	// only requests whose body can be replayed are retried. Load sets DefaultRegistryRetries
	// when it is zero; negative disables retries.
	Retries int `yaml:"retries"`
	// Capabilities overrides what probing finds, for backends that lie or cannot be probed.
	Capabilities CapabilityOverrides `yaml:"capabilities"`
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
}

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

// Ingest configures the write path (internal/ingest).
type Ingest struct {
	// ClaimTTL is how long a publish's claim on a tag holds off a publish of other bytes to the
	// same tag; a claim older than this is stale, and the next publish takes it over
	// (docs/spec/index-schema.md, Coordination). It must outlast a manifest PUT and one index
	// transaction, not the blob uploads, which come before the claim. Must not be negative.
	ClaimTTL time.Duration `yaml:"claimTTL"`
}

// Auth configures authentication (docs/spec/rbac.md).
type Auth struct {
	// AnonymousRead is the server-wide switch for anonymous read: when false, no repo is
	// readable without credentials, whatever its own anonymousRead (RBAC, issue #45).
	AnonymousRead bool `yaml:"anonymousRead"`
	// TokenSigningKeyFile is the PEM file with the key that signs OCI bearer tokens (issue #26).
	TokenSigningKeyFile string `yaml:"tokenSigningKeyFile"`
}

// Keys configures signing keys (issue #41, internal/keys).
type Keys struct {
	// MasterKeyEnv names the environment variable that holds the master key, which encrypts
	// signing keys at rest: 64 hexadecimal characters (32 bytes). The value is read by serve
	// and admin key (keys.LoadMaster), never by Load, so it is not part of Config. Empty, or a
	// variable that is not set while the index holds no signing key: signing is off.
	MasterKeyEnv string `yaml:"masterKeyEnv"`
	// Algorithm is the algorithm of the repo keys made from now on: ed25519 (apt 2.0 and later)
	// or rsa4096 (older apt). Keys made before keep theirs.
	Algorithm string `yaml:"algorithm"`
	// RetireGrace is how long a retired key stays in a repo's key.asc and keeps signing beside
	// the new one, after a rotation; DefaultKeysRetireGrace when the file leaves it out. Zero
	// drops a retired key at once (a compromised key). Must not be negative.
	RetireGrace time.Duration `yaml:"retireGrace"`
}

// The algorithms of keys.algorithm.
const (
	KeysAlgorithmEd25519 = "ed25519"
	KeysAlgorithmRSA4096 = "rsa4096"
)

// Repo declares a repo to create at startup (the config bootstrap of issue #14).
type Repo struct {
	// Name is the repo name, unique within its format.
	Name string `yaml:"name"`
	// Format is the plugin name: raw, pypi, apt, maven.
	Format string `yaml:"format"`
	// Registry is the name of the registry that stores the repo's artifacts.
	Registry string `yaml:"registry"`
	// AnonymousRead makes the repo readable without credentials, when auth.anonymousRead allows it.
	AnonymousRead bool `yaml:"anonymousRead"`
}

// Defaults that Load applies before reading the file.
const (
	DefaultShutdownTimeout = 30 * time.Second
	DefaultMaxManifestSize = 4 << 20 // the least the distribution spec asks a registry to take
	DefaultLogLevel        = "info"
	DefaultLogFormat       = "text"
	DefaultDatabaseDriver  = "sqlite"
	DefaultDatabaseDSN     = "file:lajstrom.db"
	DefaultRegistryType    = "generic"
	DefaultRegistryTimeout = 30 * time.Second
	DefaultRegistryRetries = 3
	DefaultIngestClaimTTL  = 10 * time.Minute
	DefaultKeysAlgorithm   = KeysAlgorithmEd25519
	DefaultKeysRetireGrace = 720 * time.Hour
)

// Default returns the configuration before the file is read. server.listen has no default.
func Default() *Config {
	return &Config{
		Server:   Server{ShutdownTimeout: DefaultShutdownTimeout, MaxManifestSize: DefaultMaxManifestSize},
		Log:      Log{Level: DefaultLogLevel, Format: DefaultLogFormat},
		Database: Database{Driver: DefaultDatabaseDriver, DSN: DefaultDatabaseDSN},
		Ingest:   Ingest{ClaimTTL: DefaultIngestClaimTTL},
		Keys:     Keys{Algorithm: DefaultKeysAlgorithm, RetireGrace: DefaultKeysRetireGrace},
	}
}

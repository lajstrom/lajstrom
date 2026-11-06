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

// Auth configures authentication (docs/spec/rbac.md).
type Auth struct {
	// AnonymousRead is the server-wide switch for anonymous read: when false, no repo is
	// readable without credentials, whatever its own anonymousRead (RBAC, issue #45).
	AnonymousRead bool `yaml:"anonymousRead"`
	// TokenSigningKeyFile is the PEM file with the key that signs OCI bearer tokens (issue #26).
	TokenSigningKeyFile string `yaml:"tokenSigningKeyFile"`
}

// Keys configures signing keys (issue #41).
type Keys struct {
	// MasterKeyEnv names the environment variable that holds the master key, which encrypts
	// signing keys at rest.
	MasterKeyEnv string `yaml:"masterKeyEnv"`
}

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
	DefaultLogLevel        = "info"
	DefaultLogFormat       = "text"
	DefaultDatabaseDriver  = "sqlite"
	DefaultDatabaseDSN     = "file:lajstrom.db"
	DefaultRegistryType    = "generic"
)

// Default returns the configuration before the file is read. server.listen has no default.
func Default() *Config {
	return &Config{
		Server:   Server{ShutdownTimeout: DefaultShutdownTimeout},
		Log:      Log{Level: DefaultLogLevel, Format: DefaultLogFormat},
		Database: Database{Driver: DefaultDatabaseDriver, DSN: DefaultDatabaseDSN},
	}
}

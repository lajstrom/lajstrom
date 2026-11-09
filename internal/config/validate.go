package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// RepoNamePattern is the rule for repo names (docs/spec/manifest-profile.md §4.1), also applied
// to registry names and formats so that each is one safe URL and OCI path segment. The
// management API (internal/api) checks repo names with it too. Treat it as read-only.
var RepoNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:[-._][a-z0-9]+)*$`)

// MaxNameLen is the longest repo or registry name, in bytes (docs/spec/manifest-profile.md §4.1).
const MaxNameLen = 64

// Validate checks c and returns nil or one error per problem, joined with errors.Join. Each
// error starts with the key it is about, e.g. "repos[0].registry: …". Errors never contain
// a password.
func (c *Config) Validate() error {
	var p problems
	c.Server.validate(&p)
	c.Log.validate(&p)
	c.Database.validate(&p)
	if c.Ingest.ClaimTTL < 0 {
		p.add("ingest.claimTTL", "must not be negative")
	}
	if c.Reconcile.Interval < 0 {
		p.add("reconcile.interval", "must not be negative")
	}
	c.Keys.validate(&p)
	registries := map[string]int{}
	for i, r := range c.Registries {
		key := fmt.Sprintf("registries[%d]", i)
		r.validate(&p, key)
		if first, dup := registries[r.Name]; dup && r.Name != "" {
			p.add(key+".name", "%q is already used by registries[%d]", r.Name, first)
		} else {
			registries[r.Name] = i
		}
	}
	repos := map[string]int{}
	for i, r := range c.Repos {
		key := fmt.Sprintf("repos[%d]", i)
		r.validate(&p, key)
		if _, ok := registries[r.Registry]; !ok && r.Registry != "" {
			p.add(key+".registry", "%q is not a configured registry", r.Registry)
		}
		id := r.Format + "/" + r.Name
		if first, dup := repos[id]; dup {
			p.add(key, "%q is already declared by repos[%d]", id, first)
		} else {
			repos[id] = i
		}
	}
	return errors.Join(p...)
}

// problems collects validation errors.
type problems []error

func (p *problems) add(key, format string, args ...any) {
	*p = append(*p, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, args...)))
}

func (s Server) validate(p *problems) {
	if s.Listen == "" {
		p.add("server.listen", "must not be empty")
	} else if _, _, err := net.SplitHostPort(s.Listen); err != nil {
		p.add("server.listen", "%q is not host:port", s.Listen)
	}
	if s.ExternalURL != "" {
		u, err := url.Parse(s.ExternalURL)
		if err != nil || !isHTTP(u) || u.RawQuery != "" || u.Fragment != "" {
			p.add("server.externalURL", "must be an absolute http(s) URL without query or fragment")
		}
	}
	if s.ShutdownTimeout <= 0 {
		p.add("server.shutdownTimeout", "must be positive")
	}
	if s.MetricsListen != "" {
		if _, _, err := net.SplitHostPort(s.MetricsListen); err != nil {
			p.add("server.metricsListen", "%q is not host:port", s.MetricsListen)
		} else if s.MetricsListen == s.Listen {
			p.add("server.metricsListen", "must not be server.listen: the metrics are never on the public listener")
		}
	}
	if s.MaxManifestSize < 0 {
		p.add("server.maxManifestSize", "must not be negative")
	}
}

func (l Log) validate(p *problems) {
	switch l.Level {
	case "debug", "info", "warn", "error":
	default:
		p.add("log.level", "%q is not debug, info, warn or error", l.Level)
	}
	if l.Format != "text" && l.Format != "json" {
		p.add("log.format", "%q is not text or json", l.Format)
	}
}

func (d Database) validate(p *problems) {
	if d.Driver != "sqlite" && d.Driver != "postgres" {
		p.add("database.driver", "%q is not sqlite or postgres", d.Driver)
	}
	if d.DSN == "" {
		p.add("database.dsn", "must not be empty")
	}
}

// validate checks the keys section. The master key itself is not read here: serve and admin
// key read the file of keys.masterKeyFile or the variable keys.masterKeyEnv names; Load checks
// that not both are set.
func (k Keys) validate(p *problems) {
	if k.Algorithm != KeysAlgorithmEd25519 && k.Algorithm != KeysAlgorithmRSA4096 {
		p.add("keys.algorithm", "%q is not %s or %s", k.Algorithm, KeysAlgorithmEd25519, KeysAlgorithmRSA4096)
	}
	if k.RetireGrace < 0 {
		p.add("keys.retireGrace", "must not be negative")
	}
	if k.TokenAlgorithm != KeysTokenES256 && k.TokenAlgorithm != KeysTokenEdDSA {
		p.add("keys.tokenAlgorithm", "%q is not %s or %s", k.TokenAlgorithm, KeysTokenES256, KeysTokenEdDSA)
	}
}

func (r Registry) validate(p *problems, key string) {
	validName(p, key+".name", r.Name)
	if r.Type != "generic" && r.Type != "ecr" {
		p.add(key+".type", "%q is not generic or ecr", r.Type)
	}
	// The URL is never quoted in errors: it could carry credentials.
	u, err := url.Parse(r.URL)
	switch {
	case err != nil || !isHTTP(u):
		p.add(key+".url", "must be an absolute http(s) URL")
	case u.User != nil:
		p.add(key+".url", "must not contain credentials; use auth")
	}
	if r.Prefix != "" && (strings.HasPrefix(r.Prefix, "/") || !strings.HasSuffix(r.Prefix, "/")) {
		p.add(key+".prefix", `must not start with "/" and must end with "/" when set`)
	}
	if r.Timeout < 0 {
		p.add(key+".timeout", "must not be negative")
	}
	if r.Capabilities.MaxBlobSize < 0 {
		p.add(key+".capabilities.maxBlobSize", "must not be negative")
	}
	a := r.Auth
	if (a.Username == "") != (a.PasswordEnv == "") {
		p.add(key+".auth", "username and passwordEnv must be set together")
	}
	if a.PasswordEnv != "" && a.Password == "" {
		p.add(key+".auth.passwordEnv", "environment variable %q is not set or empty", a.PasswordEnv)
	}
}

func (r Repo) validate(p *problems, key string) {
	validName(p, key+".name", r.Name)
	validName(p, key+".format", r.Format)
	if r.Registry == "" {
		p.add(key+".registry", "must not be empty")
	}
}

func validName(p *problems, key, name string) {
	switch {
	case name == "":
		p.add(key, "must not be empty")
	case len(name) > MaxNameLen || !RepoNamePattern.MatchString(name):
		p.add(key, "%q must be lowercase letters and digits separated by single '.', '_' or '-', at most %d characters", name, MaxNameLen)
	}
}

func isHTTP(u *url.URL) bool {
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads the configuration in this order, later steps winning:
//
//  1. the defaults of Default;
//  2. the YAML file at path; unknown keys are an error; registries[] get the defaults of the
//     keys they leave empty (type, timeout, retries);
//  3. the environment overrides LAJSTROM_SERVER_LISTEN, LAJSTROM_SERVER_EXTERNAL_URL,
//     LAJSTROM_SERVER_METRICS_LISTEN, LAJSTROM_LOG_LEVEL, LAJSTROM_LOG_FORMAT,
//     LAJSTROM_DATABASE_DRIVER and LAJSTROM_DATABASE_DSN; an empty variable counts as unset, and lists (registries, repos)
//     cannot be set from the environment;
//  4. registries[].auth.passwordEnv, resolved into registries[].auth.password;
//  5. Validate.
//
// lookupEnv is os.LookupEnv in production; nil means an empty environment. Errors name the
// file and never contain a secret.
func Load(path string, lookupEnv func(string) (string, bool)) (*Config, error) {
	if lookupEnv == nil {
		lookupEnv = func(string) (string, bool) { return "", false }
	}
	c := Default()
	if err := decodeFile(path, c); err != nil {
		return nil, err
	}
	for i := range c.Registries {
		c.Registries[i].applyDefaults()
	}
	applyEnv(c, lookupEnv)
	resolveSecrets(c, lookupEnv)
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: invalid:\n%w", path, err)
	}
	return c, nil
}

// decodeFile decodes the YAML file at path into c. An empty file leaves c unchanged. A file
// that still sets auth.tokenSigningKeyFile is refused with a message of its own, so that nobody
// keeps a key file believing it is read: the bearer-token key lives in the database (ADR 0010).
func decodeFile(path string, c *Config) error {
	b, err := os.ReadFile(path) // #nosec G304 -- the operator chooses the configuration file
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		if setsTokenKeyFile(b) {
			return fmt.Errorf("config: %s: auth.tokenSigningKeyFile is gone: the key lives in the database, ADR 0010", path)
		}
		if strings.Contains(err.Error(), "into time.Duration") {
			return fmt.Errorf("config: %s: %w (a duration is written with its unit: 0s, 30s, 10m, 1h)", path, err)
		}
		return fmt.Errorf("config: %s: %w", path, err)
	}
	return nil
}

// setsTokenKeyFile reports whether the YAML document b has the key auth.tokenSigningKeyFile,
// whatever its value.
func setsTokenKeyFile(b []byte) bool {
	var doc struct {
		Auth map[string]yaml.Node `yaml:"auth"`
	}
	if yaml.Unmarshal(b, &doc) != nil {
		return false
	}
	_, ok := doc.Auth["tokenSigningKeyFile"]
	return ok
}

// applyDefaults fills the keys of r that the file left at their zero value.
func (r *Registry) applyDefaults() {
	if r.Type == "" {
		r.Type = DefaultRegistryType
	}
	if r.Timeout == 0 {
		r.Timeout = DefaultRegistryTimeout
	}
	if r.Retries == 0 {
		r.Retries = DefaultRegistryRetries
	}
}

// envOverrides are the environment variables Load reads and the keys they override.
var envOverrides = []struct {
	env string
	set func(*Config, string)
}{
	{"LAJSTROM_SERVER_LISTEN", func(c *Config, v string) { c.Server.Listen = v }},
	{"LAJSTROM_SERVER_EXTERNAL_URL", func(c *Config, v string) { c.Server.ExternalURL = v }},
	{"LAJSTROM_SERVER_METRICS_LISTEN", func(c *Config, v string) { c.Server.MetricsListen = v }},
	{"LAJSTROM_LOG_LEVEL", func(c *Config, v string) { c.Log.Level = v }},
	{"LAJSTROM_LOG_FORMAT", func(c *Config, v string) { c.Log.Format = v }},
	{"LAJSTROM_DATABASE_DRIVER", func(c *Config, v string) { c.Database.Driver = v }},
	{"LAJSTROM_DATABASE_DSN", func(c *Config, v string) { c.Database.DSN = v }},
}

func applyEnv(c *Config, lookupEnv func(string) (string, bool)) {
	for _, o := range envOverrides {
		if v, ok := lookupEnv(o.env); ok && v != "" {
			o.set(c, v)
		}
	}
}

// resolveSecrets copies the value of each registry's passwordEnv into its password. A missing
// variable leaves the password empty; Validate reports it.
func resolveSecrets(c *Config, lookupEnv func(string) (string, bool)) {
	for i := range c.Registries {
		a := &c.Registries[i].Auth
		if a.PasswordEnv != "" {
			a.Password, _ = lookupEnv(a.PasswordEnv)
		}
	}
}

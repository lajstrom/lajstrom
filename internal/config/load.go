package config

import (
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads the configuration in this order, later steps winning:
//
//  1. the defaults of Default;
//  2. the YAML file at path; unknown keys are an error;
//  3. the environment overrides LAJSTROM_SERVER_LISTEN, LAJSTROM_SERVER_EXTERNAL_URL,
//     LAJSTROM_LOG_LEVEL, LAJSTROM_LOG_FORMAT, LAJSTROM_DATABASE_DRIVER and
//     LAJSTROM_DATABASE_DSN; an empty variable counts as unset, and lists (registries, repos)
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
		if c.Registries[i].Type == "" {
			c.Registries[i].Type = DefaultRegistryType
		}
	}
	applyEnv(c, lookupEnv)
	resolveSecrets(c, lookupEnv)
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: invalid:\n%w", path, err)
	}
	return c, nil
}

// decodeFile decodes the YAML file at path into c. An empty file leaves c unchanged.
func decodeFile(path string, c *Config) error {
	f, err := os.Open(path) // #nosec G304 -- the operator chooses the configuration file
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	defer func() { _ = f.Close() }()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("config: %s: %w", path, err)
	}
	return nil
}

// envOverrides are the environment variables Load reads and the keys they override.
var envOverrides = []struct {
	env string
	set func(*Config, string)
}{
	{"LAJSTROM_SERVER_LISTEN", func(c *Config, v string) { c.Server.Listen = v }},
	{"LAJSTROM_SERVER_EXTERNAL_URL", func(c *Config, v string) { c.Server.ExternalURL = v }},
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

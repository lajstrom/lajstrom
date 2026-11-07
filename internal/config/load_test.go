package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// writeConfig writes content to a file in a temporary directory and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lajstrom.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// env returns a lookupEnv function backed by m.
func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

const minimal = "server:\n  listen: \":8080\"\n"

func TestLoadDefaults(t *testing.T) {
	tests := []struct {
		name string
		file string
	}{
		{name: "only server.listen", file: minimal},
		{name: "explicit null sections", file: minimal + "log:\ndatabase:\nregistries:\ningest:\nkeys:\nrepos:\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load(writeConfig(t, tt.file), nil)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			want := &Config{
				Server:   Server{Listen: ":8080", ShutdownTimeout: 30 * time.Second, MaxManifestSize: 4 << 20},
				Log:      Log{Level: "info", Format: "text"},
				Database: Database{Driver: "sqlite", DSN: "file:lajstrom.db"},
				Ingest:   Ingest{ClaimTTL: 10 * time.Minute},
				Keys:     Keys{Algorithm: "ed25519", RetireGrace: 720 * time.Hour, TokenAlgorithm: "ES256"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Load =\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

func TestLoadEnvOverride(t *testing.T) {
	file := `
server:
  listen: ":8080"
  externalURL: http://file.example
log:
  level: info
  format: text
database:
  driver: sqlite
  dsn: file:file.db
`
	tests := []struct {
		env  string
		val  string
		want func(*Config) string
		exp  string // expected value of want(c)
	}{
		{"LAJSTROM_SERVER_LISTEN", "127.0.0.1:9090", func(c *Config) string { return c.Server.Listen }, "127.0.0.1:9090"},
		{"LAJSTROM_SERVER_EXTERNAL_URL", "https://env.example", func(c *Config) string { return c.Server.ExternalURL }, "https://env.example"},
		{"LAJSTROM_LOG_LEVEL", "debug", func(c *Config) string { return c.Log.Level }, "debug"},
		{"LAJSTROM_LOG_FORMAT", "json", func(c *Config) string { return c.Log.Format }, "json"},
		{"LAJSTROM_DATABASE_DRIVER", "postgres", func(c *Config) string { return c.Database.Driver }, "postgres"},
		{"LAJSTROM_DATABASE_DSN", "postgres://db/lajstrom", func(c *Config) string { return c.Database.DSN }, "postgres://db/lajstrom"},
		{"LAJSTROM_SERVER_LISTEN", "", func(c *Config) string { return c.Server.Listen }, ":8080"}, // empty = unset
		{"LAJSTROM_UNRELATED", "x", func(c *Config) string { return c.Database.DSN }, "file:file.db"},
	}
	path := writeConfig(t, file)
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s=%q", tt.env, tt.val), func(t *testing.T) {
			c, err := Load(path, env(map[string]string{tt.env: tt.val}))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := tt.want(c); got != tt.exp {
				t.Errorf("got %q, want %q", got, tt.exp)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		file string
		env  map[string]string
		want []string // substrings of the error
	}{
		{name: "empty file", file: "", want: []string{"server.listen: must not be empty"}},
		{name: "unknown top-level key", file: minimal + "metrics: {}\n", want: []string{"field metrics not found"}},
		{name: "unknown nested key", file: "server:\n  listen: \":8080\"\n  port: 8080\n", want: []string{"field port not found"}},
		{
			name: "password in the file",
			file: minimal + "registries:\n  - {name: local, url: \"http://zot:5000\", auth: {username: u, password: hunter2}}\n",
			want: []string{"field password not found"},
		},
		{name: "duration without unit", file: minimal[:len(minimal)-1] + "\n  shutdownTimeout: 30\n", want: []string{"line 3", "into time.Duration"}},
		{name: "negative claim TTL", file: minimal + "ingest: {claimTTL: -1m}\n", want: []string{"ingest.claimTTL: must not be negative"}},
		{name: "negative manifest size", file: minimal + "  maxManifestSize: -1\n", want: []string{"server.maxManifestSize: must not be negative"}},
		{name: "keys", file: minimal + "keys: {algorithm: dsa, retireGrace: -1h}\n",
			want: []string{`keys.algorithm: "dsa" is not ed25519 or rsa4096`, "keys.retireGrace: must not be negative"}},
		{name: "empty keys.algorithm", file: minimal + "keys: {algorithm: \"\"}\n", want: []string{`keys.algorithm: "" is not ed25519 or rsa4096`}},
		{name: "keys.tokenAlgorithm", file: minimal + "keys: {tokenAlgorithm: HS256}\n", want: []string{`keys.tokenAlgorithm: "HS256" is not ES256 or EdDSA`}},
		{name: "master key in the file", file: minimal + "keys: {masterKey: abc}\n", want: []string{"field masterKey not found"}},
		{name: "env override is validated", file: minimal, env: map[string]string{"LAJSTROM_DATABASE_DRIVER": "mysql"}, want: []string{`database.driver: "mysql" is not sqlite or postgres`}},
		{
			name: "registry limits",
			file: minimal + "registries:\n  - {name: local, url: \"http://zot:5000\", timeout: -1s, capabilities: {maxBlobSize: -1}}\n",
			want: []string{"registries[0].timeout: must not be negative", "registries[0].capabilities.maxBlobSize: must not be negative"},
		},
		{
			name: "unknown capability",
			file: minimal + "registries:\n  - {name: local, url: \"http://zot:5000\", capabilities: {deletes: false}}\n",
			want: []string{"field deletes not found"},
		},
		{
			// catalog is set under capabilities only; there is no short form.
			name: "catalog outside capabilities",
			file: minimal + "registries:\n  - {name: local, url: \"http://zot:5000\", catalog: false}\n",
			want: []string{"field catalog not found"},
		},
		{
			name: "every problem is reported",
			file: `
server: {listen: "", externalURL: "file:///x", shutdownTimeout: 0s}
log: {level: verbose, format: xml}
database: {driver: mysql, dsn: ""}
registries:
  - {name: local, url: "http://zot:5000", prefix: "/art"}
  - {name: local, type: harbor, url: "zot:5000"}
  - {name: Bad Name, url: "https://u:secret@example.com"}
repos:
  - {name: scratch, format: raw, registry: x}
  - {name: scratch, format: raw, registry: local}
  - {name: "", format: "", registry: ""}
`,
			want: []string{
				"server.listen: must not be empty",
				"server.externalURL: must be an absolute http(s) URL",
				"server.shutdownTimeout: must be positive",
				`log.level: "verbose" is not debug, info, warn or error`,
				`log.format: "xml" is not text or json`,
				`database.driver: "mysql" is not sqlite or postgres`,
				"database.dsn: must not be empty",
				`registries[0].prefix: must not start with "/" and must end with "/" when set`,
				`registries[1].name: "local" is already used by registries[0]`,
				`registries[1].type: "harbor" is not generic or ecr`,
				"registries[1].url: must be an absolute http(s) URL",
				`registries[2].name: "Bad Name" must be lowercase`,
				"registries[2].url: must not contain credentials",
				`repos[0].registry: "x" is not a configured registry`,
				`repos[1]: "raw/scratch" is already declared by repos[0]`,
				"repos[2].name: must not be empty",
				"repos[2].format: must not be empty",
				"repos[2].registry: must not be empty",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, tt.file)
			_, err := Load(path, env(tt.env))
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			msg := err.Error()
			if !strings.Contains(msg, path) {
				t.Errorf("error does not name the file %s:\n%s", path, msg)
			}
			for _, w := range tt.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error does not contain %q:\n%s", w, msg)
				}
			}
			if strings.Contains(msg, "secret") || strings.Contains(msg, "hunter2") {
				t.Errorf("error contains a password:\n%s", msg)
			}
		})
	}
}

func TestLoadRegistryKeys(t *testing.T) {
	file := minimal + `
registries:
  - {name: defaults, url: "http://zot:5000"}
  - name: tuned
    url: https://registry.example
    timeout: 5s
    retries: -1
    capabilities: {referrers: false, catalog: false, deleteManifest: true, maxBlobSize: 1073741824}
`
	c, err := Load(writeConfig(t, file), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	f, tr := false, true
	want := []Registry{
		{Name: "defaults", Type: "generic", URL: "http://zot:5000", Timeout: 30 * time.Second, Retries: 3},
		{
			Name: "tuned", Type: "generic", URL: "https://registry.example", Timeout: 5 * time.Second, Retries: -1,
			Capabilities: CapabilityOverrides{Referrers: &f, Catalog: &f, DeleteManifest: &tr, MaxBlobSize: 1 << 30},
		},
	}
	if !reflect.DeepEqual(c.Registries, want) {
		t.Errorf("registries =\n%+v\nwant\n%+v", c.Registries, want)
	}
}

func TestLoadPasswordEnv(t *testing.T) {
	file := minimal + `
registries:
  - name: local
    url: http://zot:5000
    auth: {username: lajstrom, passwordEnv: ZOT_PASSWORD}
`
	path := writeConfig(t, file)

	c, err := Load(path, env(map[string]string{"ZOT_PASSWORD": "s3cr3t"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := c.Registries[0]
	if r.Auth.Password != "s3cr3t" {
		t.Errorf("password = %q, want the value of ZOT_PASSWORD", r.Auth.Password)
	}
	if r.Type != "generic" {
		t.Errorf("type = %q, want the default generic", r.Type)
	}
	js, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), fmt.Sprintf("%v", *c), string(js)} {
		if strings.Contains(s, "s3cr3t") {
			t.Errorf("formatted config contains the password: %s", s)
		}
	}

	for name, e := range map[string]map[string]string{"unset": nil, "empty": {"ZOT_PASSWORD": ""}} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(path, env(e))
			want := `registries[0].auth.passwordEnv: environment variable "ZOT_PASSWORD" is not set or empty`
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("Load error = %v, want it to contain %q", err, want)
			}
		})
	}
}

// TestLoadKeys reads the keys section. Load does not read the variable masterKeyEnv names:
// serve and admin key do (keys.LoadMaster), so a configuration that names an unset variable
// loads, as the example does for make run.
func TestLoadKeys(t *testing.T) {
	c, err := Load(writeConfig(t, minimal+"keys: {masterKeyEnv: LAJSTROM_MASTER_KEY, algorithm: rsa4096, retireGrace: 48h, tokenAlgorithm: EdDSA}\n"), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Keys{MasterKeyEnv: "LAJSTROM_MASTER_KEY", Algorithm: "rsa4096", RetireGrace: 48 * time.Hour, TokenAlgorithm: "EdDSA"}
	if c.Keys != want {
		t.Errorf("keys = %+v, want %+v", c.Keys, want)
	}
}

// TestLoadTokenSigningKeyFile checks that a file that still sets auth.tokenSigningKeyFile, to
// any value, is refused with a message that says where the key went, so that nobody keeps a
// key file believing it is read; other unknown keys of auth are refused as before.
func TestLoadTokenSigningKeyFile(t *testing.T) {
	const want = "auth.tokenSigningKeyFile is gone: the key lives in the database, ADR 0010"
	for _, file := range []string{
		minimal + "auth:\n  tokenSigningKeyFile: /etc/lajstrom/jwt.key\n",
		minimal + "auth: {anonymousRead: true, tokenSigningKeyFile: \"\"}\n",
	} {
		if _, err := Load(writeConfig(t, file), nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load of %q: %v, want %q", file, err, want)
		}
	}
	_, err := Load(writeConfig(t, minimal+"auth: {tokenKey: x}\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "field tokenKey not found") {
		t.Errorf("Load with another unknown auth key: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	_, err := Load(path, nil)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load error = %v, want fs.ErrNotExist", err)
	}
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("Load error = %v, want it to name %s", err, path)
	}
}

// The example configuration must stay loadable as it is: make run and the quickstarts use it.
func TestExampleFile(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "deploy", "compose", "lajstrom.example.yaml"), nil)
	if err != nil {
		t.Fatalf("Load example: %v", err)
	}
	if len(c.Registries) != 2 || len(c.Repos) != 1 {
		t.Errorf("example has %d registries and %d repos, want 2 and 1", len(c.Registries), len(c.Repos))
	}
}

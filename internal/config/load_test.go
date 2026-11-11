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

// listenOnly is a configuration with server.listen alone, which Load refuses: the externalURL
// is required (#248). minimal adds it.
const (
	listenOnly = "server:\n  listen: \":8080\"\n"
	minimal    = listenOnly + "  externalURL: https://art.example.com\n"
)

func TestLoadDefaults(t *testing.T) {
	tests := []struct {
		name string
		file string
	}{
		{name: "server.listen and server.externalURL", file: minimal},
		{name: "explicit null sections", file: minimal + "log:\ndatabase:\nregistries:\ningest:\nreconcile:\nkeys:\nrepos:\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load(writeConfig(t, tt.file), nil)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			want := &Config{
				Server: Server{Listen: ":8080", ExternalURL: "https://art.example.com", ShutdownTimeout: 30 * time.Second, MaxManifestSize: 4 << 20, MetricsListen: "127.0.0.1:9464",
					StallTimeout: time.Minute},
				Log:       Log{Level: "info", Format: "text"},
				Database:  Database{Driver: "sqlite", DSN: "file:lajstrom.db"},
				Ingest:    Ingest{ClaimTTL: 10 * time.Minute},
				Reconcile: Reconcile{Interval: time.Hour},
				Keys:      Keys{Algorithm: "ed25519", RetireGrace: 720 * time.Hour, TokenAlgorithm: "ES256"},
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
  externalURL: https://file.example
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
		{"LAJSTROM_MASTER_KEY_FILE", "/run/secrets/master-key", func(c *Config) string { return c.Keys.MasterKeyFile }, "/run/secrets/master-key"},
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
		name    string
		file    string
		env     map[string]string
		want    []string // substrings of the error
		wantNot []string // substrings the error must not have
	}{
		{name: "empty file", file: "", want: []string{"server.listen: must not be empty"}},
		{name: "unknown top-level key", file: minimal + "metrics: {}\n", want: []string{"field metrics not found"}},
		{name: "unknown nested key", file: "server:\n  listen: \":8080\"\n  port: 8080\n", want: []string{"field port not found"}},
		{
			name: "password in the file",
			file: minimal + "registries:\n  - {name: local, url: \"http://zot:5000\", auth: {username: u, password: hunter2}}\n",
			want: []string{"field password not found"},
		},
		{name: "duration without unit", file: minimal[:len(minimal)-1] + "\n  shutdownTimeout: 30\n", want: []string{"line 4", "into time.Duration", "with its unit: 0s"}},
		{name: "a zero without unit", file: minimal + "reconcile: {interval: 0}\n", want: []string{"into time.Duration", "with its unit: 0s"}},
		{name: "negative claim TTL", file: minimal + "ingest: {claimTTL: -1m}\n", want: []string{"ingest.claimTTL: must not be negative"}},
		{name: "negative reconcile interval", file: minimal + "reconcile: {interval: -1h}\n", want: []string{"reconcile.interval: must not be negative"}},
		{name: "metrics listener", file: minimal[:len(minimal)-1] + "\n  metricsListen: \"9464\"\n", want: []string{`server.metricsListen: "9464" is not host:port`}},
		{name: "metrics on the public listener", file: minimal[:len(minimal)-1] + "\n  metricsListen: \":8080\"\n",
			want: []string{"server.metricsListen: must not be server.listen"}},
		{name: "negative manifest size", file: minimal + "  maxManifestSize: -1\n", want: []string{"server.maxManifestSize: must not be negative"}},
		{name: "half a TLS pair", file: minimal + "  tls: {certFile: /etc/lajstrom/tls.crt}\n", want: []string{"server.tls: certFile and keyFile must be set together"}},
		{name: "negative limits", file: minimal + "  stallTimeout: -1s\n  maxConcurrentUploads: -1\n",
			want: []string{"server.stallTimeout: must not be negative", "server.maxConcurrentUploads: must not be negative"}},
		{name: "trusted proxies", file: minimal + "  trustedProxies: [\"10.0.0.0/8\", \"proxy.local\", \"10.0.0.1/33\", \"::ffff:0:0/80\"]\n",
			want: []string{`server.trustedProxies[1]: "proxy.local" is not an address`, `server.trustedProxies[2]: "10.0.0.1/33"`, `server.trustedProxies[3]`}},
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
			name: "registry consistency",
			file: minimal + "registries:\n  - {name: a, url: \"http://zot:5000\", capabilities: {consistency: maybe, consistencyBound: -1s}}\n" +
				"  - {name: b, url: \"http://zot:5001\", capabilities: {consistency: strong, consistencyBound: 2s}}\n",
			want: []string{`registries[0].capabilities.consistency: "maybe" is not strong or eventual`,
				"registries[0].capabilities.consistencyBound: must not be negative",
				"registries[1].capabilities.consistencyBound: a strongly consistent registry has no lag"},
		},
		{
			name: "token realm hosts",
			file: minimal + `registries:
  - name: local
    url: "https://registry.example"
    tokenRealmHosts: [gitlab.com, "*.docker.io", "https://auth.docker.io", "auth.docker.io:443", "", "auth.example.", "a..b", "-a.example", "fe80::1%eth0", "auth.docker.io/token", "::1", 192.0.2.1, Auth-1.Example]
`,
			want: []string{
				`registries[0].tokenRealmHosts[1]: "*.docker.io" is not a host name or an IP address`,
				`registries[0].tokenRealmHosts[2]: "https://auth.docker.io"`,
				`registries[0].tokenRealmHosts[3]: "auth.docker.io:443"`,
				`registries[0].tokenRealmHosts[4]: ""`,
				`registries[0].tokenRealmHosts[5]: "auth.example."`,
				`registries[0].tokenRealmHosts[6]: "a..b"`,
				`registries[0].tokenRealmHosts[7]: "-a.example"`,
				`registries[0].tokenRealmHosts[8]: "fe80::1%eth0"`,
				`registries[0].tokenRealmHosts[9]: "auth.docker.io/token"`,
			},
			wantNot: []string{"tokenRealmHosts[0]", "tokenRealmHosts[10]", "tokenRealmHosts[11]", "tokenRealmHosts[12]"},
		},
		{
			name: "spoolUnknownLength not a bool",
			file: minimal + "registries:\n  - name: local\n    url: \"http://zot:5000\"\n    uploads: {spoolUnknownLength: sometimes}\n",
			want: []string{"line 7", "into bool"},
		},
		{
			name: "unknown uploads key",
			file: minimal + "registries:\n  - {name: local, url: \"http://zot:5000\", uploads: {spool: true}}\n",
			want: []string{"field spool not found"},
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
				"server.externalURL: must be an absolute URL, https://<host>[:port]",
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
			for _, w := range tt.wantNot {
				if strings.Contains(msg, w) {
					t.Errorf("error contains %q:\n%s", w, msg)
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
    capabilities: {referrers: false, catalog: false, deleteManifest: true, maxBlobSize: 1073741824, consistency: eventual, consistencyBound: 4s}
    tokenRealmHosts: [auth.registry.example, 10.0.0.5]
    uploads: {spoolUnknownLength: true}
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
			Capabilities: CapabilityOverrides{Referrers: &f, Catalog: &f, DeleteManifest: &tr, MaxBlobSize: 1 << 30,
				Consistency: ConsistencyEventual, ConsistencyBound: 4 * time.Second},
			TokenRealmHosts: []string{"auth.registry.example", "10.0.0.5"},
			Uploads:         RegistryUploads{SpoolUnknownLength: true},
		},
	}
	if !reflect.DeepEqual(c.Registries, want) {
		t.Errorf("registries =\n%+v\nwant\n%+v", c.Registries, want)
	}
}

func TestLoadRegistryECR(t *testing.T) {
	file := minimal + `
registries:
  - name: ecr
    type: ecr
    url: https://123456789012.dkr.ecr.eu-west-1.amazonaws.com
    ecr:
      endpoint: http://localhost:5010
      imageTagMutability: IMMUTABLE_WITH_EXCLUSION
      imageTagMutabilityExclusions: ["sha256-*"]
      encryption: KMS
      kmsKey: alias/lajstrom
      forceDelete: true
`
	c, err := Load(writeConfig(t, file), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := ECR{Endpoint: "http://localhost:5010", ImageTagMutability: "IMMUTABLE_WITH_EXCLUSION",
		ImageTagMutabilityExclusions: []string{"sha256-*"}, Encryption: "KMS", KMSKey: "alias/lajstrom", ForceDelete: true}
	if got := c.Registries[0].ECR; !reflect.DeepEqual(got, want) {
		t.Errorf("ecr =\n%+v\nwant\n%+v", got, want)
	}

	bad := []struct{ name, entry, want string }{
		{"ecr on generic", `{name: r, url: "http://zot:5000", ecr: {region: us-east-1}}`, "registries[0].ecr: is only for type ecr"},
		{"auth on ecr", `{name: r, type: ecr, url: "https://r.example", auth: {username: AWS, passwordEnv: X}}`, "registries[0].auth: must be empty for type ecr"},
		{"endpoint", `{name: r, type: ecr, url: "https://r.example", ecr: {endpoint: "localhost:5010"}}`, "registries[0].ecr.endpoint: must be an absolute http(s) URL"},
		{"mutability", `{name: r, type: ecr, url: "https://r.example", ecr: {imageTagMutability: immutable}}`, `registries[0].ecr.imageTagMutability: "immutable" is not one of IMMUTABLE`},
		{"exclusion without filter", `{name: r, type: ecr, url: "https://r.example", ecr: {imageTagMutability: MUTABLE_WITH_EXCLUSION}}`, "registries[0].ecr.imageTagMutabilityExclusions: must name a filter"},
		{"filter without exclusion", `{name: r, type: ecr, url: "https://r.example", ecr: {imageTagMutabilityExclusions: ["v*"]}}`, "registries[0].ecr.imageTagMutabilityExclusions: needs an imageTagMutability"},
		{"empty filter", `{name: r, type: ecr, url: "https://r.example", ecr: {imageTagMutability: IMMUTABLE_WITH_EXCLUSION, imageTagMutabilityExclusions: [""]}}`, "must not contain an empty filter"},
		{"encryption", `{name: r, type: ecr, url: "https://r.example", ecr: {encryption: SSE}}`, `registries[0].ecr.encryption: "SSE" is not one of AES256, KMS, KMS_DSSE`},
		{"key without KMS", `{name: r, type: ecr, url: "https://r.example", ecr: {kmsKey: alias/x}}`, "registries[0].ecr.kmsKey: needs encryption KMS or KMS_DSSE"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, minimal+"registries:\n  - "+tt.entry+"\n"), nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load: %v; want an error containing %q", err, tt.want)
			}
		})
	}
}

// TestLoadNestedPrefixes: two registries on one URL, the same scheme, host and port, must not
// have prefixes that nest, or a repository of one is a repository of the other (threat model
// P-1, issue #250).
func TestLoadNestedPrefixes(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		want    string // the error, "" when the configuration loads
	}{
		{
			name:    "the issue's case: an empty prefix and raw/x/",
			entries: []string{`{name: a, url: "https://r"}`, `{name: b, url: "https://r", prefix: "raw/x/"}`},
			want:    `registries[1].prefix: "raw/x/" of registry "b" nests with "" of registry "a" (registries[0]) on the same URL`,
		},
		{
			name:    "the longer prefix first",
			entries: []string{`{name: a, url: "https://r", prefix: "art/raw/"}`, `{name: b, url: "https://r", prefix: "art/"}`},
			want:    `registries[1].prefix: "art/" of registry "b" nests with "art/raw/" of registry "a" (registries[0])`,
		},
		{
			name:    "the same prefix",
			entries: []string{`{name: a, url: "http://zot:5000", prefix: "art/"}`, `{name: b, url: "http://zot:5000", prefix: "art/"}`},
			want:    `registries[1].prefix: "art/" of registry "b" nests with "art/" of registry "a"`,
		},
		{
			name:    "two empty prefixes",
			entries: []string{`{name: a, url: "http://zot:5000"}`, `{name: b, url: "http://zot:5000/"}`},
			want:    `registries[1].prefix: "" of registry "b" nests with "" of registry "a"`,
		},
		{
			name:    "the default port written out, the host in capitals",
			entries: []string{`{name: a, url: "https://R.example"}`, `{name: b, url: "https://r.example.:443", prefix: "x/"}`},
			want:    `registries[1].prefix: "x/" of registry "b" nests with "" of registry "a"`,
		},
		{
			name:    "a third registry nests with the first",
			entries: []string{`{name: a, url: "https://r", prefix: "a/"}`, `{name: b, url: "https://r", prefix: "b/"}`, `{name: c, url: "https://r", prefix: "a/c/"}`},
			want:    `registries[2].prefix: "a/c/" of registry "c" nests with "a/" of registry "a" (registries[0])`,
		},
		{
			name:    "prefixes that share characters, not a component",
			entries: []string{`{name: a, url: "https://r", prefix: "art/"}`, `{name: b, url: "https://r", prefix: "art2/"}`, `{name: c, url: "https://r", prefix: "ar/"}`},
		},
		{
			name:    "another port",
			entries: []string{`{name: a, url: "http://localhost:5000"}`, `{name: b, url: "http://localhost:5001", prefix: "raw/x/"}`},
		},
		{
			name:    "another scheme",
			entries: []string{`{name: a, url: "http://r"}`, `{name: b, url: "https://r", prefix: "raw/x/"}`},
		},
		{
			name:    "another host",
			entries: []string{`{name: a, url: "https://r1"}`, `{name: b, url: "https://r2"}`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, minimal+"registries:\n  - "+strings.Join(tt.entries, "\n  - ")+"\n"), nil)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("Load: %v, want success", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("Load: %v; want an error containing %q", err, tt.want)
			case tt.want != "" && strings.Count(err.Error(), "nests with") != 1:
				t.Errorf("Load: %v; want the pair reported once", err)
			}
		})
	}
}

// TestLoadRepoPolicy: repos[].policy is read as JSON-shaped values, which the bootstrap passes to
// the API's checks as they are.
func TestLoadRepoPolicy(t *testing.T) {
	file := minimal + `
registries:
  - {name: local, url: "http://zot:5000"}
repos:
  - {name: scratch, format: raw, registry: local}
  - name: releases
    format: maven
    registry: local
    policy: {maxFileSize: 104857600, blobRedirect: never, maven: {sessionTimeout: 45m}}
`
	c, err := Load(writeConfig(t, file), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Repos[0].Policy != nil {
		t.Errorf("repos[0].policy = %v, want none", c.Repos[0].Policy)
	}
	got, err := json.Marshal(c.Repos[1].Policy)
	if want := `{"blobRedirect":"never","maven":{"sessionTimeout":"45m"},"maxFileSize":104857600}`; err != nil || string(got) != want {
		t.Errorf("repos[1].policy = %s, %v; want %s", got, err, want)
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

// TestLoadMasterKeyFile: keys.masterKeyFile is a path that Load keeps and does not open; the file
// and the variable of keys.masterKeyEnv set together are an error that names both and shows
// neither value, and an empty variable counts as unset.
func TestLoadMasterKeyFile(t *testing.T) {
	const twice = "keys.masterKeyFile: set together with the environment variable LAJSTROM_MASTER_KEY of keys.masterKeyEnv"
	withEnv := minimal + "keys: {masterKeyEnv: LAJSTROM_MASTER_KEY}\n"
	withFile := minimal + "keys: {masterKeyEnv: LAJSTROM_MASTER_KEY, masterKeyFile: /run/secrets/master-key}\n"
	onlyFile := minimal + "keys: {masterKeyFile: /run/secrets/master-key}\n"
	value := strings.Repeat("ab", 32)
	tests := []struct {
		name    string
		file    string
		env     map[string]string
		wantErr bool
	}{
		{"file, variable unset", withFile, nil, false},
		{"file, variable empty", withFile, map[string]string{"LAJSTROM_MASTER_KEY": ""}, false},
		{"file and variable", withFile, map[string]string{"LAJSTROM_MASTER_KEY": value}, true},
		{"LAJSTROM_MASTER_KEY_FILE and variable", withEnv, map[string]string{"LAJSTROM_MASTER_KEY": value, "LAJSTROM_MASTER_KEY_FILE": "/k"}, true},
		{"file without masterKeyEnv, a variable of that name set", onlyFile, map[string]string{"LAJSTROM_MASTER_KEY": value}, false},
		{"variable only", withEnv, map[string]string{"LAJSTROM_MASTER_KEY": value}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(writeConfig(t, tt.file), env(tt.env))
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if strings.Contains(tt.file, "masterKeyFile") && c.Keys.MasterKeyFile != "/run/secrets/master-key" {
					t.Errorf("keys.masterKeyFile = %q", c.Keys.MasterKeyFile)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), twice) {
				t.Fatalf("Load: %v, want %q", err, twice)
			}
			if strings.Contains(err.Error(), value) {
				t.Errorf("the error shows the master key: %v", err)
			}
		})
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

// TestServerListen reads server.listen, LAJSTROM_SERVER_LISTEN and server.tls alone: a
// registry whose password variable is unset, which Load refuses, does not matter, and neither
// do certificate files that do not exist; an unknown key or a listen address that is not
// host:port does.
func TestServerListen(t *testing.T) {
	withSecret := minimal + "registries:\n  - {name: local, url: \"http://localhost:5000\", auth: {username: u, passwordEnv: UNSET_PASSWORD}}\n"
	withTLS := "server:\n  listen: \":8443\"\n  tls: {certFile: /missing/tls.crt, keyFile: /missing/tls.key}\n"
	tests := []struct {
		name, file string
		env        map[string]string
		want       string
		wantTLS    TLS
		wantErr    string
	}{
		{"the file", withSecret, nil, ":8080", TLS{}, ""},
		{"the variable", withSecret, map[string]string{"LAJSTROM_SERVER_LISTEN": "127.0.0.1:9090"}, "127.0.0.1:9090", TLS{}, ""},
		{"empty variable", withSecret, map[string]string{"LAJSTROM_SERVER_LISTEN": ""}, ":8080", TLS{}, ""},
		{"tls", withTLS, nil, ":8443", TLS{CertFile: "/missing/tls.crt", KeyFile: "/missing/tls.key"}, ""},
		{"unknown key", "server: {listen: \":8080\", lissen: x}\n", nil, "", TLS{}, "field lissen not found"},
		{"not host:port", "server:\n  listen: \"8080\"\n", nil, "", TLS{}, `server.listen: "8080" is not host:port`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotTLS, err := ServerListen(writeConfig(t, tt.file), env(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want || gotTLS != tt.wantTLS {
				t.Fatalf("ServerListen = %q, %+v, %v; want %q, %+v", got, gotTLS, err, tt.want, tt.wantTLS)
			}
		})
	}
	if _, err := Load(writeConfig(t, withSecret), nil); err == nil {
		t.Error("Load of the configuration with an unset password variable: no error, the test above proves nothing")
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

// TestExternalURL checks the rules of server.externalURL (docs/spec/http-routing.md §1,
// docs/acceptance.md C-02 and C-03): https://<host>[:port] without a path, query or fragment;
// http only for a loopback host with server.insecureHTTP; required but with server.insecureHTTP
// (#248).
func TestExternalURL(t *testing.T) {
	tests := []struct {
		url      string
		insecure bool
		want     string // a substring of the error; empty for none
	}{
		{url: "https://art.example.com"},
		{url: "", want: "must be set to the URL clients use"},
		{url: "", insecure: true},
		{url: "https://art.example.com:8443/"},
		{url: "https://[2001:db8::1]:443"},
		{url: "http://localhost:8080", insecure: true},
		{url: "http://127.0.0.1:8080", insecure: true},
		{url: "http://[::1]:8080", insecure: true},
		{url: "https://art.test/sub", want: "no path, query or fragment is allowed"},
		{url: "https://art.test/?x=1", want: "no path, query or fragment is allowed"},
		{url: "https://art.test#top", want: "no path, query or fragment is allowed"},
		{url: "http://art.test", want: "must be https: clients see Lajstrom over TLS only"},
		{url: "http://localhost:8080", want: "server.insecureHTTP: true"},
		{url: "http://art.test", insecure: true, want: "http is for a loopback host only"},
		{url: "http://10.0.0.1:8080", insecure: true, want: "http is for a loopback host only"},
		{url: "https://u:p@art.test", want: "must not contain credentials"}, //nolint:gosec // a fake password the error must not show
		{url: "ftp://art.test", want: "must be an absolute URL"},
		{url: "art.test", want: "must be an absolute URL"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s insecure=%v", tt.url, tt.insecure), func(t *testing.T) {
			file := fmt.Sprintf("%s  externalURL: %q\n  insecureHTTP: %v\n", listenOnly, tt.url, tt.insecure)
			_, err := Load(writeConfig(t, file), nil)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("Load: %v, want no error", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), "server.externalURL: ") || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("Load: %v, want server.externalURL: …%s…", err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), "u:p@") {
				t.Errorf("the error shows the credentials: %v", err)
			}
		})
	}
}

func TestTrustedPrefixes(t *testing.T) {
	tests := []struct {
		in   []string
		want string
	}{
		{nil, "[]"},
		{[]string{"10.0.0.0/8", "127.0.0.1", "::1", "2001:db8::/32"}, "[10.0.0.0/8 127.0.0.1/32 ::1/128 2001:db8::/32]"},
		{[]string{"10.1.2.3/8", "0.0.0.0/0", "::/0"}, "[10.0.0.0/8 0.0.0.0/0 ::/0]"},
		{[]string{"::ffff:10.0.0.0/104", "::ffff:192.0.2.7", "fe80::1%eth0"}, "[10.0.0.0/8 192.0.2.7/32 fe80::1/128]"},
	}
	for _, tt := range tests {
		got, err := Server{TrustedProxies: tt.in}.TrustedPrefixes()
		if err != nil {
			t.Fatalf("TrustedPrefixes(%q): %v", tt.in, err)
		}
		if s := fmt.Sprint(got); s != tt.want {
			t.Errorf("TrustedPrefixes(%q) = %s, want %s", tt.in, s, tt.want)
		}
	}
	if _, err := (Server{TrustedProxies: []string{"10.0.0.0/8", "nope"}}).TrustedPrefixes(); err == nil ||
		!strings.Contains(err.Error(), "server.trustedProxies[1]") {
		t.Errorf("TrustedPrefixes of a bad entry: %v", err)
	}
}

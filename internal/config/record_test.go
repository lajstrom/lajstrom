package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// recordedConfig is a configuration with a registry of a password, an upstream of a password
// file, a bundle of client certificates and the secrets of serve.
func recordedConfig() *Config {
	c := Default()
	c.Server.Listen, c.Server.MetricsListen = ":8080", "10.1.2.3:9464"
	c.Server.ExternalURL = "https://art.example.com"
	c.Server.TrustedProxies = []string{"10.0.0.0/8"}
	c.Database.Driver, c.Database.DSN = "postgres", "postgres://lajstrom:db-password@db/lajstrom"
	c.Registries = []Registry{{Name: "zot", URL: "https://zot.example.com", Auth: RegistryAuth{Username: "lajstrom",
		PasswordEnv: "ZOT_PASSWORD", Password: "registry-password"}}}
	c.Upstreams = []Upstream{{Name: "pypi", Format: "pypi", URL: "https://pypi.org/simple",
		Auth: UpstreamAuth{PasswordFile: "/run/secrets/pypi"}}} //nolint:gosec // the path of a secret, which the test checks is never read
	c.Auth.AnonymousRead = true
	c.Auth.OIDC.ClientSecretFile = "/run/secrets/oidc"
	c.Auth.Bootstrap.TokenFile = "/run/secrets/bootstrap"
	c.Auth.ClientCertificates.Bundles = []CertBundle{{Name: "corp", CAFile: "/etc/lajstrom/corp-ca.pem", Identity: "cn"}}
	c.Keys.MasterKeyFile = "/run/secrets/master"
	c.Repos = []Repo{{Name: "scratch", Format: "raw", Registry: "zot"}}
	return c
}

// publicOnly reads the files of public material of recordedConfig and fails the test on any other:
// a secret is never read.
func publicOnly(t *testing.T, files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if strings.HasPrefix(path, "/run/secrets/") {
			t.Errorf("the secret %s was read", path)
		}
		if b, ok := files[path]; ok {
			return []byte(b), nil
		}
		return nil, fs.ErrNotExist
	}
}

// TestRecorded is #1284's 3A (rbac.md §5): every key with its value after the defaults, but
// database, log, server.listen, server.metricsListen and repos; an item of a named list by its
// name; a secret by its path or variable, never read; a file of public material by its path and
// the sha256 of its content.
func TestRecorded(t *testing.T) {
	ca := "-----BEGIN CERTIFICATE-----\nMIIB…\n-----END CERTIFICATE-----\n"
	view, fp, err := recordedConfig().Recorded(publicOnly(t, map[string]string{"/etc/lajstrom/corp-ca.pem": ca}))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(ca))
	for k, want := range map[string]string{ //nolint:gosec // keys and paths of secrets, which the view records as they are
		"auth.anonymousRead":                             `true`,
		"server.externalURL":                             `"https://art.example.com"`,
		"server.trustedProxies":                          `["10.0.0.0/8"]`,
		"keys.retireGrace":                               `"720h0m0s"`,
		"keys.masterKeyFile":                             `"/run/secrets/master"`,
		"auth.oidc.clientSecretFile":                     `"/run/secrets/oidc"`,
		"registries[zot].url":                            `"https://zot.example.com"`,
		"registries[zot].auth.passwordEnv":               `"ZOT_PASSWORD"`,
		"upstreams[pypi].auth.passwordFile":              `"/run/secrets/pypi"`,
		"auth.clientCertificates.bundles[corp].caFile":   `{"path":"/etc/lajstrom/corp-ca.pem","sha256":"` + hex.EncodeToString(sum[:]) + `"}`,
		"auth.clientCertificates.bundles[corp].identity": `"cn"`,
		"server.tls.certFile":                            `""`,
	} {
		if got := string(view[k]); got != want {
			t.Errorf("%s = %s, want %s", k, got, want)
		}
	}
	all, _ := json.Marshal(view)
	for _, s := range []string{"database.", "log.", `"server.listen"`, "server.metricsListen", "repos", "db-password",
		"registry-password", "10.1.2.3"} {
		if strings.Contains(string(all), s) {
			t.Errorf("the view holds %s:\n%s", s, all)
		}
	}
	if !strings.HasPrefix(fp, "sha256:") || len(fp) != len("sha256:")+64 || fp != Fingerprint(view) {
		t.Errorf("fingerprint %s", fp)
	}

	// a CA added to the bundle's file is a change of that key alone
	view2, fp2, err := recordedConfig().Recorded(publicOnly(t, map[string]string{"/etc/lajstrom/corp-ca.pem": ca + ca}))
	if err != nil {
		t.Fatal(err)
	}
	changes := DiffRecorded(view, view2)
	if fp2 == fp || len(changes) != 1 || changes[0].Key != "auth.clientCertificates.bundles[corp].caFile" {
		t.Errorf("a new CA in the file: %s, %+v", fp2, changes)
	}

	// a file of public material that cannot be read is an error, which names its key
	if _, _, err := recordedConfig().Recorded(publicOnly(t, nil)); !errors.Is(err, fs.ErrNotExist) ||
		!strings.Contains(err.Error(), "auth.clientCertificates.bundles[corp].caFile") {
		t.Errorf("an unreadable CA file: %v", err)
	}
}

// TestRecordedClassified: every key that ends in File or Env names a secret (secretKeys) or a
// file of public material (publicFiles), so that a new one is classified before it is recorded;
// and every key the lists name exists.
func TestRecordedClassified(t *testing.T) {
	patterns := keyPatterns("", reflect.TypeFor[Config]())
	for _, p := range patterns {
		leaf := p[strings.LastIndexByte(p, '.')+1:]
		if (strings.HasSuffix(leaf, "File") || strings.HasSuffix(leaf, "Env") || leaf == "trustedRoot") &&
			!slices.Contains(secretKeys, p) && !slices.Contains(publicFiles, p) && !under(p, unrecorded) {
			t.Errorf("%s names a file or a variable: add it to secretKeys or to publicFiles (config/record.go)", p)
		}
	}
	for _, list := range [][]string{secretKeys, publicFiles, unrecorded} {
		for _, p := range list {
			if !slices.Contains(patterns, p) {
				t.Errorf("%s is no key of the configuration", p)
			}
		}
	}
}

// keyPatterns returns the keys of the struct type ty below p, and of its structs, an item of a
// named list as [].
func keyPatterns(p string, ty reflect.Type) []string {
	var out []string
	for i := range ty.NumField() {
		f := ty.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		k := join(p, name)
		out = append(out, k)
		switch ft := f.Type; {
		case ft.Kind() == reflect.Struct && ft.PkgPath() == ty.PkgPath():
			out = append(out, keyPatterns(k, ft)...)
		case ft.Kind() == reflect.Pointer && ft.Elem().Kind() == reflect.Struct:
			out = append(out, keyPatterns(k, ft.Elem())...)
		case ft.Kind() == reflect.Slice && named(ft.Elem()):
			out = append(out, keyPatterns(k+"[]", ft.Elem())...)
		}
	}
	return out
}

// under reports whether the pattern p is one of keys or below one.
func under(p string, keys []string) bool {
	return slices.ContainsFunc(keys, func(k string) bool { return p == k || strings.HasPrefix(p, k+".") || strings.HasPrefix(p, k+"[") })
}

// TestDiffRecorded: keys added, removed and changed, in key order; two spellings of one value
// are no change, and have one fingerprint.
func TestDiffRecorded(t *testing.T) {
	before := map[string]json.RawMessage{"a": json.RawMessage(`1`), "b": json.RawMessage(`{"x": 1, "y": [1, 2]}`),
		"c": json.RawMessage(`"gone"`), "e": json.RawMessage(`false`)}
	after := map[string]json.RawMessage{"a": json.RawMessage(`2`), "b": json.RawMessage(`{"y":[1,2],"x":1}`),
		"d": json.RawMessage(`"new"`), "e": json.RawMessage(`false`)}
	got, _ := json.Marshal(DiffRecorded(before, after))
	want := `[{"key":"a","from":1,"to":2},{"key":"c","from":"gone"},{"key":"d","to":"new"}]`
	if string(got) != want {
		t.Errorf("DiffRecorded = %s, want %s", got, want)
	}
	if DiffRecorded(nil, nil) != nil {
		t.Error("DiffRecorded of nothing is not empty")
	}
	b2 := map[string]json.RawMessage{"b": json.RawMessage(`{"y":[1,2],"x":1}`)}
	b1 := map[string]json.RawMessage{"b": json.RawMessage(`{"x": 1, "y": [1, 2]}`)}
	if Fingerprint(b1) != Fingerprint(b2) {
		t.Error("two spellings of one value have two fingerprints")
	}
	if Fingerprint(b1) == Fingerprint(map[string]json.RawMessage{"b": json.RawMessage(`{"x":2,"y":[1,2]}`)}) {
		t.Error("two values have one fingerprint")
	}
}

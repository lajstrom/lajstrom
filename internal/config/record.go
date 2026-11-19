package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"
)

// What a config.change audit row records of the configuration (docs/spec/rbac.md §5, #1284):
// every key, with its value after the defaults, the file and the environment, but those of
// unrecorded, which are a run's own or a replica's own, or recorded otherwise. A field tagged
// yaml:"-" (a secret the configuration resolved, such as a password read from a variable) is
// never recorded. A key that names where a secret is (secretKeys) counts by that name, the path
// or the variable, and its secret is never read; a key that names a file of public material
// (publicFiles) counts by its path and the sha256 of the file's content.
var (
	// unrecorded are the keys left out, each with what is below it: database (its DSN may hold
	// a password, and the record is in that database), log (a knob of one run, which flags
	// override), server.listen and server.metricsListen (an address of each replica, such as a
	// pod's IP) and repos (their effect has its own rows, repo.create with source config).
	unrecorded = []string{"database", "log", "server.listen", "server.metricsListen", "repos"}
	// secretKeys name where a secret is: a file, a directory of them or a variable, recorded as
	// the path or the name, never what the file or the directory holds. A test fails on a key
	// ending in File or Env that is in neither list.
	secretKeys = []string{"server.tls.keyFile", "auth.bootstrap.tokenFile", "auth.oidc.clientSecretFile",
		"keys.masterKeyEnv", "keys.masterKeyFile", "keys.masterKeyDir", "registries[].auth.passwordEnv", "upstreams[].auth.passwordEnv",
		"upstreams[].auth.passwordFile", "upstreams[].auth.tokenFile"}
	// publicFiles name a file of certificates or public keys, recorded as {"path", "sha256"}, so
	// that a CA added to a bundle's file is a change.
	publicFiles = []string{"server.tls.certFile", "auth.clientCertificates.bundles[].caFile",
		"auth.kubernetes.clusters[].caFile", "auth.ci.issuers[].caFile", "attachments.trust[].publicKeyFile",
		"attachments.keyless.trustedRoot"}
)

// RecordedFile is the value of a key of publicFiles: the path, and the sha256 of the content the
// start read, lowercase hex.
type RecordedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Recorded returns what a config.change row records of c (docs/spec/rbac.md §5): each key that
// counts, as configuration.md names it with [<name>] for an item of a list of named items, to its
// value as JSON, and the fingerprint, sha256:<hex> of the canonical JSON of them all. A list of
// values, a list of items without a name and a map are one value; a duration is its text (720h0m0s).
// readFile reads a file of publicFiles; nil means os.ReadFile. A file it cannot read is an error.
func (c *Config) Recorded(readFile func(string) ([]byte, error)) (map[string]json.RawMessage, string, error) {
	if readFile == nil {
		readFile = os.ReadFile
	}
	r := recorder{view: map[string]json.RawMessage{}, readFile: readFile}
	r.walk("", "", reflect.ValueOf(c).Elem())
	if r.err != nil {
		return nil, "", r.err
	}
	return r.view, Fingerprint(r.view), nil
}

// Fingerprint is sha256:<hex> of the canonical JSON of view: its keys in order, each value as
// encoding/json writes it again, so that two spellings of one value have one fingerprint.
func Fingerprint(view map[string]json.RawMessage) string {
	h := sha256.New()
	_, _ = h.Write([]byte{'{'}) // a hash.Hash never fails to write
	for i, k := range slices.Sorted(maps.Keys(view)) {
		if i > 0 {
			_, _ = h.Write([]byte{','})
		}
		key, _ := json.Marshal(k) // a string always marshals
		_, _ = h.Write(append(append(key, ':'), canonical(view[k])...))
	}
	_, _ = h.Write([]byte{'}'})
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Change is a key whose value differs between two views of Recorded: From is absent for a key
// added, To for one removed.
type Change struct {
	Key  string          `json:"key"`
	From json.RawMessage `json:"from,omitempty"`
	To   json.RawMessage `json:"to,omitempty"`
}

// DiffRecorded returns the keys whose values differ between before and after, in key order;
// values are compared as JSON, not as bytes: a database may keep another spelling of one value.
func DiffRecorded(before, after map[string]json.RawMessage) []Change {
	all := maps.Clone(before)
	if all == nil {
		all = map[string]json.RawMessage{}
	}
	maps.Copy(all, after)
	keys := slices.Sorted(maps.Keys(all))
	var out []Change
	for _, k := range keys {
		from, had := before[k]
		to, has := after[k]
		switch {
		case had && has && string(canonical(from)) == string(canonical(to)):
			continue
		case !had:
			out = append(out, Change{Key: k, To: canonical(to)})
		case !has:
			out = append(out, Change{Key: k, From: canonical(from)})
		default:
			out = append(out, Change{Key: k, From: canonical(from), To: canonical(to)})
		}
	}
	return out
}

// canonical is raw as encoding/json writes it again: objects with their keys in order, no
// spaces; raw itself when it is not JSON.
func canonical(raw json.RawMessage) json.RawMessage {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}

// recorder walks a Config into its view.
type recorder struct {
	view     map[string]json.RawMessage
	readFile func(string) ([]byte, error)
	err      error
}

// walk records the fields of the struct v below key, whose pattern ([] for an item of a named
// list) is pattern.
func (r *recorder) walk(key, pattern string, v reflect.Value) {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" || !f.IsExported() {
			continue
		}
		k, p := join(key, name), join(pattern, name)
		if slices.Contains(unrecorded, p) {
			continue
		}
		r.field(k, p, v.Field(i))
	}
}

// field records the value fv of the key k, of pattern p.
func (r *recorder) field(k, p string, fv reflect.Value) {
	switch {
	case slices.Contains(publicFiles, p):
		r.publicFile(k, fv.String())
	case fv.Kind() == reflect.Struct && fv.NumField() > 0:
		r.walk(k, p, fv)
	case fv.Kind() == reflect.Pointer && fv.Type().Elem().Kind() == reflect.Struct && !fv.IsNil() && fv.Elem().NumField() > 0:
		r.walk(k, p, fv.Elem())
	case fv.Kind() == reflect.Slice && named(fv.Type().Elem()):
		for j := range fv.Len() {
			item := fv.Index(j)
			r.walk(k+"["+item.FieldByName("Name").String()+"]", p+"[]", item)
		}
	default:
		r.put(k, plain(fv))
	}
}

// publicFile records the file of public material at path, by its path and the sha256 of its
// content; "" for no file.
func (r *recorder) publicFile(k, path string) {
	if path == "" {
		r.put(k, "")
		return
	}
	b, err := r.readFile(path)
	if err != nil {
		if r.err == nil {
			r.err = fmt.Errorf("config: %s: %w", k, err)
		}
		return
	}
	sum := sha256.Sum256(b)
	r.put(k, RecordedFile{Path: path, SHA256: hex.EncodeToString(sum[:])})
}

func (r *recorder) put(k string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		if r.err == nil {
			r.err = fmt.Errorf("config: %s: %w", k, err)
		}
		return
	}
	r.view[k] = b
}

// named reports whether t is a struct with a field tagged yaml:"name", whose items a list writes
// by their names.
func named(t reflect.Type) bool {
	if t.Kind() != reflect.Struct {
		return false
	}
	f, ok := t.FieldByName("Name")
	name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	return ok && name == "name"
}

// plain is v as a value of JSON: a duration as its text, a struct by its yaml keys (the fields
// tagged yaml:"-" left out), a nil pointer null, lists and maps of those.
func plain(v reflect.Value) any {
	switch {
	case v.Type() == reflect.TypeFor[time.Duration]():
		return time.Duration(v.Int()).String()
	case v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return plain(v.Elem())
	case v.Kind() == reflect.Struct:
		return plainStruct(v)
	case v.Kind() == reflect.Slice && !v.IsNil():
		out := make([]any, v.Len())
		for i := range v.Len() {
			out[i] = plain(v.Index(i))
		}
		return out
	case v.Kind() == reflect.Map && !v.IsNil():
		m := map[string]any{}
		for _, k := range v.MapKeys() {
			m[fmt.Sprint(k.Interface())] = plain(v.MapIndex(k))
		}
		return m
	case v.Kind() == reflect.Slice || v.Kind() == reflect.Map:
		return nil
	}
	return v.Interface()
}

// plainStruct is the struct v by its yaml keys, the fields tagged yaml:"-" left out.
func plainStruct(v reflect.Value) map[string]any {
	m := map[string]any{}
	for i := range v.NumField() {
		f := v.Type().Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name != "" && name != "-" && f.IsExported() {
			m[name] = plain(v.Field(i))
		}
	}
	return m
}

// join is key.name, or name at the top.
func join(key, name string) string {
	if key == "" {
		return name
	}
	return key + "." + name
}

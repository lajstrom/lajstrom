package npm

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/lajstrom/lajstrom/internal/format"
)

// installKeys are the keys of a version document, besides the install scripts, that decide
// what an install of the package runs (ADR 0012).
var installKeys = []string{"dependencies", "optionalDependencies", "peerDependencies", "bundleDependencies", "bin"}

// checkInstall compares what an install runs in the version document and in the tarball's
// package.json: dependencies of every kind, bundleDependencies (or bundledDependencies), bin
// and the scripts preinstall, install and postinstall, each as a JSON value, a missing key equal
// to an empty one. A difference wraps format.ErrInvalid and names the key, so that a packument
// never says other than what an install of the tarball runs ("manifest confusion").
func checkInstall(doc, pkg *PackageJSON) error {
	differ := func(key string) error {
		return fmt.Errorf("%w: the version document and the tarball's package.json differ in %s", format.ErrInvalid, key)
	}
	for _, k := range installKeys {
		if !sameValue(doc.value(k), pkg.value(k)) {
			return differ(k)
		}
	}
	for _, s := range installScripts {
		if !sameValue(doc.scriptValue(s), pkg.scriptValue(s)) {
			return differ("scripts." + s)
		}
	}
	return nil
}

// value returns the member key, bundledDependencies for a bundleDependencies that is empty.
func (p *PackageJSON) value(key string) json.RawMessage {
	v := p.Fields[key]
	if key == "bundleDependencies" && sameValue(v, nil) {
		v = p.Fields["bundledDependencies"]
	}
	return v
}

// sameValue reports whether two JSON values are equal, any empty value (missing, null, "",
// false, {} or []) equal to any other.
func sameValue(a, b json.RawMessage) bool {
	var x, y any
	_ = json.Unmarshal(a, &x) // missing: nil
	_ = json.Unmarshal(b, &y)
	if empty(x) && empty(y) {
		return true
	}
	return reflect.DeepEqual(x, y)
}

// empty reports whether a decoded JSON value is null, "", false, {} or [].
func empty(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case bool:
		return !v
	case map[string]any:
		return len(v) == 0
	case []any:
		return len(v) == 0
	}
	return false
}

package npm

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
)

// gypInstall is the install script npm gives a package with binding.gyp and no install script.
const gypInstall = "node-gyp rebuild"

// normalise returns p, the version document or the tarball's package.json, with the
// normalisations npm's CLI applies to the document it sends and not to the package.json it packs
// (ADR 0012), for checkInstall to compare the two; they are applied to both, since yarn 4 sends
// its package.json as it is, and npm's document is the same again:
//
//   - bin as a string is {<the name without its scope>: bin};
//   - directories.bin without bin is a bin of every entry below that directory in the tarball,
//     files and the directories above them, by base name, paths with a part that begins with .
//     left out, as npm 10 and 11 list them;
//   - binding.gyp at the top, with neither an install nor a preinstall script, adds the script
//     install "node-gyp rebuild";
//   - the paths of bin are without a leading ./.
//
// A directories.bin to expand in a tarball whose paths were too many to keep wraps
// format.ErrInvalid.
func (t *Tarball) normalise(p *PackageJSON) (*PackageJSON, error) {
	fields := maps.Clone(p.Fields)
	var bin any
	_ = json.Unmarshal(fields["bin"], &bin)
	if s, ok := bin.(string); ok && s != "" {
		_, base, _ := splitName(p.Name)
		bin = map[string]any{base: s}
	}
	if dir := directoriesBin(fields); falsy(bin) && dir != "" {
		var err error
		if bin, err = t.binDir(dir); err != nil {
			return nil, err
		}
	}
	if m, ok := bin.(map[string]any); ok {
		for k, v := range m {
			if s, ok := v.(string); ok {
				m[k] = withoutDot(s)
			}
		}
		fields["bin"] = mustJSON(m)
	}
	if t.HasBindingGyp && p.script("install") == "" && p.script("preinstall") == "" {
		var scripts map[string]json.RawMessage
		_ = json.Unmarshal(fields["scripts"], &scripts)
		scripts = maps.Clone(scripts)
		if scripts == nil {
			scripts = map[string]json.RawMessage{}
		}
		scripts["install"] = mustJSON(gypInstall)
		fields["scripts"] = mustJSON(scripts)
	}
	return &PackageJSON{Name: p.Name, Version: p.Version, Fields: fields}, nil
}

// directoriesBin returns the string directories.bin of fields, "" when there is none.
func directoriesBin(fields map[string]json.RawMessage) string {
	var directories map[string]json.RawMessage
	var dir string
	if json.Unmarshal(fields["directories"], &directories) == nil {
		_ = json.Unmarshal(directories["bin"], &dir)
	}
	return dir
}

// binDir returns the bin npm makes of directories.bin dir: each entry below dir in the tarball,
// and each directory between it and dir, by its base name, its path dir joined with it; none
// whose path below dir has a part that begins with ".", none for a dir out of the package.
func (t *Tarball) binDir(dir string) (map[string]any, error) {
	if t.pathsDropped {
		return nil, fmt.Errorf("%w: directories.bin %q cannot be expanded: the paths of the tarball are more than %d bytes",
			format.ErrInvalid, dir, maxPaths)
	}
	bin := map[string]any{}
	clean := path.Clean(withoutDot(dir))
	if clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return bin, nil
	}
	prefix := clean + "/"
	if clean == "." {
		prefix = ""
	}
	for p := range strings.SplitSeq(strings.TrimSuffix(string(t.paths), "\x00"), "\x00") {
		rel, ok := strings.CutPrefix(p, prefix)
		if !ok || rel == "" || hidden(rel) {
			continue
		}
		for ; rel != "."; rel = path.Dir(rel) {
			bin[path.Base(rel)] = withoutDot(path.Join(dir, rel))
		}
	}
	return bin, nil
}

// hidden reports whether a part of the slash-separated path p begins with ".", which npm's
// glob of directories.bin leaves out.
func hidden(p string) bool {
	return strings.HasPrefix(p, ".") || strings.Contains(p, "/.")
}

// withoutDot returns p without its leading "./", as npm writes the paths of bin.
func withoutDot(p string) string {
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	return p
}

// falsy reports whether a decoded JSON value is false in JavaScript, as npm tests bin: missing,
// null, false, "" or 0.
func falsy(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case bool:
		return !v
	case string:
		return v == ""
	case float64:
		return v == 0
	}
	return false
}

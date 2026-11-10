package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
)

// The bounds of a tarball read, as an sdist's (#243): at most maxEntries tar entries and
// maxUnpacked bytes unpacked, the whole archive being read, and a package.json of at most
// maxPackageJSON bytes, the bound of the metadata layer it may become (an oras push).
const (
	maxEntries     = 1 << 18
	maxUnpacked    = 1 << 30
	maxPackageJSON = 2 << 20
)

// The files of a tarball's top directory that the profile looks at
// (docs/spec/manifest-profile.md §2, npm).
const (
	filePackageJSON = "package.json"
	fileShrinkwrap  = "npm-shrinkwrap.json"
	fileBindingGyp  = "binding.gyp"
)

// installScripts are the scripts an install of a package runs.
var installScripts = []string{"preinstall", "install", "postinstall"}

// Tarball is what ReadTarball finds in a package tarball.
type Tarball struct {
	// Root is the top directory, package for what npm pack writes.
	Root string
	// PackageJSON is <Root>/package.json, read.
	PackageJSON *PackageJSON
	// HasShrinkwrap and HasBindingGyp say whether the top directory has npm-shrinkwrap.json and
	// binding.gyp, regular files.
	HasShrinkwrap, HasBindingGyp bool
}

// HasInstallScript reports whether an install of the package runs a script: package.json has a
// preinstall, install or postinstall script, or the top directory has binding.gyp, for which npm
// runs node-gyp rebuild.
func (t *Tarball) HasInstallScript() bool {
	if t.HasBindingGyp {
		return true
	}
	for _, s := range installScripts {
		if t.PackageJSON.script(s) != "" {
			return true
		}
	}
	return false
}

// ReadTarball reads a package tarball whole: a gzipped tar archive whose entries are all below
// one top directory, which holds package.json, a regular file, once. An entry that a
// case-insensitive file system would take for package.json is refused too, as is a second one,
// so that an install never extracts another package.json than the one read here. Pax global
// headers and the entry of "." are skipped. At most maxEntries entries and maxUnpacked bytes are
// read. Errors wrap format.ErrInvalid, except a failure to read r.
func ReadTarball(r io.Reader) (*Tarball, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("%w: the tarball is not gzip: %w", format.ErrInvalid, err)
	}
	tr := tar.NewReader(&bounded{r: gz, left: maxUnpacked})
	var t Tarball
	var pkg []byte
	for n := 0; ; n++ {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return t.finish(pkg)
		case errors.Is(err, errUnpacked):
			return nil, fmt.Errorf("%w: the tarball unpacks to more than %d bytes", format.ErrInvalid, maxUnpacked)
		case err != nil:
			return nil, fmt.Errorf("%w: the tarball is not a tar archive: %w", format.ErrInvalid, err)
		case n == maxEntries:
			return nil, fmt.Errorf("%w: the tarball has more than %d entries", format.ErrInvalid, maxEntries)
		case h.Typeflag == tar.TypeXGlobalHeader:
			continue
		}
		if err := t.take(tr, h, &pkg); err != nil {
			return nil, err
		}
	}
}

// take looks at the entry h, whose bytes r reads: package.json is read into pkg, which must be
// nil, npm-shrinkwrap.json and binding.gyp are noted.
func (t *Tarball) take(r io.Reader, h *tar.Header, pkg *[]byte) error {
	rel, err := t.entry(h)
	if err != nil {
		return err
	}
	regular := h.Typeflag == tar.TypeReg
	switch {
	case strings.EqualFold(rel, filePackageJSON):
		if *pkg != nil || rel != filePackageJSON || !regular {
			return fmt.Errorf("%w: the tarball has %s/%s besides or in place of %s/%s", format.ErrInvalid, t.Root, rel, t.Root, filePackageJSON)
		}
		*pkg, err = readPackageJSON(r, h.Size)
		return err
	case rel == fileShrinkwrap && regular:
		t.HasShrinkwrap = true
	case rel == fileBindingGyp && regular:
		t.HasBindingGyp = true
	}
	return nil
}

// entry returns the path of h below the top directory, "" for the top directory itself, and
// takes the first entry's top directory for the tarball's.
func (t *Tarball) entry(h *tar.Header) (string, error) {
	p := path.Clean(h.Name)
	if p == "." {
		return "", nil
	}
	top, rel, _ := strings.Cut(p, "/")
	switch {
	case top == "" || top == ".." || p[0] == '/':
		return "", fmt.Errorf("%w: the tarball has %q, outside a top directory", format.ErrInvalid, h.Name)
	case t.Root == "":
		t.Root = top
	case top != t.Root:
		return "", fmt.Errorf("%w: the tarball has two top directories, %q and %q", format.ErrInvalid, t.Root, top)
	}
	if rel == "" && h.Typeflag != tar.TypeDir {
		return "", fmt.Errorf("%w: the tarball has %q at the top, not in a directory", format.ErrInvalid, h.Name)
	}
	return rel, nil
}

// finish parses the package.json the read found.
func (t *Tarball) finish(pkg []byte) (*Tarball, error) {
	if pkg == nil {
		root := t.Root
		if root == "" {
			root = "package"
		}
		return nil, fmt.Errorf("%w: the tarball has no %s/%s", format.ErrInvalid, root, filePackageJSON)
	}
	p, err := ParsePackageJSON(pkg)
	if err != nil {
		return nil, err
	}
	t.PackageJSON = p
	return t, nil
}

// readPackageJSON reads the package.json entry of size bytes, at most maxPackageJSON.
func readPackageJSON(r io.Reader, size int64) ([]byte, error) {
	if size > maxPackageJSON {
		return nil, fmt.Errorf("%w: package.json is %d bytes, at most %d", format.ErrInvalid, size, maxPackageJSON)
	}
	b, err := io.ReadAll(io.LimitReader(r, maxPackageJSON))
	switch {
	case errors.Is(err, errUnpacked):
		return nil, fmt.Errorf("%w: the tarball unpacks to more than %d bytes", format.ErrInvalid, maxUnpacked)
	case err != nil:
		return nil, fmt.Errorf("%w: package.json: %w", format.ErrInvalid, err)
	}
	return b, nil
}

// PackageJSON is a package.json, or the version document of a publish, read: its name and
// version, and every top-level key as it is written.
type PackageJSON struct {
	// Name and Version are the name and version fields.
	Name, Version string
	// Fields are the top-level members, each value as JSON.
	Fields map[string]json.RawMessage
}

// ParsePackageJSON reads a package.json: one JSON object, nothing after it, whose name and
// version are strings and not empty. Errors wrap format.ErrInvalid.
func ParsePackageJSON(b []byte) (*PackageJSON, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil {
		return nil, fmt.Errorf("%w: package.json is not a JSON object: %w", format.ErrInvalid, err)
	}
	if fields == nil {
		return nil, fmt.Errorf("%w: package.json is null, not an object", format.ErrInvalid)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: package.json has more after its object", format.ErrInvalid)
	}
	p := &PackageJSON{Fields: fields}
	for _, f := range []struct {
		key string
		v   *string
	}{{"name", &p.Name}, {"version", &p.Version}} {
		if err := json.Unmarshal(fields[f.key], f.v); err != nil || *f.v == "" {
			return nil, fmt.Errorf("%w: package.json has no %s string", format.ErrInvalid, f.key)
		}
	}
	return p, nil
}

// script returns the script name of the scripts object, "" when there is none or it is not a
// string.
func (p *PackageJSON) script(name string) string {
	var s string
	_ = json.Unmarshal(p.scriptValue(name), &s)
	return s
}

// scriptValue returns the script name of the scripts object as JSON; nil when there is none or
// scripts is not an object.
func (p *PackageJSON) scriptValue(name string) json.RawMessage {
	var scripts map[string]json.RawMessage
	if json.Unmarshal(p.Fields["scripts"], &scripts) != nil {
		return nil
	}
	return scripts[name]
}

// errUnpacked is the read error of a stream past its bound of unpacked bytes.
var errUnpacked = errors.New("unpacked past the bound")

// bounded passes at most left bytes of r through, and fails the read after them with
// errUnpacked.
type bounded struct {
	r    io.Reader
	left int64
}

func (b *bounded) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errUnpacked
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

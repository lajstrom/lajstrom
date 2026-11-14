package deb

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The Sources index of a component (docs/spec/http-routing.md §3, apt; #79): a paragraph per
// source version, the fields of its .dsc with Source as Package, the lists of files with the .dsc
// itself first, and Directory, the pool directory of the source.

// sourcesFile is the name of the index of the source packages of a component, below
// <component>/source/.
const sourcesFile = "Sources"

// sourceFieldOrder is the order of the fields of a Sources paragraph, as the Debian archive
// writes them: the fields of the .dsc named here first, then the others sorted by name, then
// Directory. Files and the Checksums lists are written from the .dsc's with the .dsc's own line
// first; Package is the .dsc's Source.
var sourceFieldOrder = []string{
	"package", "binary", "version", "maintainer", "uploaders",
	"build-depends", "build-depends-indep", "build-depends-arch",
	"build-conflicts", "build-conflicts-indep", "build-conflicts-arch",
	"architecture", "standards-version", "format", "files", "checksums-sha1", "checksums-sha256",
	"homepage", "package-list",
}

// sourceRank maps a lowercase field name to its place in sourceFieldOrder.
var sourceRank = func() map[string]int {
	m := make(map[string]int, len(sourceFieldOrder))
	for i, f := range sourceFieldOrder {
		m[f] = i
	}
	return m
}()

// sourceParagraphOf returns the Sources paragraph of the source version v of pkg in component,
// from its metadata (sourceMetadata).
func sourceParagraphOf(pkg index.Package, v index.Version, component string, raw []byte) (paragraph, error) {
	what := pkg.Name + " " + v.Version + " (" + v.Variant + ")"
	var md sourceMetadata
	if err := json.Unmarshal(raw, &md); err != nil {
		return paragraph{}, fmt.Errorf("deb: the metadata of %s: %w", what, err)
	}
	if md.Control == nil || md.Filename == "" || md.SHA256 == "" || md.MD5 == "" {
		return paragraph{}, fmt.Errorf("deb: the metadata of %s lacks control, filename, md5 or sha256", what)
	}
	own := map[string]string{"files": md.MD5, "checksums-sha1": md.SHA1, "checksums-sha256": md.SHA256}
	fields := map[string]string{}
	names := map[string]string{} // lowercase to the name as written
	for name, value := range md.Control {
		key := strings.ToLower(name)
		switch key {
		case "package", "directory":
			continue // Package is the .dsc's Source, Directory the pool's
		case "source":
			name, key = "Package", "package"
		}
		if sum, ok := own[key]; ok {
			if sum == "" {
				continue // a .dsc without Checksums-Sha1 gets none; the metadata of a push may lack the sha1
			}
			value = "\n " + sum + " " + strconv.FormatInt(md.Size, 10) + " " + md.Filename + value
		}
		if !safeField(name, value) {
			return paragraph{}, fmt.Errorf("deb: the metadata of %s has the field %q, which cannot go into Sources as it is", what, name)
		}
		fields[key], names[key] = value, name
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	rank := func(k string) int {
		if r, ok := sourceRank[k]; ok {
			return r
		}
		return len(sourceFieldOrder)
	}
	slices.SortFunc(keys, func(a, b string) int { return cmp.Or(cmp.Compare(rank(a), rank(b)), strings.Compare(a, b)) })
	var b bytes.Buffer
	for _, k := range keys {
		writeField(&b, names[k], fields[k])
	}
	writeField(&b, "Directory", "pool/"+component+"/"+poolDir(pkg.Name))
	return paragraph{name: pkg.Name, version: v.Version, arch: ArchSource, component: component, text: b.Bytes()}, nil
}

// poolDir returns the directory of a source in a pool, <p>/<source> (PoolPath).
func poolDir(source string) string {
	p := PoolPath(source, "x")
	return p[:len(p)-2]
}

// sourcesOf returns the Sources index of a component: its source paragraphs, each followed by a
// blank line.
func sourcesOf(srcs []paragraph, component string) []byte {
	var b bytes.Buffer
	for _, p := range srcs {
		if p.component == component {
			b.Write(p.text)
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

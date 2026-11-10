package testutil

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // npm's dist.shasum is a sha1; not a security use
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// NpmOptions are the options of NpmTarball. Every empty field takes its default, so
// NpmOptions{} is a valid package.
type NpmOptions struct {
	// Name is the package name, unscoped or @scope/name; default "widgets".
	Name string
	// Version is the version, written as it is; default "1.0.0".
	Version string
	// Description is the description; default a sentence about the fixture.
	Description string
	// Dependencies, OptionalDependencies and PeerDependencies are those maps of package.json;
	// none by default.
	Dependencies, OptionalDependencies, PeerDependencies map[string]string
	// Bin is the bin map of package.json, a command to a file of the package; none by default.
	Bin map[string]string
	// Scripts are the scripts of package.json; none by default.
	Scripts map[string]string
	// Shrinkwrap and BindingGyp put npm-shrinkwrap.json and binding.gyp in the top directory.
	Shrinkwrap, BindingGyp bool
	// Root is the top directory of the archive; default "package", the one npm pack writes.
	Root string
	// PackageJSON, when set, is the package.json as it is, and the fields above but Root are
	// not written into it.
	PackageJSON []byte
}

// NpmInfo is what a reader finds in the tarball NpmTarball builds.
type NpmInfo struct {
	// Name and Version are those of package.json.
	Name, Version string
	// Filename is the file name npm serves the tarball under: the name without its scope, the
	// version, .tgz (widgets-1.0.0.tgz for @acme/widgets).
	Filename string
	// Root is the top directory: package.
	Root string
	// PackageJSON is the package.json file.
	PackageJSON []byte
	// Integrity is npm's dist.integrity, sha512-<base64>, and Shasum its dist.shasum, the sha1
	// in lowercase hex.
	Integrity, Shasum string
	// Size is the size of the whole file in bytes.
	Size int64
	// Checksums are the digests of the whole file.
	Checksums
}

// NpmTarball returns an npm package tarball, a tar.gz as npm pack writes it: the top directory
// package/ holding package.json, index.js and the files the options ask for, and the facts a
// reader finds in it. The same options give the same bytes: every member is dated 2026-01-01
// 00:00 UTC and owned by root.
func NpmTarball(opts NpmOptions) ([]byte, NpmInfo) {
	members, info := npmMembers(opts)
	b := tarGz(members)
	return b, npmFacts(b, info)
}

// NpmBomb returns the tarball of NpmTarball(opts) with, between its top directory and the rest,
// entries empty files and then a file of zeros zero bytes, rounded up to 512 (none when 0): small
// to send, and costly to read to its end, as a reader of the whole tarball does.
func NpmBomb(opts NpmOptions, entries int, zeros int64) ([]byte, NpmInfo) {
	members, info := npmMembers(opts)
	parts := []repeated{{tarOf(members[:1])[:512], 1}, {tarHeader(info.Root+"/empty", 0), entries}}
	if blocks := int((zeros + 511) / 512); blocks > 0 {
		parts = append(parts, repeated{tarHeader(info.Root+"/zeros", int64(blocks)*512), 1}, repeated{make([]byte, 512), blocks})
	}
	b := gzipOfRepeated(append(parts, repeated{tarOf(members[1:]), 1})...)
	return b, npmFacts(b, info)
}

// npmFacts completes info with the size and the checksums of the tarball b.
func npmFacts(b []byte, info NpmInfo) NpmInfo {
	s512 := sha512.Sum512(b)
	s1 := sha1.Sum(b) //nolint:gosec // see the import
	info.Integrity = "sha512-" + base64.StdEncoding.EncodeToString(s512[:])
	info.Shasum = hex.EncodeToString(s1[:])
	info.Size = int64(len(b))
	info.Checksums = checksumsOf(b)
	return info
}

// npmMembers returns the members of NpmTarball(opts), its top directory first, and its facts but
// the size and the checksums.
func npmMembers(opts NpmOptions) ([]member, NpmInfo) {
	info := NpmInfo{Name: opts.Name, Version: opts.Version, Root: opts.Root}
	if info.Name == "" {
		info.Name = "widgets"
	}
	if info.Version == "" {
		info.Version = "1.0.0"
	}
	if info.Root == "" {
		info.Root = "package"
	}
	base := info.Name
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	info.Filename = base + "-" + info.Version + ".tgz"
	info.PackageJSON = opts.PackageJSON
	if info.PackageJSON == nil {
		info.PackageJSON = npmPackageJSON(opts, info)
	}
	members := []member{
		{name: info.Root + "/"},
		{name: info.Root + "/package.json", body: info.PackageJSON},
		{name: info.Root + "/index.js", body: []byte("module.exports = " + jsString(info.Name+"@"+info.Version) + ";\n")},
	}
	if opts.Shrinkwrap {
		lock := `{"name": ` + jsString(info.Name) + `, "version": ` + jsString(info.Version) + `, "lockfileVersion": 3, "packages": {}}` + "\n"
		members = append(members, member{name: info.Root + "/npm-shrinkwrap.json", body: []byte(lock)})
	}
	if opts.BindingGyp {
		members = append(members, member{name: info.Root + "/binding.gyp", body: []byte(`{"targets": []}` + "\n")})
	}
	for _, file := range opts.Bin {
		members = append(members, member{name: info.Root + "/" + strings.TrimPrefix(file, "./"), body: []byte("#!/usr/bin/env node\n")})
	}
	return members, info
}

// npmPackageJSON returns the package.json of the options, with two-space indents as npm writes it.
func npmPackageJSON(opts NpmOptions, info NpmInfo) []byte {
	description := opts.Description
	if description == "" {
		description = "A package built in process by internal/testutil."
	}
	pkg := struct {
		Name                 string            `json:"name"`
		Version              string            `json:"version"`
		Description          string            `json:"description"`
		Main                 string            `json:"main"`
		License              string            `json:"license"`
		Bin                  map[string]string `json:"bin,omitempty"`
		Scripts              map[string]string `json:"scripts,omitempty"`
		Dependencies         map[string]string `json:"dependencies,omitempty"`
		OptionalDependencies map[string]string `json:"optionalDependencies,omitempty"`
		PeerDependencies     map[string]string `json:"peerDependencies,omitempty"`
	}{info.Name, info.Version, description, "index.js", "MIT", opts.Bin, opts.Scripts,
		opts.Dependencies, opts.OptionalDependencies, opts.PeerDependencies}
	b, err := json.MarshalIndent(pkg, "", "  ")
	must(err)
	return append(b, '\n')
}

// jsString returns s as a JavaScript (and JSON) string literal.
func jsString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	must(enc.Encode(s))
	return strings.TrimSuffix(buf.String(), "\n")
}

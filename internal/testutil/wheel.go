package testutil

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// WheelOptions are the options of Wheel. Every empty field takes its default, so
// WheelOptions{} is a valid wheel.
type WheelOptions struct {
	// Name is the project name as METADATA spells it, in any case and with any separators;
	// default "My-Pkg".
	Name string
	// Version is the version, written as it is; default "1.0.0".
	Version string
	// Summary is the Summary field; default a sentence about the fixture.
	Summary string
	// PythonTag, AbiTag and PlatformTag are the tags of the file name; defaults "py3", "none",
	// "any". A compressed tag set (cp311.cp312, manylinux_2_17_x86_64.manylinux2014_x86_64) is
	// kept in the file name and expanded into the Tag lines of WHEEL.
	PythonTag   string
	AbiTag      string
	PlatformTag string
	// RequiresPython is the Requires-Python field; default ">=3.9".
	RequiresPython string
	// RequiresDist are the Requires-Dist fields, one each; none by default.
	RequiresDist []string
}

// WheelInfo is what a parser finds in the wheel Wheel builds.
type WheelInfo struct {
	// Name is the project name as METADATA spells it: My-Pkg.
	Name string
	// Normalized is the PEP 503 name: my-pkg.
	Normalized string
	// Version is the version, as METADATA and the file name spell it.
	Version string
	// Filename is the PEP 427 file name: the normalised name with '-' escaped to '_', the
	// version with '-' escaped to '_', and the tags: my_pkg-1.0.0-py3-none-any.whl.
	Filename string
	// DistInfo is the .dist-info directory, named with the escaped name and version, as the file
	// name: my_pkg-1.0.0.dist-info.
	DistInfo string
	// Module is the package of the wheel, my_pkg, with its __init__.py.
	Module string
	// PythonTag, AbiTag and PlatformTag are the tags as the file name has them.
	PythonTag   string
	AbiTag      string
	PlatformTag string
	// Variant is the compressed tag set py3-none-any, the lajstrom.variant of the wheel
	// (docs/spec/manifest-profile.md §3).
	Variant string
	// Tags are the expanded tags, one per Tag line of WHEEL, in that order.
	Tags []string
	// Summary, RequiresPython and RequiresDist are the METADATA fields.
	Summary        string
	RequiresPython string
	RequiresDist   []string
	// Metadata is the METADATA file.
	Metadata []byte
	// Size is the size of the whole file in bytes.
	Size int64
	// Checksums are the digests of the whole file.
	Checksums
}

// Wheel returns a wheel, a zip archive with <module>/__init__.py and then the .dist-info
// directory with METADATA (Metadata-Version 2.1), WHEEL (Wheel-Version 1.0, a Tag line per
// expanded tag) and RECORD last (every other member with sha256=<urlsafe base64 without
// padding> and its size, and the RECORD line itself with neither, as PEP 376 has it), and the
// facts a parser finds in it. The same options give the same bytes: every member is dated
// 2026-01-01 00:00 UTC.
func Wheel(opts WheelOptions) ([]byte, WheelInfo) {
	o := opts.withDefaults()
	info := WheelInfo{
		Name:           o.Name,
		Normalized:     normalizePyPI(o.Name),
		Version:        o.Version,
		PythonTag:      o.PythonTag,
		AbiTag:         o.AbiTag,
		PlatformTag:    o.PlatformTag,
		Variant:        o.PythonTag + "-" + o.AbiTag + "-" + o.PlatformTag,
		Summary:        o.Summary,
		RequiresPython: o.RequiresPython,
		RequiresDist:   slices.Clone(o.RequiresDist),
	}
	escaped, version := strings.ReplaceAll(info.Normalized, "-", "_"), strings.ReplaceAll(o.Version, "-", "_")
	info.Module = escaped
	info.DistInfo = escaped + "-" + version + ".dist-info"
	info.Filename = escaped + "-" + version + "-" + info.Variant + ".whl"

	var md strings.Builder
	fmt.Fprintf(&md, "Metadata-Version: 2.1\nName: %s\nVersion: %s\nSummary: %s\nRequires-Python: %s\n",
		o.Name, o.Version, o.Summary, o.RequiresPython)
	for _, r := range o.RequiresDist {
		fmt.Fprintf(&md, "Requires-Dist: %s\n", r)
	}
	info.Metadata = []byte(md.String())

	var wheel strings.Builder
	fmt.Fprintf(&wheel, "Wheel-Version: 1.0\nGenerator: lajstrom-testutil\nRoot-Is-Purelib: %t\n", o.PlatformTag == "any")
	for _, py := range strings.Split(o.PythonTag, ".") {
		for _, abi := range strings.Split(o.AbiTag, ".") {
			for _, plat := range strings.Split(o.PlatformTag, ".") {
				info.Tags = append(info.Tags, py+"-"+abi+"-"+plat)
				fmt.Fprintf(&wheel, "Tag: %s-%s-%s\n", py, abi, plat)
			}
		}
	}

	members := []member{
		{name: escaped + "/__init__.py", body: pyInit(o.Version)},
		{name: info.DistInfo + "/METADATA", body: info.Metadata},
		{name: info.DistInfo + "/WHEEL", body: []byte(wheel.String())},
	}
	members = append(members, member{name: info.DistInfo + "/RECORD", body: record(members, info.DistInfo+"/RECORD")})
	b := zipOf(members)
	info.Size = int64(len(b))
	info.Checksums = checksumsOf(b)
	return b, info
}

// withDefaults returns the options with every empty field set to its default.
func (o WheelOptions) withDefaults() WheelOptions {
	o.Name, o.Version, o.Summary = pyDefaults(o.Name, o.Version, o.Summary)
	if o.PythonTag == "" {
		o.PythonTag = "py3"
	}
	if o.AbiTag == "" {
		o.AbiTag = "none"
	}
	if o.PlatformTag == "" {
		o.PlatformTag = "any"
	}
	if o.RequiresPython == "" {
		o.RequiresPython = ">=3.9"
	}
	return o
}

// record returns the RECORD of a wheel with the members and RECORD itself, named self: one CSV
// line per member, path, sha256=<urlsafe base64 of the digest without padding> and size, and
// self with an empty hash and size.
func record(members []member, self string) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	for _, m := range members {
		sum := sha256.Sum256(m.body)
		must(w.Write([]string{m.name, "sha256=" + base64.RawURLEncoding.EncodeToString(sum[:]), strconv.Itoa(len(m.body))}))
	}
	must(w.Write([]string{self, "", ""}))
	w.Flush()
	must(w.Error())
	return buf.Bytes()
}

// SdistOptions are the options of Sdist. Every empty field takes its default, so
// SdistOptions{} is a valid sdist.
type SdistOptions struct {
	// Name is the project name as PKG-INFO spells it; default "My-Pkg".
	Name string
	// Version is the version, written as it is; default "1.0.0".
	Version string
	// Summary is the Summary field; default a sentence about the fixture.
	Summary string
}

// SdistInfo is what a parser finds in the sdist Sdist builds.
type SdistInfo struct {
	// Name is the project name as PKG-INFO spells it: My-Pkg.
	Name string
	// Normalized is the PEP 503 name: my-pkg.
	Normalized string
	// Version and Summary are the PKG-INFO fields.
	Version string
	Summary string
	// Filename is the PEP 625 file name, the normalised name with '-' escaped to '_':
	// my_pkg-1.0.0.tar.gz.
	Filename string
	// Root is the top directory of the archive: my_pkg-1.0.0.
	Root string
	// Module is the package of the sdist, my_pkg, with its __init__.py.
	Module string
	// PKGInfo is the PKG-INFO file.
	PKGInfo []byte
	// Size is the size of the whole file in bytes.
	Size int64
	// Checksums are the digests of the whole file.
	Checksums
}

// Sdist returns a source distribution, a tar.gz with the top directory <name>-<version>/
// holding PKG-INFO (Metadata-Version 2.1, Name, Version, Summary), a pyproject.toml with a
// [build-system] on setuptools and a minimal [project] table, so that pip installs it, and
// <module>/__init__.py, and the facts a parser finds in it. The same options give the same
// bytes: every member is dated 2026-01-01 00:00 UTC and owned by root.
func Sdist(opts SdistOptions) ([]byte, SdistInfo) {
	var o SdistOptions
	o.Name, o.Version, o.Summary = pyDefaults(opts.Name, opts.Version, opts.Summary)
	info := SdistInfo{Name: o.Name, Normalized: normalizePyPI(o.Name), Version: o.Version, Summary: o.Summary}
	escaped := strings.ReplaceAll(info.Normalized, "-", "_")
	info.Module = escaped
	info.Root = escaped + "-" + o.Version
	info.Filename = info.Root + ".tar.gz"
	info.PKGInfo = fmt.Appendf(nil, "Metadata-Version: 2.1\nName: %s\nVersion: %s\nSummary: %s\n", o.Name, o.Version, o.Summary)
	pyproject := fmt.Appendf(nil, "[build-system]\nrequires = [\"setuptools>=61\"]\nbuild-backend = \"setuptools.build_meta\"\n\n"+
		"[project]\nname = %s\nversion = %s\ndescription = %s\n", tomlString(o.Name), tomlString(o.Version), tomlString(o.Summary))
	b := tarGz([]member{
		{name: info.Root + "/"},
		{name: info.Root + "/PKG-INFO", body: info.PKGInfo},
		{name: info.Root + "/pyproject.toml", body: pyproject},
		{name: info.Root + "/" + escaped + "/"},
		{name: info.Root + "/" + escaped + "/__init__.py", body: pyInit(o.Version)},
	})
	info.Size = int64(len(b))
	info.Checksums = checksumsOf(b)
	return b, info
}

// pyDefaults returns name, version and summary, each set to its default when empty.
func pyDefaults(name, version, summary string) (string, string, string) {
	if name == "" {
		name = "My-Pkg"
	}
	if version == "" {
		version = "1.0.0"
	}
	if summary == "" {
		summary = "A package built in process by internal/testutil."
	}
	return name, version, summary
}

// separators is a run of the characters PEP 503 folds into one '-'.
var separators = regexp.MustCompile(`[-_.]+`)

// normalizePyPI returns the PEP 503 normalised project name: runs of '-', '_' and '.' become
// one '-', and the name is lower-cased.
func normalizePyPI(name string) string {
	return strings.ToLower(separators.ReplaceAllString(name, "-"))
}

// pyInit returns the __init__.py of the module.
func pyInit(version string) []byte {
	return fmt.Appendf(nil, "__version__ = %s\n", strconv.Quote(version))
}

// tomlString returns s as a TOML basic string: a JSON string is one.
func tomlString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	must(enc.Encode(s))
	return strings.TrimSuffix(buf.String(), "\n")
}

package pypi

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
)

// maxCoreMetadata is the largest METADATA or PKG-INFO file the plugin reads: a long README in
// the description is a few hundred KiB.
const maxCoreMetadata = 4 << 20

// Normalize returns the PEP 503 normalized form of a project name: lowercase, every run of
// '-', '_' and '.' one '-'. Requests, requests and REQUESTS are one project; so are
// zope.interface and zope-interface.
func Normalize(name string) string {
	return strings.ToLower(separators.ReplaceAllString(name, "-"))
}

var separators = regexp.MustCompile(`[-_.]+`)

// validName is the project name of the core metadata specification: ASCII letters and digits,
// '.', '_' and '-' inside, a letter or digit at both ends.
var validName = regexp.MustCompile(`^(?i:[a-z0-9]|[a-z0-9][a-z0-9._-]*[a-z0-9])$`)

// ValidName reports whether name is a valid project name.
func ValidName(name string) bool { return validName.MatchString(name) }

// pep440 is the version scheme of PEP 440, the canonical pattern of its appendix B, anchored and
// without regard to case: epoch, release, pre-release, post-release, development release and
// local version, each with the separators PEP 440 allows.
var pep440 = regexp.MustCompile(`^(?i:\s*v?` +
	`(?:[0-9]+!)?` +
	`[0-9]+(?:\.[0-9]+)*` +
	`(?:[-_.]?(?:a|b|c|rc|alpha|beta|pre|preview)[-_.]?[0-9]*)?` +
	`(?:-[0-9]+|[-_.]?(?:post|rev|r)[-_.]?[0-9]*)?` +
	`(?:[-_.]?dev[-_.]?[0-9]*)?` +
	`(?:\+[a-z0-9]+(?:[-_.][a-z0-9]+)*)?` +
	`\s*)$`)

// IsPEP440 reports whether version is a PEP 440 version.
func IsPEP440(version string) bool { return pep440.MatchString(version) }

// Wheel is what the PEP 427 file name of a wheel says:
// {distribution}-{version}(-{build})?-{python}-{abi}-{platform}.whl.
type Wheel struct {
	// Name is the distribution as the file name spells it, with '_' for the separators.
	Name string
	// Version is the version, '_' in the file name read back as '-'.
	Version string
	// Build is the optional build tag; it starts with a digit.
	Build string
	// Python, ABI and Platform are the tags, compressed tag sets kept as they are
	// (py2.py3, manylinux_2_17_x86_64.manylinux2014_x86_64).
	Python, ABI, Platform string
}

// Variant returns the lajstrom.variant of the wheel, its compressed tag set:
// py3-none-any, cp311-cp311-manylinux_2_17_x86_64.
func (w Wheel) Variant() string { return w.Python + "-" + w.ABI + "-" + w.Platform }

// wheelPart is a component of a wheel file name: letters, digits, '_', '.' and '+' (a local
// version), as PEP 427 escapes them.
var wheelPart = regexp.MustCompile(`^[A-Za-z0-9_.+!]+$`)

// ParseWheelFilename reads a wheel file name. Errors wrap format.ErrInvalid.
func ParseWheelFilename(filename string) (Wheel, error) {
	stem, ok := strings.CutSuffix(filename, ".whl")
	if !ok || strings.ContainsAny(stem, "/\\") {
		return Wheel{}, fmt.Errorf("%w: %q is not a wheel file name", format.ErrInvalid, filename)
	}
	parts := strings.Split(stem, "-")
	if len(parts) != 5 && len(parts) != 6 {
		return Wheel{}, fmt.Errorf("%w: %q: a wheel file name has 5 or 6 parts separated by '-', not %d",
			format.ErrInvalid, filename, len(parts))
	}
	for _, p := range parts {
		if !wheelPart.MatchString(p) {
			return Wheel{}, fmt.Errorf("%w: %q: the part %q of the wheel file name", format.ErrInvalid, filename, p)
		}
	}
	w := Wheel{Name: parts[0], Version: strings.ReplaceAll(parts[1], "_", "-")}
	tags := parts[2:]
	if len(parts) == 6 {
		w.Build, tags = parts[2], parts[3:]
		if w.Build[0] < '0' || w.Build[0] > '9' {
			return Wheel{}, fmt.Errorf("%w: %q: the build tag %q does not start with a digit", format.ErrInvalid, filename, w.Build)
		}
	}
	w.Python, w.ABI, w.Platform = tags[0], tags[1], tags[2]
	if !ValidName(strings.ReplaceAll(w.Name, "_", "-")) {
		return Wheel{}, fmt.Errorf("%w: %q: %q is not a project name", format.ErrInvalid, filename, w.Name)
	}
	return w, nil
}

// Sdist kinds: the extensions of a source distribution Lajstrom stores.
const (
	SdistTarGz = ".tar.gz"
	SdistZip   = ".zip"
)

// Sdist is what the file name of a source distribution says: {name}-{version}.tar.gz (PEP
// 625), or .zip (legacy).
type Sdist struct {
	Name, Version string
	// Kind is SdistTarGz or SdistZip.
	Kind string
}

// Root returns the top-level directory of the sdist archive, {name}-{version}.
func (s Sdist) Root() string { return s.Name + "-" + s.Version }

// ParseSdistFilename reads the file name of a source distribution. The version is what follows
// the last '-', so legacy names with '-' in the project name read as the setuptools of their time
// wrote them. Errors wrap format.ErrInvalid; another archive kind (.tar.bz2, .tgz) is one too.
func ParseSdistFilename(filename string) (Sdist, error) {
	var s Sdist
	stem := ""
	for _, kind := range []string{SdistTarGz, SdistZip} {
		if st, ok := strings.CutSuffix(filename, kind); ok {
			stem, s.Kind = st, kind
		}
	}
	if s.Kind == "" || strings.ContainsAny(stem, "/\\") {
		return Sdist{}, fmt.Errorf("%w: %q is not a source distribution: .tar.gz or .zip", format.ErrInvalid, filename)
	}
	i := strings.LastIndexByte(stem, '-')
	if i <= 0 || i == len(stem)-1 {
		return Sdist{}, fmt.Errorf("%w: %q: a source distribution is named <name>-<version>", format.ErrInvalid, filename)
	}
	s.Name, s.Version = stem[:i], stem[i+1:]
	if !ValidName(s.Name) {
		return Sdist{}, fmt.Errorf("%w: %q: %q is not a project name", format.ErrInvalid, filename, s.Name)
	}
	return s, nil
}

// multipleUse are the fields of the core metadata that may occur more than once, by their JSON
// keys: PEP 566 makes each a list.
var multipleUse = []string{
	"dynamic", "platform", "supported_platform", "classifier", "requires_dist", "requires_external",
	"project_url", "provides_extra", "provides_dist", "obsoletes_dist", "license_file", "requires", "provides", "obsoletes",
}

// CoreMetadata is a METADATA or PKG-INFO file, read.
type CoreMetadata struct {
	// Fields are the fields in the JSON form of PEP 566: keys lowercase with '-' as '_', the
	// multiple-use fields as lists of strings, keywords as a list split on whitespace, the
	// body as description.
	Fields map[string]any
	// Name and Version are the Name and Version fields, as written.
	Name, Version string
}

// ParseCoreMetadata reads a core metadata file: headers in the email format, continuation lines
// starting with whitespace, and the body after the first empty line, the description of
// Metadata-Version 2.1 and later. Metadata-Version, Name and Version are required. Errors wrap
// format.ErrInvalid.
func ParseCoreMetadata(b []byte) (CoreMetadata, error) {
	fields := map[string]any{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), maxCoreMetadata)
	var last string // the key of the field a continuation line continues
	var body []string
	inBody := false
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		switch {
		case inBody:
			body = append(body, line)
		case line == "":
			inBody = true
		case (line[0] == ' ' || line[0] == '\t') && last != "":
			appendContinuation(fields, last, strings.TrimLeft(line, " \t"))
		default:
			k, v, ok := strings.Cut(line, ":")
			if !ok || k == "" {
				return CoreMetadata{}, fmt.Errorf("%w: core metadata: the line %q is not a field", format.ErrInvalid, line)
			}
			last = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(k)), "-", "_")
			v = strings.TrimSpace(v)
			if slices.Contains(multipleUse, last) {
				list, _ := fields[last].([]string)
				fields[last] = append(list, v)
			} else {
				fields[last] = v
			}
		}
	}
	if err := sc.Err(); err != nil {
		return CoreMetadata{}, fmt.Errorf("%w: core metadata: %w", format.ErrInvalid, err)
	}
	if d := strings.TrimRight(strings.Join(body, "\n"), "\n"); d != "" {
		fields["description"] = d
	}
	if kw, ok := fields["keywords"].(string); ok {
		fields["keywords"] = strings.Fields(kw)
	}
	m := CoreMetadata{Fields: fields}
	m.Name, _ = fields["name"].(string)
	m.Version, _ = fields["version"].(string)
	for _, required := range []struct{ key, value string }{{"metadata_version", str(fields["metadata_version"])}, {"name", m.Name}, {"version", m.Version}} {
		if required.value == "" {
			return CoreMetadata{}, fmt.Errorf("%w: core metadata without %s", format.ErrInvalid, required.key)
		}
	}
	return m, nil
}

// appendContinuation adds a continuation line to the field key.
func appendContinuation(fields map[string]any, key, line string) {
	switch v := fields[key].(type) {
	case []string:
		v[len(v)-1] += "\n" + line
	case string:
		fields[key] = v + "\n" + line
	}
}

// str returns v as a string, "" when it is none.
func str(v any) string {
	s, _ := v.(string)
	return s
}

// ReadWheelMetadata returns the METADATA of a wheel: the file METADATA of the one
// *.dist-info directory at the top of the archive. Errors wrap format.ErrInvalid, except a
// failure to read.
func ReadWheelMetadata(r io.ReaderAt, size int64) ([]byte, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("%w: the wheel is not a zip archive: %w", format.ErrInvalid, err)
	}
	var found *zip.File
	for _, f := range zr.File {
		dir, name := path.Split(f.Name)
		if name != "METADATA" || strings.Count(dir, "/") != 1 || !strings.HasSuffix(dir, ".dist-info/") {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: the wheel has two .dist-info directories: %s and %s", format.ErrInvalid, found.Name, f.Name)
		}
		found = f
	}
	if found == nil {
		return nil, fmt.Errorf("%w: the wheel has no .dist-info/METADATA", format.ErrInvalid)
	}
	return readMember(found)
}

// ReadSdistPKGInfo returns the PKG-INFO at the top of a source distribution:
// <root>/PKG-INFO, root being {name}-{version}. A .tar.gz is read as a stream; a .zip needs
// r to be an io.ReaderAt as well, with size. Errors wrap format.ErrInvalid, except a failure
// to read.
func ReadSdistPKGInfo(r io.Reader, size int64, s Sdist) ([]byte, error) {
	want := s.Root() + "/PKG-INFO"
	if s.Kind == SdistZip {
		ra, ok := r.(io.ReaderAt)
		if !ok {
			return nil, errors.New("pypi: a zip sdist needs random access")
		}
		zr, err := zip.NewReader(ra, size)
		if err != nil {
			return nil, fmt.Errorf("%w: the sdist is not a zip archive: %w", format.ErrInvalid, err)
		}
		for _, f := range zr.File {
			if f.Name == want {
				return readMember(f)
			}
		}
		return nil, fmt.Errorf("%w: the sdist has no %s", format.ErrInvalid, want)
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("%w: the sdist is not gzip: %w", format.ErrInvalid, err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: the sdist has no %s", format.ErrInvalid, want)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: the sdist is not a tar archive: %w", format.ErrInvalid, err)
		}
		if path.Clean(h.Name) == want && h.Typeflag == tar.TypeReg {
			return readLimited(tr, h.Size)
		}
	}
}

// readMember returns the bytes of a zip member, at most maxCoreMetadata.
func readMember(f *zip.File) ([]byte, error) {
	if f.UncompressedSize64 > maxCoreMetadata {
		return nil, fmt.Errorf("%w: %s is %d bytes, more than %d", format.ErrInvalid, f.Name, f.UncompressedSize64, maxCoreMetadata)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", format.ErrInvalid, f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	return readLimited(rc, int64(f.UncompressedSize64))
}

// readLimited reads r, which says it holds size bytes, refusing more than maxCoreMetadata.
func readLimited(r io.Reader, size int64) ([]byte, error) {
	if size > maxCoreMetadata {
		return nil, fmt.Errorf("%w: the core metadata is %d bytes, more than %d", format.ErrInvalid, size, maxCoreMetadata)
	}
	b, err := io.ReadAll(io.LimitReader(r, maxCoreMetadata+1))
	if err != nil {
		return nil, fmt.Errorf("%w: the core metadata: %w", format.ErrInvalid, err)
	}
	if len(b) > maxCoreMetadata {
		return nil, fmt.Errorf("%w: the core metadata is more than %d bytes", format.ErrInvalid, maxCoreMetadata)
	}
	return b, nil
}

package deb

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
)

// The source packages of an apt repo (#79; docs/spec/manifest-profile.md, deb). A source version
// is a .dsc and the files it names, in one suite and component: lajstrom.name is its Source,
// lajstrom.version its Version, lajstrom.deb.arch is ArchSource and the variant
// <suite>/<component>/source.

// ArchSource is the architecture of a source package: what a .changes calls the source, the
// lajstrom.deb.arch of a source version, and an architecture no binary package may have.
const ArchSource = "source"

// MaxDsc is the largest .dsc read: 1 MiB. One names its files with three lines each, and the
// largest in Debian are a few KiB.
const MaxDsc = 1 << 20

// maxSourceFiles is the most files a .dsc may name.
const maxSourceFiles = 64

// Fields of a .dsc (Debian Policy §5.4) that the plugin reads.
const (
	FieldFormat          = "Format"
	FieldFiles           = "Files"
	FieldChecksumsSha1   = "Checksums-Sha1"
	FieldChecksumsSha256 = "Checksums-Sha256"
)

// The armor of an OpenPGP clearsigned message (RFC 9580 §7).
const (
	armorSigned    = "-----BEGIN PGP SIGNED MESSAGE-----"
	armorSignature = "-----BEGIN PGP SIGNATURE-----"
	armorEnd       = "-----END PGP SIGNATURE-----"
)

// sourceFileName is the grammar of a file a .dsc names: no directory, and the characters of a
// package name and a version, an "_" between them (hello_2.10.orig.tar.gz).
var sourceFileName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*_[A-Za-z0-9.+~_-]+$`)

// Dsc is what a .dsc says: its fields and the files it names.
type Dsc struct {
	// Control is the paragraph of the .dsc, every field as written, without the armor of a
	// signed one.
	Control Control
	// Source, Version and Format are those fields.
	Source, Version, Format string
	// Files are the files the .dsc names, in the order of Checksums-Sha256, each with the
	// checksums of every list: the md5 of Files, the sha1 of Checksums-Sha1 when it has one.
	Files []SourceFile
	// Filename is DscFilename(Source, Version), what dpkg-source names the .dsc.
	Filename string
}

// SourceFile is one file a .dsc names: its name, size and the checksums the .dsc gives it,
// lowercase hex; SHA1 is empty when the .dsc has no Checksums-Sha1.
type SourceFile struct {
	Name              string
	Size              int64
	MD5, SHA1, SHA256 string
}

// DscFilename returns the name of the .dsc of source at version: <source>_<version without the
// epoch>.dsc, as dpkg-source writes it.
func DscFilename(source, version string) string {
	return source + "_" + withoutEpoch(version) + ".dsc"
}

// withoutEpoch returns version without its epoch, as the names of pool files have it.
func withoutEpoch(version string) string {
	if _, v, ok := strings.Cut(version, ":"); ok {
		return v
	}
	return version
}

// ParseDsc reads a .dsc (Debian Policy §5.4), signed or not: an OpenPGP clearsigned message is
// unwrapped (its signature is not checked) and the paragraph inside it parsed as a control file.
// Format, Source, Version, Files and Checksums-Sha256 are required, Source and Version with
// their grammars; Files, Checksums-Sha256 and, when present, Checksums-Sha1 must list the same
// files with the same sizes, each at most once, at most 64, named <source>_… without a
// directory, the .dsc itself not among them. Errors wrap format.ErrInvalid.
func ParseDsc(b []byte) (*Dsc, error) {
	if len(b) > MaxDsc {
		return nil, fmt.Errorf("%w: the .dsc has %d bytes, at most %d", format.ErrInvalid, len(b), MaxDsc)
	}
	text, err := unarmor(string(b))
	if err != nil {
		return nil, err
	}
	c, err := parseControl([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("the .dsc: %w", err)
	}
	d := &Dsc{Control: c}
	for _, f := range []struct {
		name  string
		dst   *string
		check func(what, s string) error
	}{
		{FieldFormat, &d.Format, nil},
		{FieldSource, &d.Source, checkName},
		{FieldVersion, &d.Version, checkVersion},
	} {
		v, ok := c.Get(f.name)
		switch {
		case !ok || v == "":
			return nil, fmt.Errorf("%w: the .dsc has no %s field", format.ErrInvalid, f.name)
		case strings.Contains(v, "\n"):
			return nil, fmt.Errorf("%w: the .dsc field %s has more than one line", format.ErrInvalid, f.name)
		case f.check != nil:
			if err := f.check(f.name, v); err != nil {
				return nil, err
			}
		}
		*f.dst = v
	}
	d.Filename = DscFilename(d.Source, d.Version)
	if d.Files, err = dscFiles(c, d); err != nil {
		return nil, err
	}
	return d, nil
}

// unarmor returns the text of a .dsc: the message of an OpenPGP clearsigned one, its armor
// headers and its signature block left out and its dash-escaped lines unescaped, or the whole of
// an unsigned one. A clearsigned message that does not end in a signature block, or has more
// than blank lines after it, wraps format.ErrInvalid.
func unarmor(s string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i == len(lines) || strings.TrimRight(lines[i], " \t") != armorSigned {
		return s, nil
	}
	i++
	for i < len(lines) && strings.TrimSpace(lines[i]) != "" { // armor headers: Hash: SHA512
		i++
	}
	text, rest, err := signedText(lines[min(i+1, len(lines)):])
	if err != nil {
		return "", err
	}
	return text, checkSignature(rest)
}

// signedText returns the text of the lines of a clearsigned message, up to the signature block,
// dash-escaped lines unescaped, and the lines of the signature block, its first line left out.
func signedText(lines []string) (string, []string, error) {
	var text strings.Builder
	for i, line := range lines {
		if strings.TrimRight(line, " \t") == armorSignature {
			return text.String(), lines[i+1:], nil
		}
		if strings.HasPrefix(line, "-") {
			rest, ok := strings.CutPrefix(line, "- ")
			if !ok {
				return "", nil, fmt.Errorf("%w: the signed .dsc has a line that starts with \"-\" and is not dash-escaped: %q", format.ErrInvalid, line)
			}
			line = rest
		}
		text.WriteString(line)
		text.WriteByte('\n')
	}
	return "", nil, fmt.Errorf("%w: the signed .dsc has no %s", format.ErrInvalid, armorSignature)
}

// checkSignature checks the lines of a signature block after its first: they end with armorEnd,
// and only blank lines follow it. The signature itself is not read.
func checkSignature(lines []string) error {
	end := slices.IndexFunc(lines, func(l string) bool { return strings.TrimRight(l, " \t") == armorEnd })
	if end < 0 {
		return fmt.Errorf("%w: the signed .dsc has no %s", format.ErrInvalid, armorEnd)
	}
	for _, l := range lines[end+1:] {
		if strings.TrimSpace(l) != "" {
			return fmt.Errorf("%w: the signed .dsc has text after %s", format.ErrInvalid, armorEnd)
		}
	}
	return nil
}

// dscFiles returns the files of the lists of c, the paragraph of d, in the order of
// Checksums-Sha256, with the md5 of Files and the sha1 of Checksums-Sha1.
func dscFiles(c Control, d *Dsc) ([]SourceFile, error) {
	sha256s, err := checksumList(c, FieldChecksumsSha256, 64, true)
	if err != nil {
		return nil, err
	}
	md5s, err := checksumList(c, FieldFiles, 32, true)
	if err != nil {
		return nil, err
	}
	sha1s, err := checksumList(c, FieldChecksumsSha1, 40, false)
	if err != nil {
		return nil, err
	}
	if len(sha256s) > maxSourceFiles {
		return nil, fmt.Errorf("%w: the .dsc names %d files, at most %d", format.ErrInvalid, len(sha256s), maxSourceFiles)
	}
	md5By, sha1By := byName(md5s), byName(sha1s)
	files := make([]SourceFile, 0, len(sha256s))
	for _, l := range sha256s {
		f, err := sourceFileOf(d, l, md5By, sha1By, len(sha1s) > 0)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	if len(md5s) != len(files) || (len(sha1s) > 0 && len(sha1s) != len(files)) {
		return nil, fmt.Errorf("%w: the .dsc's %s, %s and %s do not list the same files", format.ErrInvalid,
			FieldFiles, FieldChecksumsSha1, FieldChecksumsSha256)
	}
	return files, nil
}

// sourceFileOf returns the file of the line l of Checksums-Sha256 of d, with the md5 of its line
// in Files and, when hasSHA1, the sha1 of its line in Checksums-Sha1, which must give it the size
// l gives it.
func sourceFileOf(d *Dsc, l listEntry, md5By, sha1By map[string]listEntry, hasSHA1 bool) (SourceFile, error) {
	switch {
	case l.name == d.Filename:
		return SourceFile{}, fmt.Errorf("%w: the .dsc names itself, %s", format.ErrInvalid, l.name)
	case !strings.HasPrefix(l.name, d.Source+"_"):
		return SourceFile{}, fmt.Errorf("%w: the .dsc names %s, which is not a file of the source %s (%s_…)", format.ErrInvalid, l.name, d.Source, d.Source)
	}
	f := SourceFile{Name: l.name, Size: l.size, SHA256: l.sum}
	m, ok := md5By[l.name]
	if !ok || m.size != l.size {
		return SourceFile{}, fmt.Errorf("%w: the .dsc's %s and %s disagree on %s", format.ErrInvalid, FieldFiles, FieldChecksumsSha256, l.name)
	}
	f.MD5 = m.sum
	if hasSHA1 {
		s, ok := sha1By[l.name]
		if !ok || s.size != l.size {
			return SourceFile{}, fmt.Errorf("%w: the .dsc's %s and %s disagree on %s", format.ErrInvalid, FieldChecksumsSha1, FieldChecksumsSha256, l.name)
		}
		f.SHA1 = s.sum
	}
	return f, nil
}

// byName returns the lines of a list by their names.
func byName(list []listEntry) map[string]listEntry {
	m := make(map[string]listEntry, len(list))
	for _, l := range list {
		m[l.name] = l
	}
	return m
}

// listEntry is one line of a checksum list of a .dsc.
type listEntry struct {
	sum, name string
	size      int64
}

// checksumList parses the field name of c, a list of " <checksum> <size> <name>" lines after
// an empty first line, each checksum hexLen lowercase hex digits (stored in lowercase), each
// name a file name of the grammar sourceFileName and listed once. A missing field is an error
// when required, else no lines.
func checksumList(c Control, name string, hexLen int, required bool) ([]listEntry, error) {
	v, ok := c.Get(name)
	if !ok {
		if required {
			return nil, fmt.Errorf("%w: the .dsc has no %s field", format.ErrInvalid, name)
		}
		return nil, nil
	}
	first, rest, _ := strings.Cut(v, "\n")
	if first != "" {
		return nil, fmt.Errorf("%w: the .dsc field %s has a value on its first line; it is a list of files, one a line", format.ErrInvalid, name)
	}
	var out []listEntry
	seen := map[string]bool{}
	for i, line := range strings.Split(rest, "\n") {
		parts := strings.Fields(line)
		if len(parts) != 3 {
			return nil, fmt.Errorf("%w: line %d of the .dsc field %s is not \"<checksum> <size> <name>\"", format.ErrInvalid, i+1, name)
		}
		sum, sizeText, file := strings.ToLower(parts[0]), parts[1], parts[2]
		size, err := strconv.ParseInt(sizeText, 10, 64)
		switch _, herr := hex.DecodeString(sum); {
		case len(sum) != hexLen || herr != nil:
			return nil, fmt.Errorf("%w: line %d of the .dsc field %s: %q is not %d hex digits", format.ErrInvalid, i+1, name, parts[0], hexLen)
		case err != nil || size < 0:
			return nil, fmt.Errorf("%w: line %d of the .dsc field %s: %q is not a size", format.ErrInvalid, i+1, name, sizeText)
		case !sourceFileName.MatchString(file):
			return nil, fmt.Errorf("%w: line %d of the .dsc field %s: %q is not the name of a file of a source package", format.ErrInvalid, i+1, name, file)
		case seen[file]:
			return nil, fmt.Errorf("%w: the .dsc field %s lists %s twice", format.ErrInvalid, name, file)
		}
		seen[file] = true
		out = append(out, listEntry{sum: sum, name: file, size: size})
	}
	return out, nil
}

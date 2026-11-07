package deb

import (
	"archive/tar"
	"compress/gzip"
	"crypto/md5"  //nolint:gosec // apt's Packages names an md5 of the file; not a security use
	"crypto/sha1" //nolint:gosec // apt's Packages names a sha1 of the file; not a security use
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"

	"github.com/lajstrom/lajstrom/internal/format"
)

// MaxControl is the largest control member Parse reads, as stored in the archive and
// uncompressed: 4 MiB. A control member holds control, md5sums and the maintainer scripts;
// md5sums is the one that grows, by a line per file of the package.
const MaxControl = 4 << 20

const (
	// maxDebianBinary is the largest debian-binary member read; "2.0\n" has four bytes.
	maxDebianBinary = 1 << 10
	// md5Len is the length of an md5 in hex.
	md5Len = 32
)

// Package is what a .deb says about itself: the control file and the fields the plugin uses,
// md5sums, and, from Parse, the size and the checksums of the whole file.
type Package struct {
	// Control is the control file, every field as written.
	Control Control
	// Name, Version and Architecture are the fields Package, Version and Architecture.
	Name, Version, Architecture string
	// Maintainer is the field Maintainer.
	Maintainer string
	// Description is the synopsis: the first line of the field Description.
	Description string
	// Source is the source package name: the name of the field Source, or Name without one.
	Source string
	// SourceVersion is the version of the field Source when it gives one ("name (version)");
	// empty when the source version is Version.
	SourceVersion string
	// MD5Sums is md5sums as path to lowercase hex md5, paths as written (without "./"); nil
	// when the package has none, which policy allows.
	MD5Sums map[string]string
	// ControlMember is the name of the control member: control.tar, control.tar.gz,
	// control.tar.xz or control.tar.zst.
	ControlMember string
	// Filename is Filename(Name, Version, Architecture), the name of the file in a pool.
	Filename string
	// PoolPath is PoolPath(Source, Filename), the path of the file below pool/<component>/; the
	// component is a property of the upload, not of the file.
	PoolPath string
	// Size is the size of the whole file in bytes; 0 from ParseControl.
	Size int64
	// MD5, SHA1 and SHA256 are the lowercase hex digests of the whole file, the three that
	// apt's Packages lists; empty from ParseControl.
	MD5, SHA1, SHA256 string
}

// Parse reads a binary package (deb(5)) from r to its end in one pass: the ar archive, the
// debian-binary member, which must say 2.0, the control member (control.tar, .gz, .xz or .zst,
// at most MaxControl bytes stored and uncompressed) with control and md5sums, and the
// data.tar.* member, which it does not open; members after data.tar are ignored, as deb(5)
// asks. Every byte goes through md5, sha1 and sha256 on the way. Errors about the bytes wrap
// format.ErrInvalid and say what was wrong; a failure of r is returned as it is, wrapped.
func Parse(r io.Reader) (*Package, error) {
	src := &source{r: r}
	sums := newSums(src)
	pkg, err := parse(sums, MaxControl)
	if err != nil {
		return nil, src.blame(err)
	}
	pkg.Size, pkg.MD5, pkg.SHA1, pkg.SHA256 = sums.n, hexSum(sums.md5), hexSum(sums.sha1), hexSum(sums.sha256)
	return pkg, nil
}

// parse reads the members of a .deb from r and drains what follows data.tar.
func parse(r io.Reader, limit int64) (*Package, error) {
	ar, err := newArReader(r)
	if err != nil {
		return nil, err
	}
	pkg, err := readControl(ar, limit)
	if err != nil {
		return nil, err
	}
	hdr, _, err := ar.next()
	switch {
	case errors.Is(err, io.EOF):
		return nil, fmt.Errorf("%w: the archive ends after %s, before the data member", format.ErrInvalid, pkg.ControlMember)
	case err != nil:
		return nil, err
	case !isDataMember(hdr.name):
		return nil, fmt.Errorf("%w: the third member is %q, not data.tar with the extension of a compression dpkg reads",
			format.ErrInvalid, hdr.name)
	}
	if err := ar.skip(); err != nil {
		return nil, err
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return nil, fmt.Errorf("deb: read after %s: %w", hdr.name, err)
	}
	return pkg, nil
}

// ParseControl reads a binary package from r only up to the end of its control member: the ar
// archive, debian-binary and the control member, at most limit bytes stored and uncompressed.
// It never reads what follows the control member, so that Validate learns what a blob of
// hundreds of MiB says about itself from its first KiB. Size and the checksums of the result
// are zero. Errors as Parse.
func ParseControl(r io.Reader, limit int64) (*Package, error) {
	src := &source{r: r}
	ar, err := newArReader(src)
	if err != nil {
		return nil, src.blame(err)
	}
	pkg, err := readControl(ar, limit)
	if err != nil {
		return nil, src.blame(err)
	}
	return pkg, nil
}

// readControl reads debian-binary and the control member, the first two members of ar.
func readControl(ar *arReader, limit int64) (*Package, error) {
	if err := readDebianBinary(ar); err != nil {
		return nil, err
	}
	hdr, body, err := ar.next()
	switch {
	case errors.Is(err, io.EOF):
		return nil, fmt.Errorf("%w: the archive ends after debian-binary, before the control member", format.ErrInvalid)
	case err != nil:
		return nil, err
	case hdr.size > limit:
		return nil, fmt.Errorf("%w: the control member %s has %d bytes, at most %d", format.ErrInvalid, hdr.name, hdr.size, limit)
	}
	control, md5sums, err := readControlTar(hdr.name, body, limit)
	if err != nil {
		return nil, err
	}
	pkg, err := packageOf(control)
	if err != nil {
		return nil, err
	}
	if md5sums != nil {
		if pkg.MD5Sums, err = parseMD5Sums(md5sums); err != nil {
			return nil, err
		}
	}
	pkg.ControlMember = hdr.name
	return pkg, nil
}

// readDebianBinary reads the first member, which must be debian-binary and say 2.0 on its first
// line; further lines are allowed (deb(5)).
func readDebianBinary(ar *arReader) error {
	hdr, body, err := ar.next()
	switch {
	case errors.Is(err, io.EOF):
		return fmt.Errorf("%w: the archive has no members", format.ErrInvalid)
	case err != nil:
		return err
	case hdr.name != "debian-binary":
		return fmt.Errorf("%w: the first member is %q, not debian-binary", format.ErrInvalid, hdr.name)
	case hdr.size > maxDebianBinary:
		return fmt.Errorf("%w: debian-binary has %d bytes, at most %d", format.ErrInvalid, hdr.size, maxDebianBinary)
	}
	b, err := io.ReadAll(body)
	switch {
	case err != nil:
		return fmt.Errorf("deb: read debian-binary: %w", err)
	case int64(len(b)) < hdr.size:
		return fmt.Errorf("%w: truncated archive: debian-binary has %d of %d bytes", format.ErrInvalid, len(b), hdr.size)
	}
	if line, _, _ := strings.Cut(string(b), "\n"); line != "2.0" {
		return fmt.Errorf("%w: debian-binary says %q, not 2.0", format.ErrInvalid, line)
	}
	return nil
}

// readControlTar decompresses the control member name as its extension says and reads control
// and md5sums out of the tar archive, which is read to its end; the uncompressed archive may
// have at most limit bytes. md5sums is nil when the archive has none.
func readControlTar(name string, body io.Reader, limit int64) (control, md5sums []byte, err error) {
	tr, closeFn, err := decompressor(name, body)
	if err != nil {
		return nil, nil, err
	}
	defer closeFn()
	files := map[string][]byte{"control": nil, "md5sums": nil}
	t := tar.NewReader(&capReader{r: tr, n: limit, limit: limit, name: name})
	for {
		h, err := t.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s: %w", format.ErrInvalid, name, err)
		}
		file := strings.TrimPrefix(h.Name, "./")
		if _, want := files[file]; !want {
			continue
		}
		if err := readTarFile(name, t, h, files); err != nil {
			return nil, nil, err
		}
	}
	if files["control"] == nil {
		return nil, nil, fmt.Errorf("%w: %s has no control file", format.ErrInvalid, name)
	}
	return files["control"], files["md5sums"], nil
}

// readTarFile reads the regular file h of t into files under its name without "./"; a second
// file of the name, or one that is not a regular file, is refused.
func readTarFile(member string, t *tar.Reader, h *tar.Header, files map[string][]byte) error {
	file := strings.TrimPrefix(h.Name, "./")
	switch {
	case files[file] != nil:
		return fmt.Errorf("%w: %s holds %s twice", format.ErrInvalid, member, file)
	case !h.FileInfo().Mode().IsRegular():
		return fmt.Errorf("%w: %s: %s is not a regular file", format.ErrInvalid, member, h.Name)
	}
	b, err := io.ReadAll(t)
	if err != nil {
		return fmt.Errorf("%w: %s: %s: %w", format.ErrInvalid, member, h.Name, err)
	}
	files[file] = append([]byte{}, b...) // not nil when empty
	return nil
}

// decompressor returns the reader of the tar archive in the control member name and a function
// that releases it. The four control members of deb(5): control.tar (dpkg 1.17.6),
// control.tar.gz, control.tar.xz (1.17.6) and control.tar.zst (1.21.18).
func decompressor(name string, body io.Reader) (io.Reader, func(), error) {
	nop := func() {}
	var (
		r   io.Reader
		err error
	)
	switch name {
	case "control.tar":
		return body, nop, nil
	case "control.tar.gz":
		r, err = gzip.NewReader(body)
	case "control.tar.xz":
		r, err = newXZReader(body)
	case "control.tar.zst":
		// One goroutine decodes as the tar reader reads: the errors of the stream come from
		// Read, and NewReader fails only on its options.
		d, err := zstd.NewReader(body, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true),
			zstd.WithDecoderMaxWindow(maxDictionary))
		if err != nil {
			return nil, nil, fmt.Errorf("deb: zstd: %w", err)
		}
		return d, d.Close, nil
	default:
		return nil, nil, fmt.Errorf("%w: the second member is %q, not control.tar, control.tar.gz, control.tar.xz or control.tar.zst",
			format.ErrInvalid, name)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s: %w", format.ErrInvalid, name, err)
	}
	return r, nop, nil
}

// isDataMember reports whether name is a data member of deb(5): data.tar, uncompressed or
// with .gz, .xz, .zst, .bz2 or .lzma.
func isDataMember(name string) bool {
	switch name {
	case "data.tar", "data.tar.gz", "data.tar.xz", "data.tar.zst", "data.tar.bz2", "data.tar.lzma":
		return true
	}
	return false
}

// capReader passes at most limit bytes of r through and fails the read after them with an
// error wrapping format.ErrInvalid, unless r ends there: it bounds a decompressed stream.
type capReader struct {
	r        io.Reader
	n, limit int64 // bytes left, bytes allowed
	name     string
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		var one [1]byte
		if k, err := c.r.Read(one[:]); k == 0 {
			return 0, err
		}
		return 0, fmt.Errorf("%w: %s has more than %d bytes uncompressed", format.ErrInvalid, c.name, c.limit)
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}

// packageOf checks the fields of a control file that the plugin relies on and returns the
// Package with them. Package, Version and Architecture are required and have their grammars;
// Maintainer and Description are required by Debian Policy §5.3; Source, when present, is
// "name" or "name (version)".
func packageOf(b []byte) (*Package, error) {
	c, err := parseControl(b)
	if err != nil {
		return nil, err
	}
	get := func(name string) (string, error) {
		v, ok := c.Get(name)
		first, _, _ := strings.Cut(v, "\n")
		switch {
		case !ok:
			return "", fmt.Errorf("%w: control has no %s field", format.ErrInvalid, name)
		case first == "":
			return "", fmt.Errorf("%w: control field %s is empty", format.ErrInvalid, name)
		}
		return v, nil
	}
	pkg := &Package{Control: c}
	var errs []error
	for _, f := range []struct {
		name  string
		dst   *string
		check func(what, s string) error
	}{
		{FieldPackage, &pkg.Name, checkName},
		{FieldVersion, &pkg.Version, checkVersion},
		{FieldArchitecture, &pkg.Architecture, checkArchitecture},
		{FieldMaintainer, &pkg.Maintainer, nil},
		{FieldDescription, &pkg.Description, nil},
	} {
		v, err := get(f.name)
		if err == nil && f.check != nil {
			err = f.check(f.name, v)
		}
		if err != nil {
			errs = append(errs, err)
		}
		*f.dst = v
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	pkg.Description, _, _ = strings.Cut(pkg.Description, "\n")
	pkg.Source = pkg.Name
	if s, ok := c.Get(FieldSource); ok {
		if pkg.Source, pkg.SourceVersion, err = SplitSource(s); err != nil {
			return nil, err
		}
	}
	pkg.Filename = Filename(pkg.Name, pkg.Version, pkg.Architecture)
	pkg.PoolPath = PoolPath(pkg.Source, pkg.Filename)
	return pkg, nil
}

// parseMD5Sums reads md5sums: a line per file, the md5 in hex, two spaces (or a space and the
// "*" of md5sum's binary mode) and the path as written, which dh_md5sums writes without "./".
// Empty lines are skipped; an md5 is stored in lowercase. A malformed line, a path twice or a
// path that is not UTF-8 (the map ends up in JSON) is refused with an error wrapping
// format.ErrInvalid.
func parseMD5Sums(b []byte) (map[string]string, error) {
	sums := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		if len(line) < md5Len+3 || line[md5Len] != ' ' || (line[md5Len+1] != ' ' && line[md5Len+1] != '*') {
			return nil, fmt.Errorf("%w: md5sums line %d is not \"<md5>  <path>\"", format.ErrInvalid, i+1)
		}
		sum, p := strings.ToLower(line[:md5Len]), line[md5Len+2:]
		switch _, err := hex.DecodeString(sum); {
		case err != nil:
			return nil, fmt.Errorf("%w: md5sums line %d: %q is not an md5", format.ErrInvalid, i+1, line[:md5Len])
		case !utf8.ValidString(p):
			return nil, fmt.Errorf("%w: md5sums line %d: the path is not UTF-8", format.ErrInvalid, i+1)
		case sums[p] != "":
			return nil, fmt.Errorf("%w: md5sums line %d: %q is listed twice", format.ErrInvalid, i+1, p)
		}
		sums[p] = sum
	}
	return sums, nil
}

// source is the reader a parse reads from. It keeps the first error of r other than io.EOF,
// so that blame can tell a failure to read from a malformed package.
type source struct {
	r   io.Reader
	err error
}

func (s *source) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && s.err == nil {
		s.err = err
	}
	return n, err
}

// blame returns the error of a parse: the failure of the reader when there was one, since what
// the parser made of the missing bytes says nothing about the package; else err, which wraps
// format.ErrInvalid already when it is about the bytes.
func (s *source) blame(err error) error {
	if s.err != nil {
		return fmt.Errorf("deb: read the package: %w", s.err)
	}
	return err
}

// sums passes the bytes of r through md5, sha1 and sha256 and counts them.
type sums struct {
	r                 io.Reader
	md5, sha1, sha256 hash.Hash
	n                 int64
}

func newSums(r io.Reader) *sums {
	return &sums{r: r, md5: md5.New(), sha1: sha1.New(), sha256: sha256.New()} //nolint:gosec // apt's checksums
}

func (s *sums) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	for _, h := range []hash.Hash{s.md5, s.sha1, s.sha256} {
		h.Write(p[:n])
	}
	s.n += int64(n)
	return n, err
}

// hexSum returns the lowercase hex digest of h.
func hexSum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

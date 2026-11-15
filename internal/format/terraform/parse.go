package terraform

import (
	"archive/tar"
	"compress/flate"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
)

// The grammar of a module address (docs/spec/http-routing.md §3, terraform), as Terraform parses
// one: a namespace or a name is letters, digits, - and _, 1 to 64 of them, beginning and ending
// with a letter or a digit, its case kept; a target system is lowercase letters and digits.
var (
	nameGrammar   = regexp.MustCompile(`^[0-9A-Za-z](?:[0-9A-Za-z_-]{0,62}[0-9A-Za-z])?$`)
	systemGrammar = regexp.MustCompile(`^[0-9a-z]{1,64}$`)
)

// versionGrammar is a SemVer 2.0.0 version without build metadata: three numbers without a
// leading zero, and a pre-release whose numeric identifiers have none either.
var versionGrammar = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)` +
	`(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$`)

// The bounds of an archive read, npm's of a tarball (#243): at most maxEntries tar entries and
// maxUnpacked bytes unpacked, the gzip stream read to its end. Variables for the tests.
var (
	maxEntries        = 1 << 18
	maxUnpacked int64 = 1 << 30
)

// Module is a module of a repo: its name and its target system, the last two parts of a module
// address.
type Module struct {
	Name, System string
}

// ParseModule checks a module's name and system against the grammar of a module address and
// returns the module. Errors wrap format.ErrInvalid.
func ParseModule(name, system string) (Module, error) {
	switch {
	case !nameGrammar.MatchString(name):
		return Module{}, fmt.Errorf("%w: %q is not a module name: letters, digits, - and _, 1 to 64, "+
			"beginning and ending with a letter or a digit", format.ErrInvalid, name)
	case !systemGrammar.MatchString(system):
		return Module{}, fmt.Errorf("%w: %q is not a target system: lowercase letters and digits, 1 to 64", format.ErrInvalid, system)
	}
	return Module{Name: name, System: system}, nil
}

// ParsePackageName returns the module of a package name, <name>/<system>, lajstrom.name. Errors
// wrap format.ErrInvalid.
func ParsePackageName(pkg string) (Module, error) {
	name, system, ok := strings.Cut(pkg, "/")
	if !ok {
		return Module{}, fmt.Errorf("%w: %q is not <name>/<system>", format.ErrInvalid, pkg)
	}
	return ParseModule(name, system)
}

// PackageName returns lajstrom.name of the module, <name>/<system>.
func (m Module) PackageName() string { return m.Name + "/" + m.System }

// Filename returns the file name of the archive of version v of the module, the title of its
// file layer: <name>-<system>-<version>.tar.gz.
func (m Module) Filename(v string) string { return m.Name + "-" + m.System + "-" + v + ".tar.gz" }

// CheckVersion checks that v is a SemVer 2.0.0 version without build metadata and without a
// leading v: Terraform orders 1.0.0+a and 1.0.0+b as one version, and asks for the version it
// picks as its version library prints it. Errors wrap format.ErrInvalid.
func CheckVersion(v string) error {
	if !versionGrammar.MatchString(v) {
		return fmt.Errorf("%w: %q is not a version: SemVer 2.0.0 without build metadata and without a leading v, "+
			"such as 1.2.0 or 2.0.0-rc.1", format.ErrInvalid, v)
	}
	return nil
}

// errUnpacked is the error of a bounded reader past its bound.
var errUnpacked = errors.New("terraform: the archive unpacks to too many bytes")

// bounded reads r until left bytes have passed, then fails with errUnpacked.
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

// ReadArchive reads a module archive whole: a gzip-compressed tar whose entries are paths without
// a leading / or a .. component, regular files, directories, and symbolic and hard links whose
// target is such a path, with a regular file among them; pax global headers are skipped. The
// gzip stream is read to its end, so its checksum is checked, and at most maxEntries entries and
// maxUnpacked bytes are read. Errors wrap format.ErrInvalid, except a failure to read r.
func ReadArchive(r io.Reader) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: the archive is not gzip-compressed: %w", format.ErrInvalid, err)
	}
	unpacked := &bounded{r: gz, left: maxUnpacked}
	tr := tar.NewReader(unpacked)
	files := 0
	for n := 0; ; n++ {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return finish(unpacked, files)
		case err != nil:
			return archiveError(err)
		case n == maxEntries:
			return fmt.Errorf("%w: the archive has more than %d entries", format.ErrInvalid, maxEntries)
		}
		regular, err := checkEntry(h)
		if err != nil {
			return err
		}
		if regular {
			files++
		}
	}
}

// finish reads the rest of the gzip stream after the end of the tar archive, for its checksum,
// and refuses an archive without a regular file.
func finish(r io.Reader, files int) error {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return archiveError(err)
	}
	if files == 0 {
		return fmt.Errorf("%w: the archive has no regular file", format.ErrInvalid)
	}
	return nil
}

// archiveError is the error of a read of the archive that failed: the bound, a gzip or tar
// stream that is malformed or cut short (format.ErrInvalid), or the error of the reader.
func archiveError(err error) error {
	switch {
	case errors.Is(err, errUnpacked):
		return fmt.Errorf("%w: the archive unpacks to more than %d bytes", format.ErrInvalid, maxUnpacked)
	case errors.Is(err, gzip.ErrChecksum), errors.Is(err, gzip.ErrHeader):
		return fmt.Errorf("%w: the archive is not a whole gzip stream: %w", format.ErrInvalid, err)
	case errors.Is(err, tar.ErrHeader), errors.Is(err, tar.ErrFieldTooLong), errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("%w: the archive is not a whole tar archive: %w", format.ErrInvalid, err)
	}
	var corrupt flate.CorruptInputError
	if errors.As(err, &corrupt) {
		return fmt.Errorf("%w: the archive is not a whole gzip stream: %w", format.ErrInvalid, err)
	}
	return fmt.Errorf("terraform: read the archive: %w", err)
}

// checkEntry checks the tar entry h and reports whether it is a regular file.
func checkEntry(h *tar.Header) (regular bool, err error) {
	switch h.Typeflag {
	case tar.TypeXGlobalHeader:
		return false, nil
	case tar.TypeReg, tar.TypeDir:
	case tar.TypeSymlink, tar.TypeLink:
		if !relative(h.Linkname) {
			return false, fmt.Errorf("%w: the archive's link %q points at %q, which is absolute or has a .. component",
				format.ErrInvalid, h.Name, h.Linkname)
		}
	default:
		return false, fmt.Errorf("%w: the archive's entry %q is of type %q: only regular files, directories and links are taken",
			format.ErrInvalid, h.Name, string(h.Typeflag))
	}
	if !relative(h.Name) {
		return false, fmt.Errorf("%w: the archive's entry %q is absolute or has a .. component", format.ErrInvalid, h.Name)
	}
	return h.Typeflag == tar.TypeReg, nil
}

// relative reports whether p is a path of the archive that stays in it: not empty, without a
// leading / and without a .. component, a backslash counted as a separator.
func relative(p string) bool {
	p = strings.ReplaceAll(p, `\`, "/")
	if p == "" || path.IsAbs(p) {
		return false
	}
	for _, c := range strings.Split(p, "/") {
		if c == ".." {
			return false
		}
	}
	return true
}

package deb

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
)

const (
	// arMagic starts every ar archive.
	arMagic = "!<arch>\n"
	// arHeaderLen is the size of a member header: name 16, mtime 12, uid 6, gid 6, mode 8,
	// size 10, and the two bytes "`\n".
	arHeaderLen = 60
)

// arReader reads the members of an ar archive in order, the common format that dpkg-deb
// writes (deb(5)): the global header "!<arch>\n", then per member a 60-byte header and the
// data, padded with one byte to an even offset. Names may end with the "/" of GNU ar; the
// symbol table and the long-name table of GNU and BSD ar are not understood, and a .deb has
// neither. It streams: a member's data is read only by whoever reads the reader next returns,
// and next skips what is left of it.
type arReader struct {
	r    io.Reader
	n    int               // members started so far
	body *io.LimitedReader // the data of the current member that is not read yet
	pad  bool              // the current member has an odd size: one byte of padding follows
}

// arHeader is the part of a member header the parser uses.
type arHeader struct {
	name string
	size int64
}

// newArReader reads the global header of r. Errors wrap format.ErrInvalid, except a failure of
// r itself, which the caller tells apart (source).
func newArReader(r io.Reader) (*arReader, error) {
	var magic [len(arMagic)]byte
	if n, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, fmt.Errorf("%w: not an ar archive: %d bytes, the header %q has %d", format.ErrInvalid, n, arMagic, len(arMagic))
	}
	if string(magic[:]) != arMagic {
		return nil, fmt.Errorf("%w: not an ar archive: it starts with %q, not %q", format.ErrInvalid, magic[:], arMagic)
	}
	return &arReader{r: r}, nil
}

// next skips the rest of the current member and its padding and returns the header of the next
// member and a reader of exactly its data. At the end of the archive it returns io.EOF.
func (a *arReader) next() (arHeader, io.Reader, error) {
	if err := a.skip(); err != nil {
		return arHeader{}, nil, err
	}
	var h [arHeaderLen]byte
	switch n, err := io.ReadFull(a.r, h[:]); {
	case errors.Is(err, io.EOF):
		return arHeader{}, nil, io.EOF
	case err != nil:
		return arHeader{}, nil, fmt.Errorf("%w: truncated archive: the header of member %d has %d of %d bytes",
			format.ErrInvalid, a.n+1, n, arHeaderLen)
	}
	a.n++
	hdr, err := parseArHeader(h[:])
	if err != nil {
		return arHeader{}, nil, fmt.Errorf("%w: member %d: %w", format.ErrInvalid, a.n, err)
	}
	a.body = &io.LimitedReader{R: a.r, N: hdr.size}
	a.pad = hdr.size%2 == 1
	return hdr, a.body, nil
}

// skip reads what is left of the current member and its padding byte.
func (a *arReader) skip() error {
	if a.body == nil {
		return nil
	}
	if _, err := io.Copy(io.Discard, a.body); err != nil {
		return fmt.Errorf("deb: member %d: %w", a.n, err)
	}
	if a.body.N > 0 {
		return fmt.Errorf("%w: truncated archive: member %d lacks %d bytes", format.ErrInvalid, a.n, a.body.N)
	}
	a.body = nil
	if a.pad {
		var b [1]byte
		if _, err := io.ReadFull(a.r, b[:]); err != nil {
			return fmt.Errorf("%w: truncated archive: member %d lacks its padding byte", format.ErrInvalid, a.n)
		}
	}
	return nil
}

// parseArHeader reads the name and the size of a member header and checks its last two bytes.
// The name is the field without trailing spaces and without one trailing "/"; mtime, uid, gid
// and mode are not looked at.
func parseArHeader(h []byte) (arHeader, error) {
	if end := string(h[58:60]); end != "`\n" {
		return arHeader{}, fmt.Errorf("the header ends with %q, not %q", end, "`\n")
	}
	name := strings.TrimSuffix(strings.TrimRight(string(h[:16]), " "), "/")
	field := strings.TrimRight(string(h[48:58]), " ")
	if field == "" || strings.Trim(field, "0123456789") != "" {
		return arHeader{}, fmt.Errorf("%q: size %q is not a decimal number", name, field)
	}
	size, _ := strconv.ParseInt(field, 10, 64) // at most ten digits: never out of range
	return arHeader{name: name, size: size}, nil
}

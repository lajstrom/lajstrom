package pypi

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"slices"

	"github.com/lajstrom/lajstrom/internal/format"
)

// maxEntries bounds the members of an archive read for its core metadata (#243): the headers
// of a zip's central directory, and the tar entries of an sdist up to its PKG-INFO. The most on
// PyPI in November 2026 are the 58 569 of the homeassistant wheel, which gains about 8 000 a
// year, and the 61 370 of its sdist, with PKG-INFO fourth; 2^18 is 4.3 times that. A zip's
// directory is read a header at a time, and only the members that may be the core metadata are
// kept (scanZip): at the bound that is a few milliseconds and 140 KiB, where zip.NewReader,
// which keeps a File of each header, held 70 MiB (#696).
const maxEntries = 1 << 18

// Signatures and lengths of the zip records read here (APPNOTE.TXT 6.3, §4.3.9 to §4.3.16),
// and the end of an archive in which zip.NewReader looks for the end record.
const (
	zipHeaderSig      = 0x02014b50
	zipHeaderLen      = 46
	zipEndLen         = 22
	zip64EndSig       = 0x06064b50
	zip64EndLen       = 56
	zip64LocSig       = 0x07064b50
	zip64LocLen       = 20
	zipEndSearch      = 65 << 10
	dataDescriptorSig = 0x08074b50
	dataDescriptorLen = 16
)

var zipEndSig = []byte("PK\x05\x06")

// errNoDirectory is the error of an archive in which the central directory is not found, and
// errBadDirectory of one whose directory zip.NewReader refuses all the same.
var (
	errNoDirectory  = errors.New("no zip central directory")
	errBadDirectory = errors.New("the zip central directory is cut short, or has not the entries its end record says")
)

// checkZipEntries refuses, with format.ErrInvalid, the zip archive r of size bytes, the wheel or
// the sdist what, when its central directory has more than maxEntries headers, or is not found,
// or zip.NewReader would not read it; and returns the members of it that scanZip keeps, the
// first keep whose name match takes. A failure of r is returned as it is.
func checkZipEntries(r io.ReaderAt, size int64, what string, match func([]byte) bool, keep int) ([]zipMember, error) {
	s, err := scanZip(r, size, maxEntries, match, keep)
	switch {
	case errors.Is(err, errNoDirectory), errors.Is(err, errBadDirectory):
		return nil, fmt.Errorf("%w: the %s is not a zip archive: %w", format.ErrInvalid, what, err)
	case err != nil:
		return nil, fmt.Errorf("pypi: the zip directory of the %s: %w", what, err)
	case s.headers > maxEntries:
		return nil, fmt.Errorf("%w: the %s has more than %d zip entries", format.ErrInvalid, what, maxEntries)
	}
	return s.members, nil
}

// zipScan is what scanZip read of a central directory: the headers it counted, and the members
// it kept.
type zipScan struct {
	headers int
	members []zipMember
}

// scanZip reads the central directory of the zip archive r of size bytes as zip.NewReader reads
// it (Go 1.27), a header at a time through a buffer, and keeps of the members only the first
// keep whose name match takes: what it holds does not grow with the directory, where
// zip.NewReader holds a File of each header (#696). It counts the headers up to limit+1: each
// with its signature and its name, extra field and comment, also past one whose zip64 extra
// field stops zip.NewReader, and also where zip.NewReader would read them had it chosen the
// other place (zipDirectory.alt), so that no count zip.NewReader could read is above it; an end
// record that declares more than limit is limit+1 too, and more than limit is for the caller to
// refuse. The count the end record declares does not bound the headers: zip.NewReader reads
// every one from the start of the directory on and compares their number with it modulo 65 536
// only. scanZip returns errNoDirectory when the directory is not found, errBadDirectory when
// zip.NewReader would refuse it all the same, and a failure of r as it is.
func scanZip(r io.ReaderAt, size int64, limit int, match func([]byte) bool, keep int) (zipScan, error) {
	d, err := findZipDirectory(r, size)
	if err != nil {
		return zipScan{}, err
	}
	if d.records > uint64(limit) { //nolint:gosec // limit is maxEntries or a test's, not negative
		return zipScan{headers: limit + 1}, nil
	}
	start, base, other := d.start, d.base, d.alt
	if d.alt >= 0 && validHeaderAt(r, size, d.alt) {
		start, base, other = d.alt, 0, d.start
	}
	s, read, err := scanHeaders(r, size, start, base, limit, match, keep)
	if err != nil {
		return zipScan{}, err
	}
	if other >= 0 {
		o, _, err := scanHeaders(r, size, other, 0, limit, nil, 0)
		if err != nil {
			return zipScan{}, err
		}
		s.headers = max(s.headers, o.headers)
	}
	if s.headers <= limit && (read < 0 || uint16(read) != uint16(d.records)) { //nolint:gosec // modulo 65 536, as zip.NewReader compares them
		return zipScan{}, errBadDirectory
	}
	return s, nil
}

// scanHeaders reads the headers of a central directory from start on, base added to their
// offsets, for scanZip: it counts them, up to limit+1, and keeps the members, as scanZip says.
// It returns them with the number of headers zip.NewReader reads, which stops before a header
// whose zip64 extra field it refuses, or -1 when zip.NewReader fails: where the file ends before
// a header, or before its name, extra field and comment.
func scanHeaders(r io.ReaderAt, size, start, base int64, limit int, match func([]byte) bool, keep int) (zipScan, int, error) {
	br := bufio.NewReaderSize(io.NewSectionReader(r, start, size-start), 64<<10)
	var s zipScan
	var h dirHeader
	read, stopped := 0, false
	for s.headers <= limit {
		switch err := h.read(br); {
		case errors.Is(err, io.EOF) && !stopped:
			return s, -1, nil
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, errNotHeader):
			return s, read, nil
		case err != nil:
			return zipScan{}, 0, err
		}
		s.headers++
		if stopped {
			continue
		}
		m, ok := h.member(base)
		if stopped = !ok; stopped {
			continue
		}
		read++
		if len(s.members) < keep && match(h.name()) {
			m.name = string(h.name())
			s.members = append(s.members, m)
		}
	}
	return s, read, nil
}

// validHeaderAt reports whether a central directory header that zip.NewReader reads is at off,
// where it then reads the directory, with no base (readDirectoryEnd).
func validHeaderAt(r io.ReaderAt, size, off int64) bool {
	var h dirHeader
	if off >= size || h.read(io.NewSectionReader(r, off, size-off)) != nil {
		return false
	}
	_, ok := h.member(0)
	return ok
}

// errNotHeader is the error of a read of a header without its signature.
var errNotHeader = errors.New("not a central directory header")

// dirHeader is a central directory header as it is read (APPNOTE.TXT 4.3.12): its fixed part,
// then its name, extra field and comment.
type dirHeader struct {
	fixed [zipHeaderLen]byte
	vary  []byte
}

// read reads the next header of r as readDirectoryHeader of archive/zip does: io.EOF when r
// ends before its fixed part or before the rest, io.ErrUnexpectedEOF in the middle of one,
// errNotHeader without the signature.
func (h *dirHeader) read(r io.Reader) error {
	if _, err := io.ReadFull(r, h.fixed[:]); err != nil {
		return err
	}
	if binary.LittleEndian.Uint32(h.fixed[:]) != zipHeaderSig {
		return errNotHeader
	}
	n := int(binary.LittleEndian.Uint16(h.fixed[28:])) + int(binary.LittleEndian.Uint16(h.fixed[30:])) +
		int(binary.LittleEndian.Uint16(h.fixed[32:]))
	h.vary = slices.Grow(h.vary[:0], n)[:n]
	_, err := io.ReadFull(r, h.vary)
	return err
}

// name returns the name of the header read.
func (h *dirHeader) name() []byte { return h.vary[:binary.LittleEndian.Uint16(h.fixed[28:])] }

// member returns the member that the header read states, base added to its offset, as
// readDirectoryHeader reads it: a size or the offset that is saturated is in a zip64 extra
// field. It is false when readDirectoryHeader refuses the header: a zip64 extra field too short
// for what is saturated.
func (h *dirHeader) member(base int64) (zipMember, bool) {
	f := h.fixed[:]
	m := zipMember{flags: binary.LittleEndian.Uint16(f[8:]), method: binary.LittleEndian.Uint16(f[10:]),
		crc32: binary.LittleEndian.Uint32(f[16:]), compressed: uint64(binary.LittleEndian.Uint32(f[20:])),
		size: uint64(binary.LittleEndian.Uint32(f[24:])), offset: int64(binary.LittleEndian.Uint32(f[42:]))}
	nameLen := int(binary.LittleEndian.Uint16(f[28:]))
	for extra := h.vary[nameLen : nameLen+int(binary.LittleEndian.Uint16(f[30:]))]; len(extra) >= 4; {
		id, n := binary.LittleEndian.Uint16(extra), int(binary.LittleEndian.Uint16(extra[2:]))
		if n > len(extra)-4 {
			break
		}
		field := extra[4 : 4+n]
		extra = extra[4+n:]
		if id == zip64ExtraID && !m.zip64(f, field) {
			return zipMember{}, false
		}
	}
	m.offset += base
	return m, true
}

// zip64 reads the zip64 extra field into the member of the fixed part f, as readDirectoryHeader
// does: the uncompressed size, the compressed size and the offset, 8 bytes each, those that are
// saturated, each zip64 field again. It is false when the field is too short.
func (m *zipMember) zip64(f, field []byte) bool {
	next := func(saturated bool, v *uint64) bool {
		if !saturated {
			return true
		}
		if len(field) < 8 {
			return false
		}
		*v, field = binary.LittleEndian.Uint64(field), field[8:]
		return true
	}
	offset := uint64(m.offset) //nolint:gosec // back to int64 below, as readDirectoryHeader converts it
	ok := next(binary.LittleEndian.Uint32(f[24:]) == math.MaxUint32, &m.size) &&
		next(binary.LittleEndian.Uint32(f[20:]) == math.MaxUint32, &m.compressed) &&
		next(m.offset == math.MaxUint32, &offset)
	m.offset = int64(offset) //nolint:gosec // wraps, as readDirectoryHeader converts it
	return ok
}

// zipDirectory is where zip.NewReader reads the central directory: from start, base added to the
// offsets of the members, or from alt with no base when it is not -1 and a header is there; and
// the entries the end record declares.
type zipDirectory struct {
	records          uint64
	start, base, alt int64
}

// findZipDirectory finds the central directory as zip.NewReader does (Go 1.27): it ends at the
// end record of findZipEnd, or starts at the offset that record states when that is past where
// it would start then.
func findZipDirectory(r io.ReaderAt, size int64) (zipDirectory, error) {
	end, err := findZipEnd(r, size)
	if err != nil {
		return zipDirectory{}, err
	}
	if end.size > math.MaxInt64 || end.offset > math.MaxInt64 {
		return zipDirectory{}, errNoDirectory
	}
	// As zip.NewReader computes them, wrapping around alike.
	dirSize, dirOffset := int64(end.size), int64(end.offset)
	base := end.at - dirSize - dirOffset
	d := zipDirectory{records: end.records, start: base + dirOffset, base: base, alt: -1}
	if d.start < 0 || d.start >= size {
		return zipDirectory{}, errNoDirectory
	}
	if base > 0 {
		d.alt = dirOffset
	}
	return d, nil
}

// zipEnd is an end record: where it is, the entries it declares, and the size and the offset
// of the directory.
type zipEnd struct {
	at                    int64
	records, size, offset uint64
}

// findZipEnd returns the end record zip.NewReader reads: the last in the last 65 KiB, with a
// comment that fits; instead of it, the zip64 end record when one of its fields is saturated
// and a zip64 locator of disk 0 of 1 is before it.
func findZipEnd(r io.ReaderAt, size int64) (zipEnd, error) {
	n := int64(zipEndSearch) // the tail read is at most this, whatever size says
	if size < n {
		n = max(size, 0)
	}
	tail := make([]byte, n)
	if _, err := r.ReadAt(tail, size-int64(len(tail))); err != nil && !errors.Is(err, io.EOF) {
		return zipEnd{}, err
	}
	i := bytes.LastIndex(tail[:max(len(tail)-zipEndLen+len(zipEndSig), 0)], zipEndSig)
	if i < 0 || i+zipEndLen+int(binary.LittleEndian.Uint16(tail[i+20:])) > len(tail) {
		return zipEnd{}, errNoDirectory
	}
	b := tail[i:]
	end := zipEnd{at: size - int64(len(tail)) + int64(i), records: uint64(binary.LittleEndian.Uint16(b[10:])),
		size: uint64(binary.LittleEndian.Uint32(b[12:])), offset: uint64(binary.LittleEndian.Uint32(b[16:]))}
	if end.records != 0xffff && end.size != 0xffffffff && end.offset != 0xffffffff {
		return end, nil
	}
	at, err := zip64End(r, end.at)
	if err != nil || at < 0 {
		return end, err
	}
	rec := make([]byte, zip64EndLen)
	if err := readFull(r, rec, at); err != nil {
		return zipEnd{}, err
	}
	if binary.LittleEndian.Uint32(rec) != zip64EndSig {
		return zipEnd{}, errNoDirectory
	}
	return zipEnd{at: at, records: binary.LittleEndian.Uint64(rec[32:]), size: binary.LittleEndian.Uint64(rec[40:]),
		offset: binary.LittleEndian.Uint64(rec[48:])}, nil
}

// zip64End returns the offset of the zip64 end record that the zip64 locator before the end
// record at endAt names, or -1 when there is no locator there, or it names another disk or an
// offset past math.MaxInt64: zip.NewReader then reads the end record as it is.
func zip64End(r io.ReaderAt, endAt int64) (int64, error) {
	if endAt < zip64LocLen {
		return -1, nil
	}
	loc := make([]byte, zip64LocLen)
	if err := readFull(r, loc, endAt-zip64LocLen); err != nil {
		return 0, err
	}
	at := binary.LittleEndian.Uint64(loc[8:])
	if binary.LittleEndian.Uint32(loc) != zip64LocSig || binary.LittleEndian.Uint32(loc[4:]) != 0 ||
		binary.LittleEndian.Uint32(loc[16:]) != 1 || at > math.MaxInt64 {
		return -1, nil
	}
	return int64(at), nil
}

// readFull reads len(b) bytes of r at off; an archive that ends before has no directory there.
func readFull(r io.ReaderAt, b []byte, off int64) error {
	n, err := r.ReadAt(b, off)
	switch {
	case n == len(b):
		return nil
	case err == nil || errors.Is(err, io.EOF):
		return errNoDirectory
	}
	return err
}

// zipMember is a member of a zip as its central directory header states it: what reading it
// takes.
type zipMember struct {
	name             string
	flags, method    uint16
	crc32            uint32
	compressed, size uint64
	offset           int64 // of its local header in the file
}

// readMember returns the bytes of the member m of the zip r, at most maxCoreMetadata.
func readMember(r io.ReaderAt, m zipMember) ([]byte, error) {
	if m.size > maxCoreMetadata {
		return nil, fmt.Errorf("%w: %s is %d bytes, more than %d", format.ErrInvalid, m.name, m.size, maxCoreMetadata)
	}
	rc, err := m.open(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", format.ErrInvalid, m.name, err)
	}
	defer func() { _ = rc.Close() }()
	return readLimited(rc, int64(m.size))
}

// open opens the member m of the zip r as File.Open of archive/zip does (Go 1.27), with its
// errors: its data after its local header, stored or deflated, through checkedMember. m is not
// a directory, whose name ends with '/', which File.Open reads as empty: no core metadata is.
func (m zipMember) open(r io.ReaderAt) (io.ReadCloser, error) {
	var h [localHeaderLen]byte
	if _, err := r.ReadAt(h[:], m.offset); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(h[:]) != localHeaderSig {
		return nil, zip.ErrFormat
	}
	body := m.offset + localHeaderLen + int64(binary.LittleEndian.Uint16(h[26:])) + int64(binary.LittleEndian.Uint16(h[28:]))
	n := int64(m.compressed) //nolint:gosec // wraps, as File.Open converts it
	data := io.NewSectionReader(r, body, n)
	c := &checkedMember{m: m, crc: crc32.NewIEEE()}
	switch m.method {
	case zip.Store:
		c.ReadCloser = io.NopCloser(data)
	case zip.Deflate:
		c.ReadCloser = flate.NewReader(data)
	default:
		return nil, zip.ErrAlgorithm
	}
	if m.flags&0x8 != 0 {
		c.descriptor = io.NewSectionReader(r, body+n, dataDescriptorLen)
	}
	return c, nil
}

// checkedMember reads a member as checksumReader of archive/zip does: past the size that the
// directory states it fails, and at the end of the data when it is shorter, or when its CRC-32
// is not that of the data descriptor, if any, and of the directory; or not that of the
// directory, when it is not 0.
type checkedMember struct {
	io.ReadCloser
	m          zipMember
	descriptor io.Reader // nil without a data descriptor
	crc        hash.Hash32
	n          uint64
	err        error // sticky
}

// Read implements io.Reader.
func (c *checkedMember) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.ReadCloser.Read(p)
	_, _ = c.crc.Write(p[:n])
	c.n += uint64(n) //nolint:gosec // a count of bytes, not negative
	switch {
	case c.n > c.m.size:
		return 0, zip.ErrFormat
	case errors.Is(err, io.EOF) && c.n != c.m.size:
		return 0, io.ErrUnexpectedEOF
	case errors.Is(err, io.EOF):
		err = c.end()
	}
	c.err = err
	return n, err
}

// end checks the CRC-32 of the data, read to its end: io.EOF when it is right.
func (c *checkedMember) end() error {
	if c.descriptor == nil {
		if c.m.crc32 != 0 && c.crc.Sum32() != c.m.crc32 {
			return zip.ErrChecksum
		}
		return io.EOF
	}
	var b [12]byte // the CRC-32 and the sizes, after the signature when there is one
	_, err := io.ReadFull(c.descriptor, b[:4])
	if err == nil {
		off := 4
		if binary.LittleEndian.Uint32(b[:]) == dataDescriptorSig {
			off = 0
		}
		_, err = io.ReadFull(c.descriptor, b[off:])
	}
	switch {
	case errors.Is(err, io.EOF):
		return io.ErrUnexpectedEOF
	case err != nil:
		return err
	case binary.LittleEndian.Uint32(b[:]) != c.m.crc32 || c.crc.Sum32() != c.m.crc32:
		return zip.ErrChecksum
	}
	return io.EOF
}

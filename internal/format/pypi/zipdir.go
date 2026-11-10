package pypi

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/lajstrom/lajstrom/internal/format"
)

// maxEntries bounds the members of an archive read for its core metadata (#243): the headers
// of a zip's central directory, of which zip.NewReader keeps a File each, about 280 bytes and
// the name, so 70 MiB at the bound; and the tar entries of an sdist up to its PKG-INFO. The
// most on PyPI in November 2026 are the 58 569 of the homeassistant wheel, which gains about
// 8 000 a year, and the 61 370 of its sdist, with PKG-INFO fourth; 2^18 is 4.3 times that.
const maxEntries = 1 << 18

// Signatures and lengths of the zip records read here (APPNOTE.TXT 6.3, §4.3.12 to §4.3.16),
// and the end of an archive in which zip.NewReader looks for the end record.
const (
	zipHeaderSig = 0x02014b50
	zipHeaderLen = 46
	zipEndLen    = 22
	zip64EndSig  = 0x06064b50
	zip64EndLen  = 56
	zip64LocSig  = 0x07064b50
	zip64LocLen  = 20
	zipEndSearch = 65 << 10
)

var zipEndSig = []byte("PK\x05\x06")

// errNoDirectory is the error of an archive in which the central directory is not found.
var errNoDirectory = errors.New("no zip central directory")

// checkZipEntries refuses, with format.ErrInvalid, the zip archive r of size bytes, the wheel
// or the sdist what, when zip.NewReader would read more than maxEntries headers of its central
// directory, or when its directory is not found; a failure of r is returned as it is.
func checkZipEntries(r io.ReaderAt, size int64, what string) error {
	n, err := zipEntries(r, size, maxEntries)
	switch {
	case errors.Is(err, errNoDirectory):
		return fmt.Errorf("%w: the %s is not a zip archive: %w", format.ErrInvalid, what, err)
	case err != nil:
		return fmt.Errorf("pypi: the zip directory of the %s: %w", what, err)
	case n > maxEntries:
		return fmt.Errorf("%w: the %s has more than %d zip entries", format.ErrInvalid, what, maxEntries)
	}
	return nil
}

// zipEntries returns how many central directory headers zip.NewReader reads from the archive r
// of size bytes, or more when it would stop earlier, counting up to limit+1; an end record
// that declares more than limit entries is limit+1 too. The count the end record declares
// does not bound the File values: zip.NewReader reads every header from the start of the
// directory on and compares their number with it modulo 65 536 only.
func zipEntries(r io.ReaderAt, size int64, limit int) (int, error) {
	d, err := findZipDirectory(r, size)
	if err != nil {
		return 0, err
	}
	if d.records > uint64(limit) { //nolint:gosec // limit is maxEntries or a test's, not negative
		return limit + 1, nil
	}
	n, err := zipHeaders(r, size, d.start, limit)
	if err != nil || d.alt < 0 {
		return n, err
	}
	alt, err := zipHeaders(r, size, d.alt, limit)
	return max(n, alt), err
}

// zipDirectory is where zip.NewReader reads the central directory: from start, or from alt
// when it is not -1 and a header is there; and the entries the end record declares.
type zipDirectory struct {
	records    uint64
	start, alt int64
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
	d := zipDirectory{records: end.records, start: base + dirOffset, alt: -1}
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

// zipHeaders counts the central directory headers from off on as zip.NewReader reads them:
// the 46 bytes with the signature, then the name, the extra field and the comment, until one
// is not there or is cut short; up to limit+1.
func zipHeaders(r io.ReaderAt, size, off int64, limit int) (int, error) {
	if off < 0 || off >= size {
		return 0, nil
	}
	br := bufio.NewReaderSize(io.NewSectionReader(r, off, size-off), 64<<10)
	h := make([]byte, zipHeaderLen)
	n := 0
	for ; n <= limit; n++ {
		_, err := io.ReadFull(br, h)
		if err == nil && binary.LittleEndian.Uint32(h) == zipHeaderSig {
			_, err = br.Discard(int(binary.LittleEndian.Uint16(h[28:])) + int(binary.LittleEndian.Uint16(h[30:])) +
				int(binary.LittleEndian.Uint16(h[32:])))
			if err == nil {
				continue
			}
		}
		if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		return 0, err
	}
	return n, nil
}

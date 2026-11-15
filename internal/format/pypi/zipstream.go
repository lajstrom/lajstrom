package pypi

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"

	"github.com/lajstrom/lajstrom/internal/format"
)

// The inspector of a zip that streams into the registry, a wheel or a .zip sdist (ADR 0014):
// the central directory, which names the core metadata, is at the end of the file. The
// inspector holds the end, and the local entries whose name may be the core metadata as they
// pass, and at Close reads the core metadata from what it held as ReadWheelMetadata, or
// ReadSdistPKGInfo, reads it from the whole file: a read of bytes it did not hold makes it ask
// for the file back from the registry (format.ErrReadBack), and then those reads go there and
// must agree with what it held. Real wheels need no read-back: their directories are at most
// 2.9 MiB, and the walk follows the entries whose sizes are in a data descriptor after their
// data, as Go's zip.Writer writes them (#684), although none of 93 inspected for the ADR has
// one. A directory of more than maxEntries headers, which the read would refuse, is refused at
// Close without one (#683).

const (
	// heldTail is how much of the end of a zip the inspector holds: the end record and the
	// central directory, the whole file when it is smaller.
	heldTail = 4 << 20
	// maxHeldEntry bounds a local entry the inspector holds: its header and the compressed core
	// metadata, at most maxCoreMetadata unpacked.
	maxHeldEntry = maxCoreMetadata + 64<<10
	// maxHeldEntries bounds the local entries it holds together: two of the largest. Past it, a
	// file of many entries that look like the core metadata is read back.
	maxHeldEntries = 2 * maxHeldEntry
)

// The local file header (APPNOTE.TXT 4.3.7), and the longest data descriptor (4.3.9): its
// signature, the CRC-32 and both sizes in 8 bytes each.
const (
	localHeaderSig     = 0x04034b50
	localHeaderLen     = 30
	zip64ExtraID       = 0x0001
	zip64DescriptorLen = 24
)

// descriptorSig is the signature of a data descriptor as it is in a file.
var descriptorSig = binary.LittleEndian.AppendUint32(nil, dataDescriptorSig)

// zipInspector is the inspector of a zip; Inspect makes it with newZipInspector.
type zipInspector struct {
	filename string
	// read returns the core metadata of the zip r of size bytes: ReadWheelMetadata, or the
	// PKG-INFO of a .zip sdist.
	read     func(r io.ReaderAt, size int64) ([]byte, error)
	walk     localWalk
	tailSize int    // heldTail, but in the tests
	tail     []byte // the last tailSize bytes, a ring that starts at start until Close
	start    int
	size     int64

	held     *heldZip // what it held, from Close on
	core     []byte   // the core metadata, once found
	readBack bool     // Close needed bytes it did not hold
}

// newZipInspector returns the inspector of the zip filename: read reads its core metadata, and
// candidate says which local entries may be it.
func newZipInspector(filename string, candidate func(name []byte) bool, read func(io.ReaderAt, int64) ([]byte, error)) *zipInspector {
	return &zipInspector{filename: filename, read: read, walk: localWalk{candidate: candidate}, tailSize: heldTail}
}

// Write implements io.Writer: it never fails, the zip is read at Close.
func (z *zipInspector) Write(p []byte) (int, error) {
	z.walk.write(p)
	z.size += int64(len(p))
	q := p
	if n := len(z.tail); n < z.tailSize {
		k := min(z.tailSize-n, len(q))
		if n+k > cap(z.tail) {
			z.tail = slices.Grow(z.tail, min(z.tailSize, max(n+k, 2*cap(z.tail)))-n)
		}
		z.tail, q = append(z.tail, q[:k]...), q[k:]
	}
	if len(q) > z.tailSize {
		q = q[len(q)-z.tailSize:] // writes the whole ring once more, from start back to start
	}
	for len(q) > 0 {
		k := copy(z.tail[z.start:], q)
		z.start, q = (z.start+k)%z.tailSize, q[k:]
	}
	return len(p), nil
}

// Close implements format.Inspector: it reads the core metadata from what it held. A zip the
// reader refuses is its error, but when the reader needed bytes not held: the file is then read
// back.
func (z *zipInspector) Close() error {
	if z.held != nil {
		return nil
	}
	slices.Reverse(z.tail[:z.start]) // the ring in order: rotated by start
	slices.Reverse(z.tail[z.start:])
	slices.Reverse(z.tail)
	z.held = &heldZip{segs: append(z.walk.held, segment{off: z.size - int64(len(z.tail)), b: z.tail}), size: z.size}
	if z.tooManyEntries() {
		return fmt.Errorf("%s: %w: the zip has more than %d entries", z.filename, format.ErrInvalid, maxEntries)
	}
	core, err := z.read(z.held, z.size)
	switch {
	case z.held.missed:
		z.readBack = true
	case err != nil:
		return fmt.Errorf("%s: %w", z.filename, err)
	default:
		z.core = core
	}
	return nil
}

// tooManyEntries reports whether the walk counted more than maxEntries headers of the central
// directory where the end record held says it starts: scanZip counts the same headers from
// there and checkZipEntries refuses the zip, but would read them back when the directory is
// larger than the end held. A directory the walk did not reach, or that starts elsewhere, is
// left to it.
func (z *zipInspector) tooManyEntries() bool {
	if !z.walk.inDir || z.walk.dirCount <= maxEntries {
		return false
	}
	h := *z.held // a copy: a read of bytes not held asks for no read-back here
	d, err := findZipDirectory(&h, z.size)
	return err == nil && (d.start == z.walk.dirAt || d.alt == z.walk.dirAt)
}

// coreMetadata implements inspector.
func (z *zipInspector) coreMetadata(back io.ReaderAt) ([]byte, error) {
	switch {
	case z.core != nil:
		return z.core, nil
	case !z.readBack:
		return nil, fmt.Errorf("pypi: %s: the inspector was not closed", z.filename)
	case back == nil:
		return nil, fmt.Errorf("pypi: %s: its core metadata is not in what streamed: %w", z.filename, format.ErrReadBack)
	}
	h := *z.held
	h.back = back
	core, err := z.read(&h, z.size)
	if err != nil {
		return nil, fmt.Errorf("%s, read back: %w", z.filename, err)
	}
	z.core = core
	return core, nil
}

// segment is bytes of a file at their offset.
type segment struct {
	off int64
	b   []byte
}

// localWalk follows the local entries of a zip as its bytes pass: a header, its name and extra
// field, then the compressed data of the size the header says, then the next header. The data
// of an entry whose sizes follow it (flag bit 3) ends at the first data descriptor that agrees
// with it and that a local or central directory header follows (descriptor).
// It holds the entries whose name candidate takes, header and data, and data descriptor if any,
// up to maxHeldEntry each and maxHeldEntries together, and stops at the first record that is not
// a local header (the central directory, or an archive with something before its first entry).
// What it did not hold is read back. When the record it stops at is a central directory header,
// it goes on counting the headers from there as scanZip does, up to maxEntries+1.
type localWalk struct {
	candidate func([]byte) bool
	off       int64 // the offset of the next byte, the first in pend when it holds some
	stopped   bool
	hdr       []byte    // the header being read
	want      int       // the length of hdr once it is read
	left      int64     // the bytes of data left of the entry
	keep      *segment  // the entry being held, nil when its data is skipped
	held      []segment // the entries held
	heldBytes int
	inDir     bool  // the walk is in the central directory
	dirAt     int64 // where its first header is, once inDir
	dirCount  int   // the headers of the directory counted, up to maxEntries+1

	// An entry whose sizes are in a data descriptor after its data.
	desc   bool
	dataAt int64  // where its data begins
	stored bool   // its data is stored, not compressed
	crc    uint32 // the CRC-32 of its data so far, when stored
	pend   []byte // the bytes after its data so far that may begin its data descriptor
}

// write follows the entries through p.
func (w *localWalk) write(p []byte) {
	for len(p) > 0 && !w.stopped {
		if w.desc {
			p = w.scan(p)
			continue
		}
		if w.left > 0 {
			k := int(min(w.left, int64(len(p))))
			if w.keep != nil {
				w.keep.b = append(w.keep.b, p[:k]...)
			}
			w.left, w.off, p = w.left-int64(k), w.off+int64(k), p[k:]
			if w.left == 0 {
				w.end()
			}
			continue
		}
		if w.want == 0 {
			w.want = localHeaderLen
			if w.inDir {
				w.want = zipHeaderLen
			}
		}
		k := min(w.want-len(w.hdr), len(p))
		w.hdr, w.off, p = append(w.hdr, p[:k]...), w.off+int64(k), p[k:]
		switch {
		case len(w.hdr) < w.want:
		case w.inDir:
			w.dirHeader()
		default:
			w.header()
		}
	}
}

// header reads the header in hdr: first its fixed part, then with its name and extra field.
func (w *localWalk) header() {
	h := w.hdr
	if len(h) == localHeaderLen {
		switch binary.LittleEndian.Uint32(h) {
		case localHeaderSig:
		case zipHeaderSig:
			w.inDir, w.dirAt, w.want = true, w.off-localHeaderLen, zipHeaderLen // the rest of the header next
			return
		default:
			w.stopped = true
			return
		}
		if w.want += int(binary.LittleEndian.Uint16(h[26:])) + int(binary.LittleEndian.Uint16(h[28:])); w.want > localHeaderLen {
			return // the name and the extra field come next
		}
	}
	nameLen := int(binary.LittleEndian.Uint16(h[26:]))
	desc := binary.LittleEndian.Uint16(h[6:])&0x8 != 0
	size, ok := uint64(binary.LittleEndian.Uint32(h[18:])), true
	switch {
	case desc:
		size = 0 // in the data descriptor: archive/zip does not read the sizes here
	case size == 0xffffffff:
		size, ok = zip64Size(h[localHeaderLen+nameLen:])
	}
	if !ok || size > 1<<62 {
		w.stopped = true
		return
	}
	if n := int64(len(h)) + int64(size); w.candidate(h[localHeaderLen:localHeaderLen+nameLen]) &&
		n <= maxHeldEntry && int64(w.heldBytes)+n <= maxHeldEntries {
		w.keep = &segment{off: w.off - int64(len(h)), b: append(make([]byte, 0, n), h...)}
	}
	w.hdr, w.want, w.left = w.hdr[:0], 0, int64(size)
	switch {
	case desc:
		w.desc, w.dataAt, w.stored, w.crc = true, w.off, binary.LittleEndian.Uint16(h[8:]) == 0, 0
	case w.left == 0:
		w.end()
	}
}

// scan follows the data of an entry whose sizes follow it through p, and returns what of p is
// after its data descriptor, once found: from a new array when bytes held in pend were before
// p. A descriptor's signature is decided on with the 28 bytes from it on, the longest
// descriptor and the signature after it; until then they wait in pend, and so do the last bytes
// of p when they may begin a signature.
func (w *localWalk) scan(p []byte) []byte {
	if len(w.pend) > 0 {
		p = append(slices.Clip(w.pend), p...)
		w.pend = w.pend[:0]
	}
	for {
		i := bytes.Index(p, descriptorSig)
		if i < 0 {
			i = len(p) - sigPrefix(p)
		}
		w.data(p[:i])
		p = p[i:]
		if len(p) < zip64DescriptorLen+4 {
			w.pend = append(w.pend, p...)
			return nil
		}
		if n := w.descriptor(p); n > 0 || w.stopped {
			return p[n:]
		}
		w.data(p[:1]) // a signature in the data
		p = p[1:]
	}
}

// sigPrefix returns the length of the longest end of p that begins descriptorSig.
func sigPrefix(p []byte) int {
	for k := min(len(p), len(descriptorSig)-1); k > 0; k-- {
		if bytes.HasPrefix(descriptorSig, p[len(p)-k:]) {
			return k
		}
	}
	return 0
}

// data passes d, of the data of an entry whose sizes follow it: into its CRC-32 when it is
// stored, and into the entry held, if any.
func (w *localWalk) data(d []byte) {
	w.off += int64(len(d))
	if w.stored {
		w.crc = crc32.Update(w.crc, crc32.IEEETable, d)
	}
	w.hold(d)
}

// hold appends b to the entry held, which is no longer held once it is larger than
// maxHeldEntry, or than maxHeldEntries leaves of it.
func (w *localWalk) hold(b []byte) {
	if w.keep == nil {
		return
	}
	if n := len(w.keep.b) + len(b); n > maxHeldEntry || w.heldBytes+n > maxHeldEntries {
		w.keep = nil
		return
	}
	w.keep.b = append(w.keep.b, b...)
}

// descriptor reads p, which begins with descriptorSig and is 28 bytes or more, as the data
// descriptor of the entry whose data passed (APPNOTE.TXT 4.3.9): the signature, the CRC-32 and
// both sizes, in 4 bytes each, or in 8 as zip64 writes them; and ends the entry when the
// compressed size is the length of the data, a local or central directory header follows, and
// the CRC-32 and the uncompressed size are those of the data. It returns the length of the
// descriptor, or 0 when the signature is in the data. The CRC-32 of stored data is computed as
// it passes; of compressed data, only of an entry held, inflated; and when that is not it, the
// walk stops, for the entry to be read back. A descriptor without its signature, which
// APPNOTE.TXT allows and no writer of wheels leaves out, is not found: the walk waits for it to
// the end of the file, and the entries after it are read back.
func (w *localWalk) descriptor(p []byte) int {
	n := uint64(w.off - w.dataAt) //nolint:gosec // an offset in the file, not negative
	crc := binary.LittleEndian.Uint32(p[4:])
	agreed := false
	for _, d := range []struct {
		len                      int
		compressed, uncompressed uint64
	}{
		{dataDescriptorLen, uint64(binary.LittleEndian.Uint32(p[8:])), uint64(binary.LittleEndian.Uint32(p[12:]))},
		{zip64DescriptorLen, binary.LittleEndian.Uint64(p[8:]), binary.LittleEndian.Uint64(p[16:])},
	} {
		if next := binary.LittleEndian.Uint32(p[d.len:]); d.compressed != n || next != localHeaderSig && next != zipHeaderSig {
			continue
		}
		agreed = true
		if w.checks(crc, d.uncompressed) {
			w.hold(p[:d.len])
			w.off, w.desc = w.off+int64(d.len), false
			w.end()
			return d.len
		}
	}
	if agreed && !w.stored && w.keep != nil {
		w.stopped = true
	}
	return 0
}

// checks reports whether crc and size are the CRC-32 and the length of the data of the entry
// that passed, as far as the walk can tell: of stored data, it computed them; of compressed
// data, it inflates that of the entry held, and takes the others on the sizes and the header
// after them.
func (w *localWalk) checks(crc uint32, size uint64) bool {
	n := uint64(w.off - w.dataAt) //nolint:gosec // an offset in the file, not negative
	switch {
	case w.stored:
		return crc == w.crc && size == n
	case w.keep == nil:
		return true
	}
	h := crc32.NewIEEE()
	k, err := io.Copy(h, io.LimitReader(flate.NewReader(bytes.NewReader(w.keep.b[w.dataAt-w.keep.off:])), maxCoreMetadata+1))
	return err == nil && h.Sum32() == crc && uint64(k) == size //nolint:gosec // a count of bytes, not negative
}

// dirHeader reads the central directory header in hdr: its name, extra field and comment are
// skipped, and it counts once they have passed, as scanZip does.
func (w *localWalk) dirHeader() {
	h := w.hdr
	if binary.LittleEndian.Uint32(h) != zipHeaderSig {
		w.stopped = true
		return
	}
	n := int64(binary.LittleEndian.Uint16(h[28:])) + int64(binary.LittleEndian.Uint16(h[30:])) + int64(binary.LittleEndian.Uint16(h[32:]))
	w.hdr, w.want, w.left = w.hdr[:0], 0, n
	if w.left == 0 {
		w.end()
	}
}

// end ends the data of an entry, which is held when it was kept, or the variable fields of a
// directory header, which counts it.
func (w *localWalk) end() {
	if w.inDir {
		if w.dirCount++; w.dirCount > maxEntries {
			w.stopped = true
		}
		return
	}
	if w.keep != nil {
		w.held, w.heldBytes, w.keep = append(w.held, *w.keep), w.heldBytes+len(w.keep.b), nil
	}
}

// zip64Size returns the compressed size of the zip64 extra field among extra: in a local
// header it has both sizes, the compressed second (APPNOTE.TXT 4.5.3).
func zip64Size(extra []byte) (uint64, bool) {
	for len(extra) >= 4 {
		id, n := binary.LittleEndian.Uint16(extra), int(binary.LittleEndian.Uint16(extra[2:]))
		if n > len(extra)-4 {
			return 0, false
		}
		if id == zip64ExtraID {
			if n < 16 {
				return 0, false
			}
			return binary.LittleEndian.Uint64(extra[12:]), true
		}
		extra = extra[4+n:]
	}
	return 0, false
}

// errNotHeld is the error of a read of bytes that the inspector did not hold, before a
// read-back.
var errNotHeld = errors.New("pypi: bytes not held as the file streamed")

// heldZip is a zip that streamed, as an io.ReaderAt over size bytes: the segments the inspector
// held, at their offsets, and every other read from back, the file read back from the registry,
// whose bytes must be those held where they overlap. Without back, a read of bytes not held
// fails with errNotHeld and sets missed.
type heldZip struct {
	segs   []segment
	size   int64
	back   io.ReaderAt
	missed bool
}

// ReadAt implements io.ReaderAt.
func (h *heldZip) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("pypi: ReadAt at a negative offset")
	}
	if off >= h.size {
		return 0, io.EOF
	}
	var eof error
	if rest := h.size - off; int64(len(p)) > rest {
		p, eof = p[:rest], io.EOF
	}
	if h.copyHeld(p, off) {
		return len(p), eof
	}
	if h.back == nil {
		h.missed = true
		return 0, errNotHeld
	}
	if n, err := h.back.ReadAt(p, off); n < len(p) {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return n, err
	}
	if err := h.agree(p, off); err != nil {
		return 0, err
	}
	return len(p), eof
}

// copyHeld copies the bytes at off into p and reports whether the segments hold all of them.
func (h *heldZip) copyHeld(p []byte, off int64) bool {
	for len(p) > 0 {
		i := slices.IndexFunc(h.segs, func(s segment) bool { return s.off <= off && off < s.off+int64(len(s.b)) })
		if i < 0 {
			return false
		}
		s := h.segs[i]
		n := copy(p, s.b[off-s.off:])
		p, off = p[n:], off+int64(n)
	}
	return true
}

// agree checks the bytes p at off, read back, against the segments where they overlap.
func (h *heldZip) agree(p []byte, off int64) error {
	for _, s := range h.segs {
		lo, hi := max(off, s.off), min(off+int64(len(p)), s.off+int64(len(s.b)))
		if lo < hi && !bytes.Equal(p[lo-off:hi-off], s.b[lo-s.off:hi-s.off]) {
			return fmt.Errorf("%w: the registry's bytes %d to %d of the file are not those that streamed", format.ErrInvalid, lo, hi-1)
		}
	}
	return nil
}

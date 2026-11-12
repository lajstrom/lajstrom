package pypi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
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
// must agree with what it held. Real wheels need no read-back: none of 93 inspected for the ADR
// has data descriptors, and their directories are at most 2.9 MiB. A directory of more than
// maxEntries headers, which the read would refuse, is refused at Close without one (#683).

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

// The local file header (APPNOTE.TXT 4.3.7).
const (
	localHeaderSig = 0x04034b50
	localHeaderLen = 30
	zip64ExtraID   = 0x0001
)

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
// field, then the compressed data of the size the header says, then the next header. It holds
// the entries whose name candidate takes, header and data, up to maxHeldEntry each and
// maxHeldEntries together, and stops at the first record that is not a local header (the
// central directory, or an archive with something before its first entry), and at an entry
// whose sizes follow its data (a data descriptor, flag bit 3), whose end it cannot find. What it
// did not hold is read back. When the record it stops at is a central directory header, it
// goes on counting the headers from there as scanZip does, up to maxEntries+1.
type localWalk struct {
	candidate func([]byte) bool
	off       int64 // the offset of the next byte
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
}

// write follows the entries through p.
func (w *localWalk) write(p []byte) {
	for len(p) > 0 && !w.stopped {
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
	size, ok := uint64(binary.LittleEndian.Uint32(h[18:])), true
	if size == 0xffffffff {
		size, ok = zip64Size(h[localHeaderLen+nameLen:])
	}
	if binary.LittleEndian.Uint16(h[6:])&0x8 != 0 || !ok || size > 1<<62 {
		w.stopped = true
		return
	}
	if n := int64(len(h)) + int64(size); w.candidate(h[localHeaderLen:localHeaderLen+nameLen]) &&
		n <= maxHeldEntry && int64(w.heldBytes)+n <= maxHeldEntries {
		w.keep = &segment{off: w.off - int64(len(h)), b: append(make([]byte, 0, n), h...)}
	}
	w.hdr, w.want, w.left = w.hdr[:0], 0, int64(size)
	if w.left == 0 {
		w.end()
	}
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

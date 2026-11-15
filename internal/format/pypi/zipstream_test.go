package pypi

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// storedZip returns a zip of the members, name and content in turn, stored. With descriptors its
// local headers have no sizes and a data descriptor follows each entry, as Go's zip.Writer
// writes them; without, the sizes are in the local headers, as Python's zipfile writes them and
// the wheels of PyPI have them.
func storedZip(t testing.TB, descriptors bool, members ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i+1 < len(members); i += 2 {
		h := &zip.FileHeader{Name: members[i], Method: zip.Store}
		var w io.Writer
		var err error
		if descriptors {
			w, err = zw.CreateHeader(h)
		} else {
			n := uint64(len(members[i+1]))
			h.CRC32, h.CompressedSize64, h.UncompressedSize64 = crc32.ChecksumIEEE([]byte(members[i+1])), n, n
			w, err = zw.CreateRaw(h)
		}
		must(t, err)
		_, err = io.WriteString(w, members[i+1])
		must(t, err)
	}
	must(t, zw.Close())
	return buf.Bytes()
}

// deflatedZip returns a zip of the members, name and content in turn, as Go's zip.Writer writes
// them by default: deflated, with the sizes in a data descriptor after each entry.
func deflatedZip(t testing.TB, members ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i+1 < len(members); i += 2 {
		w, err := zw.Create(members[i])
		must(t, err)
		_, err = io.WriteString(w, members[i+1])
		must(t, err)
	}
	must(t, zw.Close())
	return buf.Bytes()
}

// shebang returns the zip b after a shebang line, as a zip that runs is: the inspector cannot
// follow its local entries from there.
func shebang(b []byte) []byte { return append([]byte("#!/usr/bin/env python3\n"), b...) }

// metadata is the METADATA of tool at version.
func metadata(version string) string {
	return "Metadata-Version: 2.1\nName: tool\nVersion: " + version + "\n"
}

// bigData is 6 MiB, more than the end of a zip that the inspector holds.
var bigData = strings.Repeat("0123456789abcdef", 6<<16)

// largeWheel is a wheel of tool at version with its METADATA, of the version meta, first and 6
// MiB of data after it, with data descriptors or not.
func largeWheel(t testing.TB, version, meta string, descriptors bool) (string, []byte) {
	t.Helper()
	return "tool-" + version + "-py3-none-any.whl", storedZip(t, descriptors, "tool-"+version+".dist-info/METADATA", metadata(meta),
		"tool/data.bin", bigData)
}

// TestZipInspector: wheels that Go's zip.Writer writes, with the sizes of each entry in a data
// descriptor after it, deflated and stored, smaller than the end the inspector holds and with
// their METADATA between 6 MiB before it and 6 MiB after, and a wheel of 6 MiB with the sizes in
// its local headers and its METADATA first: the inspector holds the local entry of the METADATA
// as it passes, has the METADATA at Close and asks for no read-back, and its walk goes through
// every entry to the central directory and counts its headers. The last wheel after a shebang
// line, whose entries it cannot follow, asks for the file back (format.ErrReadBack), and has the
// METADATA with the file read back.
func TestZipInspector(t *testing.T) {
	const meta = "tool-1.0.dist-info/METADATA"
	noise := make([]byte, 6<<20) // which deflate does not shrink
	_, _ = rand.NewChaCha8([32]byte{}).Read(noise)
	_, plain := largeWheel(t, "1.0", "1.0", false)
	for _, tt := range []struct {
		name     string
		whl      []byte
		readBack bool
	}{
		{"deflated, small", deflatedZip(t, meta, metadata("1.0"), "tool/__init__.py", "x = 1\n"), false},
		{"stored, small", storedZip(t, true, meta, metadata("1.0"), "tool/__init__.py", "x = 1\n"), false},
		{"deflated, 12 MiB", deflatedZip(t, "tool/a.bin", string(noise), meta, metadata("1.0"), "tool/b.bin", string(noise)), false},
		{"stored, 12 MiB", storedZip(t, true, "tool/a.bin", bigData, meta, metadata("1.0"), "tool/b.bin", bigData), false},
		{"sizes in the local headers, 6 MiB", plain, false},
		{"after a shebang line", shebang(plain), true},
	} {
		z := newZipInspector("tool-1.0-py3-none-any.whl", isWheelMetadata, ReadWheelMetadata)
		for rest := tt.whl; len(rest) > 0; rest = rest[min(len(rest), 32<<10):] {
			_, err := z.Write(rest[:min(len(rest), 32<<10)])
			must(t, err)
		}
		must(t, z.Close())
		core, err := z.coreMetadata(nil)
		if tt.readBack {
			if !errors.Is(err, format.ErrReadBack) {
				t.Fatalf("%s: %q, %v; want ErrReadBack", tt.name, core, err)
			}
			core, err = z.coreMetadata(bytes.NewReader(tt.whl))
		}
		if err != nil || string(core) != metadata("1.0") {
			t.Errorf("%s: %q, %v", tt.name, core, err)
		}
		zr, err := zip.NewReader(bytes.NewReader(tt.whl), int64(len(tt.whl)))
		must(t, err)
		if followed := len(z.walk.held) == 1 && z.walk.inDir && z.walk.dirCount == len(zr.File); followed == tt.readBack {
			t.Errorf("%s: %d entries held, the walk in the directory %v with %d headers of %d", tt.name, len(z.walk.held), z.walk.inDir,
				z.walk.dirCount, len(zr.File))
		}
	}
}

// TestZipInspectorFalseDescriptor (#684): a data descriptor signature in the data of an entry is
// its end only when the sizes after it, the header after them and the CRC-32 are the data's.
// The inspector holds the 65 KiB of the end in which the end record is looked for, and 70 KiB
// of another entry are after the METADATA. In a stored entry, with sizes that are not the
// data's or a CRC-32 that is not, the walk goes on to the real descriptor and holds the
// METADATA after it; with all of them the data's and the local entry of another METADATA after
// them, it holds that one, at the offset where it is, which the central directory does not
// name, and the METADATA is read back. In a deflated METADATA, after its deflate stream, with a
// CRC-32 not that of what the stream inflates to, the walk stops, and the METADATA is read
// back. The METADATA is each time what ReadWheelMetadata reads from the whole file.
func TestZipInspectorFalseDescriptor(t *testing.T) {
	const meta = "tool-1.0.dist-info/METADATA"
	md, prefix, other, last := metadata("1.0"), strings.Repeat("p", 100), metadata("6.6.6"), strings.Repeat("z", 70<<10)
	planted := func(crc uint32, compressed, size int) []byte {
		le32 := binary.LittleEndian.AppendUint32
		return le32(le32(le32(le32(nil, dataDescriptorSig), crc), uint32(compressed)), uint32(size)) //nolint:gosec // the test's short data
	}
	data := func(crc uint32, compressed int) string {
		return prefix + string(planted(crc, compressed, compressed)) + string(localEntry(meta, 0, uint32(len(other)), nil, other)) + //nolint:gosec // a few bytes
			"and then the rest of the data, which is no header"
	}
	crc := crc32.ChecksumIEEE([]byte(prefix))
	var deflated bytes.Buffer
	fw, err := flate.NewWriter(&deflated, flate.DefaultCompression)
	must(t, err)
	_, err = io.WriteString(fw, md)
	must(t, err)
	must(t, fw.Close())
	deflated.Write(planted(crc32.ChecksumIEEE([]byte(md))^1, deflated.Len(), len(md)))
	deflated.Write(localEntry(meta, 0, 0, nil, ""))
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{Name: meta, Method: zip.Deflate, Flags: 0x8, CRC32: crc32.ChecksumIEEE([]byte(md)),
		CompressedSize64: uint64(deflated.Len()), UncompressedSize64: uint64(len(md))}) //nolint:gosec // a few bytes
	must(t, err)
	_, err = w.Write(deflated.Bytes())
	must(t, err)
	w, err = zw.CreateHeader(&zip.FileHeader{Name: "tool/z.bin", Method: zip.Store})
	must(t, err)
	_, err = io.WriteString(w, last)
	must(t, err)
	must(t, zw.Close())
	for _, tt := range []struct {
		name     string
		whl      []byte
		readBack bool
	}{
		{"sizes not the data's", storedZip(t, true, "tool/data.bin", data(crc, len(prefix)+1), meta, md, "tool/z.bin", last), false},
		{"a CRC-32 not the data's", storedZip(t, true, "tool/data.bin", data(crc^1, len(prefix)), meta, md, "tool/z.bin", last), false},
		{"all the data's", storedZip(t, true, "tool/data.bin", data(crc, len(prefix)), meta, md, "tool/z.bin", last), true},
		{"in a deflated METADATA", buf.Bytes(), true},
	} {
		z := newZipInspector("tool-1.0-py3-none-any.whl", isWheelMetadata, ReadWheelMetadata)
		z.tailSize = zipEndSearch
		for rest := tt.whl; len(rest) > 0; rest = rest[min(len(rest), 7):] {
			_, err := z.Write(rest[:min(len(rest), 7)])
			must(t, err)
		}
		must(t, z.Close())
		core, err := z.coreMetadata(nil)
		if errors.Is(err, format.ErrReadBack) != tt.readBack {
			t.Errorf("%s: read back %v, want %v", tt.name, errors.Is(err, format.ErrReadBack), tt.readBack)
		}
		if tt.readBack {
			core, err = z.coreMetadata(bytes.NewReader(tt.whl))
		}
		want, werr := ReadWheelMetadata(bytes.NewReader(tt.whl), int64(len(tt.whl)))
		if err != nil || werr != nil || string(core) != md || !bytes.Equal(core, want) {
			t.Errorf("%s: %q, %v; ReadWheelMetadata %q, %v", tt.name, core, err, want, werr)
		}
	}
}

// TestZipInspectorDirectoryBombs (#683): a directory of more than maxEntries headers, larger
// than the end held, is refused at Close, as the read back would refuse it, and asks for no
// read-back: whether the end record says so or says 1, and after an entry whose sizes are in a
// data descriptor (#684). maxEntries headers are not refused there, and a wheel's directory is
// counted from where its local entries end.
func TestZipInspectorDirectoryBombs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		b       []byte
		refused bool
	}{
		{"the end record says more", testutil.ZipDirectoryBomb(maxEntries+1, maxEntries+1), true},
		{"the end record says 1", testutil.ZipDirectoryBomb(4<<16+1, 1), true},
		{"maxEntries", testutil.ZipDirectoryBomb(maxEntries, maxEntries), false},
		{"after an entry with a data descriptor", append(descriptorEntry("tool/data.bin", "x = 1\n"),
			testutil.ZipDirectoryBomb(4<<16+1, 1)...), true},
	} {
		z := newZipInspector("tool-1.0-py3-none-any.whl", isWheelMetadata, ReadWheelMetadata)
		_, err := z.Write(tc.b)
		must(t, err)
		err = z.Close()
		if tc.refused != (err != nil) || tc.refused && (!errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "more than 262144")) ||
			z.readBack == tc.refused {
			t.Errorf("%s: %v, read back %v", tc.name, err, z.readBack)
		}
	}
	whl := storedZip(t, false, "tool-1.0.dist-info/METADATA", metadata("1.0"), "tool/data.bin", strings.Repeat("x", 3000))
	zr, err := zip.NewReader(bytes.NewReader(whl), int64(len(whl)))
	must(t, err)
	z := newZipInspector("tool-1.0-py3-none-any.whl", isWheelMetadata, ReadWheelMetadata)
	_, err = z.Write(whl)
	must(t, err)
	must(t, z.Close())
	d, err := findZipDirectory(bytes.NewReader(whl), int64(len(whl)))
	must(t, err)
	if !z.walk.inDir || z.walk.dirAt != d.start || z.walk.dirCount != len(zr.File) {
		t.Errorf("a wheel: the directory at %d with %d headers (%v), want %d with %d", z.walk.dirAt, z.walk.dirCount, z.walk.inDir,
			d.start, len(zr.File))
	}
}

// TestHeldZip: the bytes held are read from the segments, across two of them, and the end of the
// file is io.EOF; a read of bytes not held fails with errNotHeld and is missed, or, with the file
// read back, comes from it, and fails when the bytes read back are not those held where they
// overlap.
func TestHeldZip(t *testing.T) {
	file := []byte("0123456789abcdefghij")
	h := &heldZip{segs: []segment{{off: 2, b: file[2:6]}, {off: 6, b: file[6:9]}, {off: 15, b: file[15:]}}, size: int64(len(file))}
	read := func(off, n int64) (string, error) {
		p := make([]byte, n)
		k, err := h.ReadAt(p, off)
		return string(p[:k]), err
	}
	if s, err := read(3, 5); s != "34567" || err != nil {
		t.Errorf("ReadAt(3, 5) = %q, %v; want the two segments", s, err)
	}
	if s, err := read(17, 5); s != "hij" || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt(17, 5) = %q, %v; want hij, io.EOF", s, err)
	}
	if _, err := read(8, 3); !errors.Is(err, errNotHeld) || !h.missed {
		t.Errorf("ReadAt(8, 3) = %v, missed %v; want errNotHeld, missed", err, h.missed)
	}
	h.back = bytes.NewReader(file)
	if s, err := read(8, 3); s != "89a" || err != nil {
		t.Errorf("read back, ReadAt(8, 3) = %q, %v", s, err)
	}
	changed := bytes.Clone(file)
	changed[8] = 'X'
	h.back = bytes.NewReader(changed)
	if _, err := read(8, 3); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("read back other bytes than held: %v, want ErrInvalid", err)
	}
}

// FuzzZipInspector writes a zip to the inspector of a wheel, as ingest does while it streams, in
// pieces of 1 to 256 bytes, holding an end of 1 to 1024 bytes of it, and reads its METADATA as
// Build does: with the file read back when the inspector asks for it. That must be what
// ReadWheelMetadata reads from the whole file, METADATA or a refusal. The seeds are wheels with
// METADATA first and last, with data descriptors and without, deflated, one with a data
// descriptor signature in the data of an entry, one after a shebang line, and a directory that
// says more entries than it has.
func FuzzZipInspector(f *testing.F) {
	whl, _ := testutil.Wheel(testutil.WheelOptions{Name: "tool", Version: "1.0"})
	for _, b := range [][]byte{whl, append([]byte("#!/usr/bin/env python3\n"), whl...),
		storedZip(f, false, "tool-1.0.dist-info/METADATA", metadata("1.0"), "tool/data.bin", strings.Repeat("x", 3000)),
		storedZip(f, true, "tool-1.0.dist-info/METADATA", metadata("1.0"), "tool/data.bin", strings.Repeat("x", 3000)),
		deflatedZip(f, "tool/data.bin", strings.Repeat("x", 3000), "tool-1.0.dist-info/METADATA", metadata("1.0")),
		storedZip(f, true, "tool/data.bin", "x"+string(descriptor("x", false))+"PK\x03\x04 and more", "tool-1.0.dist-info/METADATA", metadata("1.0")),
		storedZip(f, false, "tool/data.bin", strings.Repeat("x", 3000), "tool-1.0.dist-info/METADATA", metadata("1.0")),
		testutil.ZipDirectoryBomb(2, 5)} {
		f.Add(uint8(99), uint16(300), b)
	}
	f.Fuzz(func(t *testing.T, piece uint8, tail uint16, b []byte) {
		z := newZipInspector("tool-1.0-py3-none-any.whl", isWheelMetadata, ReadWheelMetadata)
		z.tailSize = int(tail%1024) + 1
		for rest, n := b, int(piece)+1; len(rest) > 0; rest = rest[min(n, len(rest)):] {
			if _, err := z.Write(rest[:min(n, len(rest))]); err != nil {
				t.Fatal(err)
			}
		}
		var got []byte
		err := z.Close()
		if err == nil {
			got, err = z.coreMetadata(nil)
			if errors.Is(err, format.ErrReadBack) {
				got, err = z.coreMetadata(bytes.NewReader(b))
			}
		}
		want, werr := ReadWheelMetadata(bytes.NewReader(b), int64(len(b)))
		if (err == nil) != (werr == nil) || err == nil && !bytes.Equal(got, want) {
			t.Fatalf("the inspector read %q, %v; ReadWheelMetadata %q, %v", got, err, want, werr)
		}
	})
}

// localEntry returns a local header of name with the flags and the 32-bit sizes size32, and
// the extra field extra, followed by data.
func localEntry(name string, flags uint16, size32 uint32, extra []byte, data string) []byte {
	h := make([]byte, localHeaderLen, localHeaderLen+len(name)+len(extra)+len(data))
	binary.LittleEndian.PutUint32(h, localHeaderSig)
	binary.LittleEndian.PutUint16(h[6:], flags)
	binary.LittleEndian.PutUint32(h[18:], size32)
	binary.LittleEndian.PutUint32(h[22:], size32)
	binary.LittleEndian.PutUint16(h[26:], uint16(len(name)))  //nolint:gosec // the test's short names
	binary.LittleEndian.PutUint16(h[28:], uint16(len(extra))) //nolint:gosec // and extra fields
	return append(append(append(h, name...), extra...), data...)
}

// descriptorEntry returns a local header of name with flag bit 3 and no sizes, followed by data,
// stored, and its data descriptor, as Go's zip.Writer writes it.
func descriptorEntry(name, data string) []byte {
	return append(localEntry(name, 0x8, 0, nil, data), descriptor(data, false)...)
}

// descriptor returns the data descriptor of the stored data: its signature, the CRC-32 and both
// sizes in 4 bytes each, or in 8 with zip64.
func descriptor(data string, zip64 bool) []byte {
	le32, le64 := binary.LittleEndian.AppendUint32, binary.LittleEndian.AppendUint64
	d := le32(le32(nil, dataDescriptorSig), crc32.ChecksumIEEE([]byte(data)))
	if zip64 {
		return le64(le64(d, uint64(len(data))), uint64(len(data)))
	}
	return le32(le32(d, uint32(len(data))), uint32(len(data))) //nolint:gosec // the test's short data
}

// zip64Extra is the zip64 extra field of a local header, n bytes of length after its header of
// four: both sizes, size.
func zip64Extra(n uint16, size uint64) []byte {
	e := make([]byte, 4+16)
	binary.LittleEndian.PutUint16(e, zip64ExtraID)
	binary.LittleEndian.PutUint16(e[2:], n)
	binary.LittleEndian.PutUint64(e[4:], size)
	binary.LittleEndian.PutUint64(e[12:], size)
	return e[:4+min(int(n), 16)]
}

// TestLocalWalk: the walk follows local entries, also of zip64 sizes and of sizes in a data
// descriptor after the data, and holds those whose name is a candidate, header, data and
// descriptor, at the offset they came at, written in pieces of 7 bytes; it stops at the central
// directory, whose headers it counts then, at a zip64 field it cannot read, and at a header that
// lies about its size, where the bytes after it are no header; it waits to the end for a data
// descriptor without its signature; and it holds no entry above maxHeldEntry. What it holds are
// always the file's own bytes at their offsets: the inspector reads nothing else.
func TestLocalWalk(t *testing.T) {
	const meta = "tool-1.0.dist-info/METADATA"
	data, md := strings.Repeat("d", 100), metadata("1.0")
	mdLen := uint32(len(md)) //nolint:gosec // a few bytes
	// A header of the central directory, where the walk stops.
	dir := append([]byte("PK\x01\x02"), make([]byte, 42)...)
	for _, tt := range []struct {
		name    string
		entries [][]byte
		held    []int // the entries held, by index
	}{
		{"sizes in the headers", [][]byte{localEntry("tool/data.bin", 0, 100, nil, data), localEntry(meta, 0, mdLen, nil, md)}, []int{1}},
		{"zip64 sizes", [][]byte{localEntry("tool/data.bin", 0, 0xffffffff, zip64Extra(16, 100), data),
			localEntry(meta, 0, 0xffffffff, zip64Extra(16, uint64(mdLen)), md)}, []int{1}},
		{"a zip64 field too short", [][]byte{localEntry(meta, 0, 0xffffffff, zip64Extra(8, uint64(mdLen)), md)}, nil},
		{"data descriptors", [][]byte{descriptorEntry("tool/data.bin", data), descriptorEntry(meta, md)}, []int{1}},
		{"a data descriptor of zip64 sizes", [][]byte{append(localEntry("tool/data.bin", 0x8, 0, nil, data), descriptor(data, true)...),
			localEntry(meta, 0, mdLen, nil, md)}, []int{1}},
		{"a data descriptor without its signature", [][]byte{append(localEntry("tool/data.bin", 0x8, 0, nil, data), descriptor(data, false)[4:]...),
			localEntry(meta, 0, mdLen, nil, md)}, nil},
		{"a header that says less than its data", [][]byte{localEntry("tool/data.bin", 0, 10, nil, data), localEntry(meta, 0, mdLen, nil, md)}, nil},
		{"an entry above maxHeldEntry", [][]byte{localEntry(meta, 0, maxHeldEntry, nil, strings.Repeat("m", maxHeldEntry)),
			localEntry(meta, 0, mdLen, nil, md)}, []int{1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			file := append(bytes.Join(tt.entries, nil), dir...)
			w := localWalk{candidate: isWheelMetadata}
			for rest := file; len(rest) > 0; rest = rest[min(len(rest), 7):] {
				w.write(rest[:min(len(rest), 7)])
			}
			var want []segment
			off := 0
			for i, e := range tt.entries {
				if slices.Contains(tt.held, i) {
					want = append(want, segment{off: int64(off), b: e})
				}
				off += len(e)
			}
			if !w.stopped && !w.inDir && !w.desc || !reflect.DeepEqual(w.held, want) {
				t.Errorf("stopped %v, held %d entries at %v; want stopped, %d at %v", w.stopped, len(w.held), offsets(w.held), len(want), offsets(want))
			}
			for _, s := range w.held {
				if !bytes.Equal(s.b, file[s.off:s.off+int64(len(s.b))]) {
					t.Errorf("the entry held at %d is not the file's bytes there", s.off)
				}
			}
		})
	}
}

// offsets returns the offsets of segs.
func offsets(segs []segment) []int64 {
	var out []int64
	for _, s := range segs {
		out = append(out, s.off)
	}
	return out
}

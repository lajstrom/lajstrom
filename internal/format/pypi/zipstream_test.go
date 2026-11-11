package pypi

import (
	"archive/zip"
	"bytes"
	"errors"
	"hash/crc32"
	"io"
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

// TestZipInspector: a wheel of 6 MiB, more than the end the inspector holds, with its METADATA
// first: without data descriptors, the inspector held its local entry as it passed and has the
// METADATA at Close; with them, it could not follow the entries and asks for the file back
// (format.ErrReadBack), and has the METADATA with the file read back.
func TestZipInspector(t *testing.T) {
	for _, descriptors := range []bool{false, true} {
		filename, whl := largeWheel(t, "1.0", "1.0", descriptors)
		z := newZipInspector(filename, isWheelMetadata, ReadWheelMetadata)
		for rest := whl; len(rest) > 0; rest = rest[min(len(rest), 32<<10):] {
			_, err := z.Write(rest[:min(len(rest), 32<<10)])
			must(t, err)
		}
		must(t, z.Close())
		core, err := z.coreMetadata(nil)
		if descriptors {
			if !errors.Is(err, format.ErrReadBack) {
				t.Fatalf("with data descriptors: %q, %v; want ErrReadBack", core, err)
			}
			core, err = z.coreMetadata(bytes.NewReader(whl))
		}
		if err != nil || string(core) != metadata("1.0") {
			t.Errorf("data descriptors %v: %q, %v", descriptors, core, err)
		}
		if len(z.tail) != heldTail || len(z.walk.held) != map[bool]int{false: 1, true: 0}[descriptors] {
			t.Errorf("data descriptors %v: the end held is %d bytes, %d entries held", descriptors, len(z.tail), len(z.walk.held))
		}
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
// METADATA first and last, with data descriptors and without, one after a shebang line, and a
// directory that says more entries than it has.
func FuzzZipInspector(f *testing.F) {
	whl, _ := testutil.Wheel(testutil.WheelOptions{Name: "tool", Version: "1.0"})
	for _, b := range [][]byte{whl, append([]byte("#!/usr/bin/env python3\n"), whl...),
		storedZip(f, false, "tool-1.0.dist-info/METADATA", metadata("1.0"), "tool/data.bin", strings.Repeat("x", 3000)),
		storedZip(f, true, "tool-1.0.dist-info/METADATA", metadata("1.0"), "tool/data.bin", strings.Repeat("x", 3000)),
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

package pypi

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestZipEntries counts with scanZip the headers that zip.NewReader reads, which the end record
// states or not, and refuses an archive with no directory to find.
func TestZipEntries(t *testing.T) {
	whl, _ := testutil.Wheel(testutil.WheelOptions{})
	var commented bytes.Buffer
	zw := zip.NewWriter(&commented)
	must(t, zw.SetComment("a comment"))
	_, err := zw.Create("a")
	must(t, err)
	must(t, zw.Close())
	for _, tc := range []struct {
		name  string
		b     []byte
		limit int
		want  int
	}{
		{"a wheel", whl, 10, 4},
		{"a comment", commented.Bytes(), 10, 1},
		{"bytes before the archive", append([]byte("#!/usr/bin/env python3\n"), whl...), 10, 4},
		{"a directory at the bound", testutil.ZipDirectoryBomb(10, 10), 10, 10},
		{"a directory past the bound", testutil.ZipDirectoryBomb(12, 12), 10, 11},
		{"an end record that says more", testutil.ZipDirectoryBomb(3, 12), 10, 11},
		{"a zip64 end record", testutil.ZipDirectoryBomb(5, 1<<16+5), 1 << 17, 5},
		{"an end record that says 1", testutil.ZipDirectoryBomb(1<<16+1, 1), 1 << 17, 1<<16 + 1},
	} {
		s, err := scanZip(bytes.NewReader(tc.b), int64(len(tc.b)), tc.limit, nil, 0)
		if err != nil || s.headers != tc.want {
			t.Errorf("%s: %d, %v; want %d", tc.name, s.headers, err, tc.want)
		}
		if s.headers > tc.limit {
			continue
		}
		if zr, err := zip.NewReader(bytes.NewReader(tc.b), int64(len(tc.b))); err != nil || len(zr.File) != s.headers {
			t.Errorf("%s: zip.NewReader = %v; scanZip counted %d", tc.name, err, s.headers)
		}
	}
	// The last signature of an end record is the end record, though in a comment, as for
	// zip.NewReader; and its comment must fit.
	var signed bytes.Buffer
	zw = zip.NewWriter(&signed)
	must(t, zw.SetComment("PK\x05\x06 is not the end record"))
	must(t, zw.Close())
	for name, b := range map[string][]byte{
		"not a zip": []byte("not a zip, but longer than an end record"), "empty": nil,
		"a comment cut short": commented.Bytes()[:commented.Len()-1], "a signature in the comment": signed.Bytes(),
	} {
		if _, err := scanZip(bytes.NewReader(b), int64(len(b)), 10, nil, 0); !errors.Is(err, errNoDirectory) {
			t.Errorf("%s: %v, want errNoDirectory", name, err)
		}
	}
}

// TestZipBombs reads the bombs of #243 through ReadWheelMetadata and ReadSdistPKGInfo: a
// directory of maxEntries+1 headers, which zip.NewReader would hold in 70 MiB, and one of
// 4*65 536+1 whose end record says 1, which zip.NewReader reads all the same. Each is refused,
// allocating no more than the reads. maxEntries are not (TestZipDirectoryMemory).
func TestZipBombs(t *testing.T) {
	for _, b := range [][]byte{testutil.ZipDirectoryBomb(maxEntries+1, maxEntries+1), testutil.ZipDirectoryBomb(4<<16+1, 1)} {
		var err error
		testutil.CheckAllocated(t, "ReadWheelMetadata", testutil.AllocSlack, func() { _, err = ReadWheelMetadata(bytes.NewReader(b), int64(len(b))) })
		if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "the wheel has more than 262144 zip entries") {
			t.Errorf("a wheel of %d bytes: %v", len(b), err)
		}
		_, err = ReadSdistPKGInfo(bytes.NewReader(b), int64(len(b)), Sdist{Name: "a", Version: "1", Kind: SdistZip})
		if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "the sdist has more than 262144 zip entries") {
			t.Errorf("a zip sdist of %d bytes: %v", len(b), err)
		}
	}
}

// manyEntries returns a wheel of tool 1.0 whose central directory has n headers: its METADATA,
// stored, and n-1 of ZipDirectoryBomb, empty members without local headers. The end record says
// n modulo 65 536, as zip.NewReader compares it, without zip64.
func manyEntries(t testing.TB, n int) []byte {
	t.Helper()
	whl := storedZip(t, false, "tool-1.0.dist-info/METADATA", metadata("1.0"))
	end := slices.Clone(whl[len(whl)-zipEndLen:])
	bomb := testutil.ZipDirectoryBomb(n-1, 0)
	headers := bomb[:len(bomb)-zipEndLen]
	records, dirSize := uint16(n), uint32(len(headers)) //nolint:gosec // n modulo 65 536 on purpose; 12 MiB
	binary.LittleEndian.PutUint16(end[8:], records)
	binary.LittleEndian.PutUint16(end[10:], records)
	binary.LittleEndian.PutUint32(end[12:], binary.LittleEndian.Uint32(end[12:])+dirSize)
	return append(append(whl[:len(whl)-zipEndLen:len(whl)-zipEndLen], headers...), end...)
}

// TestZipDirectoryMemory (#696): ReadWheelMetadata reads the METADATA of a wheel whose directory
// has maxEntries headers, 12 MiB, allocating less than a MiB: it keeps of the directory only
// what may be the METADATA, where zip.NewReader held a File of each header, 70 MiB.
func TestZipDirectoryMemory(t *testing.T) {
	whl := manyEntries(t, maxEntries)
	if zr, err := zip.NewReader(bytes.NewReader(whl), int64(len(whl))); err != nil || len(zr.File) != maxEntries {
		t.Fatalf("zip.NewReader: %v", err)
	}
	var core []byte
	var err error
	testutil.CheckAllocated(t, "ReadWheelMetadata", testutil.AllocSlack, func() { core, err = ReadWheelMetadata(bytes.NewReader(whl), int64(len(whl))) })
	if err != nil || string(core) != metadata("1.0") {
		t.Errorf("%q, %v", core, err)
	}
}

// BenchmarkReadWheelMetadata reads the METADATA of a wheel whose directory has maxEntries
// headers (#696).
func BenchmarkReadWheelMetadata(b *testing.B) {
	whl := manyEntries(b, maxEntries)
	b.ReportAllocs()
	for b.Loop() {
		if core, err := ReadWheelMetadata(bytes.NewReader(whl), int64(len(whl))); err != nil || string(core) != metadata("1.0") {
			b.Fatalf("%q, %v", core, err)
		}
	}
}

// TestReadTarGzFile reads sdists whose PKG-INFO comes after empty files and a file of zeros,
// with the bounds of the call: at a bound PKG-INFO is read, one entry or one byte past it is
// ErrInvalid.
func TestReadTarGzFile(t *testing.T) {
	for _, tc := range []struct {
		entries int   // empty files before PKG-INFO
		zeros   int64 // and the size of the file of zeros
		less    int64 // the bound of bytes less what reading up to PKG-INFO's end takes
		bound   int   // the bound of entries
		want    string
	}{
		{entries: 7, bound: 9},
		{entries: 7, bound: 8, want: "the sdist has no my_pkg-1.0.0/PKG-INFO in its first 8 entries"},
		{zeros: 4096, bound: 3},
		{zeros: 4096, less: 1, bound: 3, want: "the sdist unpacks to more than"},
		{entries: 3, zeros: 1 << 20, less: 1<<20 + 512, bound: 10, want: "up to my_pkg-1.0.0/PKG-INFO"},
	} {
		b, info := testutil.SdistBomb(testutil.SdistOptions{}, tc.entries, tc.zeros)
		// The headers of the top directory, the files and PKG-INFO, the zeros and PKG-INFO.
		need := int64(512*(tc.entries+2)) + tc.zeros + int64(len(info.PKGInfo))
		if tc.zeros > 0 {
			need += 512
		}
		got, err := readTarGzFile(bytes.NewReader(b), info.Root+"/PKG-INFO", need-tc.less, tc.bound)
		switch {
		case tc.want == "" && (err != nil || !bytes.Equal(got, info.PKGInfo)):
			t.Errorf("%+v: %q, %v", tc, got, err)
		case tc.want != "" && (!errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%+v: %v, want ErrInvalid with %q", tc, err, tc.want)
		}
	}
}

// TestSdistBombs reads the bombs of #243 through ReadSdistPKGInfo, with its bounds: a GiB of
// zeros before PKG-INFO, a MiB of gzip, which unbounded cost 13 s per Validate at 64 MiB; and
// maxEntries empty files.
func TestSdistBombs(t *testing.T) {
	if testing.Short() {
		t.Skip("unpacks a GiB")
	}
	for _, tc := range []struct {
		entries int
		zeros   int64
		want    string
	}{
		{0, maxSdistUnpacked, "the sdist unpacks to more than 1073741824 bytes up to my_pkg-1.0.0/PKG-INFO"},
		{maxEntries, 0, "the sdist has no my_pkg-1.0.0/PKG-INFO in its first 262144 entries"},
	} {
		b, info := testutil.SdistBomb(testutil.SdistOptions{}, tc.entries, tc.zeros)
		s, err := ParseSdistFilename(info.Filename)
		must(t, err)
		if _, err := ReadSdistPKGInfo(bytes.NewReader(b), int64(len(b)), s); !errors.Is(err, format.ErrInvalid) ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("%d entries, %d zeros: %v, want ErrInvalid with %q", tc.entries, tc.zeros, err, tc.want)
		}
	}
}

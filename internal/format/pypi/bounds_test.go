package pypi

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestZipEntries counts the headers that zip.NewReader reads, which the end record states or
// not, and refuses an archive with no directory to find.
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
		n, err := zipEntries(bytes.NewReader(tc.b), int64(len(tc.b)), tc.limit)
		if err != nil || n != tc.want {
			t.Errorf("%s: %d, %v; want %d", tc.name, n, err, tc.want)
		}
		if n > tc.limit {
			continue
		}
		if zr, err := zip.NewReader(bytes.NewReader(tc.b), int64(len(tc.b))); err != nil || len(zr.File) != n {
			t.Errorf("%s: zip.NewReader = %v; zipEntries counted %d", tc.name, err, n)
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
		if _, err := zipEntries(bytes.NewReader(b), int64(len(b)), 10); !errors.Is(err, errNoDirectory) {
			t.Errorf("%s: %v, want errNoDirectory", name, err)
		}
	}
}

// TestZipBombs reads the bombs of #243 through ReadWheelMetadata and ReadSdistPKGInfo: a
// directory of maxEntries+1 headers, which zip.NewReader would hold in 70 MiB, and one of
// 4*65 536+1 whose end record says 1, which zip.NewReader reads all the same. Each is refused
// before zip.NewReader, allocating no more than the reads. maxEntries are not.
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
	b := testutil.ZipDirectoryBomb(maxEntries, maxEntries)
	if err := checkZipEntries(bytes.NewReader(b), int64(len(b)), "wheel"); err != nil {
		t.Errorf("%d entries: %v", maxEntries, err)
	}
}

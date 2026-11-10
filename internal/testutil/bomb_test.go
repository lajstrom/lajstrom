package testutil

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"strconv"
	"testing"

	"github.com/ulikunitz/xz"
)

// The bombs were also checked once with xz -t and xz -lvv (the blocks, their dictionaries and
// the index), Python's zipfile, and gzip -t and tar -t on 2026-11-10 (#243).

// TestXZEmptyBlocks reads the stream of xzEmptyBlocks with the xz reader of
// github.com/ulikunitz/xz, which checks the index and the footer too.
func TestXZEmptyBlocks(t *testing.T) {
	data := bytes.Repeat([]byte("control "), 10000) // two LZMA2 chunks
	got, err := io.ReadAll(must2(xz.NewReader(bytes.NewReader(xzEmptyBlocks(3, data)))))
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("read %d bytes, %v; want the %d written", len(got), err, len(data))
	}
}

// TestZipDirectoryBomb reads the directories with zip.NewReader: the end record says as many
// entries, or as many modulo 65 536 with zip64.
func TestZipDirectoryBomb(t *testing.T) {
	for _, c := range []struct {
		entries int
		records uint64
	}{{0, 0}, {3, 3}, {2, 1<<16 + 2}} {
		b := ZipDirectoryBomb(c.entries, c.records)
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil || len(zr.File) != c.entries {
			t.Fatalf("%d entries, %d records: %v", c.entries, c.records, err)
		}
		for i, f := range zr.File {
			if f.Name != strconv.Itoa(i) {
				t.Errorf("entry %d is %q", i, f.Name)
			}
		}
	}
}

// TestGzipOfRepeated unpacks the stream of gzipOfRepeated, which gzip checks with its CRC-32
// and size.
func TestGzipOfRepeated(t *testing.T) {
	parts := []repeated{{[]byte("head"), 1}, {make([]byte, 512), 5000}, {[]byte("ab"), 3}, {nil, 2}, {[]byte("tail"), 1}}
	var want []byte
	for _, p := range parts {
		want = append(want, bytes.Repeat(p.unit, p.count)...)
	}
	got, err := io.ReadAll(must2(gzip.NewReader(bytes.NewReader(gzipOfRepeated(parts...)))))
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("read %d bytes, %v; want %d", len(got), err, len(want))
	}
}

// must2 returns v, panicking on err.
func must2[T any](v T, err error) T {
	must(err)
	return v
}

package testutil

import (
	"bytes"
	"io"
	"testing"

	"github.com/ulikunitz/xz"
)

// The bombs were also checked once with xz -t and xz -lvv (the blocks, their dictionaries and
// the index) on 2026-11-10 (#243).

// TestXZEmptyBlocks reads the stream of xzEmptyBlocks with the xz reader of
// github.com/ulikunitz/xz, which checks the index and the footer too.
func TestXZEmptyBlocks(t *testing.T) {
	data := bytes.Repeat([]byte("control "), 10000) // two LZMA2 chunks
	got, err := io.ReadAll(must2(xz.NewReader(bytes.NewReader(xzEmptyBlocks(3, data)))))
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("read %d bytes, %v; want the %d written", len(got), err, len(data))
	}
}

// must2 returns v, panicking on err.
func must2[T any](v T, err error) T {
	must(err)
	return v
}

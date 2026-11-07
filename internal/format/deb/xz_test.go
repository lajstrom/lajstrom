package deb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

// xzOf compresses data with the xz writer of github.com/ulikunitz/xz and cfg.
func xzOf(t *testing.T, cfg xz.WriterConfig, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := cfg.NewWriter(&b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// xzText is the uncompressed data of the xz tests: long enough for several blocks of 4 KiB.
var xzText = []byte(strings.Repeat("Package: hello\nVersion: 1.0-1\n", 700))

// readXZ decodes b with xzReader.
func readXZ(b []byte) ([]byte, error) {
	r, err := newXZReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// withHeader returns b with the header of the first block replaced by one of the same length
// with flags, the bytes extra after them (sizes), the filter bytes and zero padding, and its
// CRC32 computed.
func withHeader(t *testing.T, b []byte, flags byte, extra, filter []byte) []byte {
	t.Helper()
	n := (int(b[xzStreamHeaderLen]) + 1) * 4
	h := append([]byte{b[xzStreamHeaderLen], flags}, extra...)
	h = append(h, filter...)
	if len(h) > n-4 {
		t.Fatalf("a header of %d bytes does not fit in %d", len(h), n-4)
	}
	h = append(h, make([]byte, n-4-len(h))...)
	h = binary.LittleEndian.AppendUint32(h, crc32.ChecksumIEEE(h))
	out := append([]byte{}, b...)
	copy(out[xzStreamHeaderLen:], h)
	return out
}

// withStreamFlags returns b with the stream flags f and, when fix is set, their CRC32.
func withStreamFlags(b []byte, f [2]byte, fix bool) []byte {
	out := append([]byte{}, b...)
	copy(out[6:8], f[:])
	if fix {
		binary.LittleEndian.PutUint32(out[8:12], crc32.ChecksumIEEE(f[:]))
	}
	return out
}

// TestXZReader decodes what the xz writer writes with every check type and with several blocks.
func TestXZReader(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  xz.WriterConfig
	}{
		{"crc64", xz.WriterConfig{}},
		{"crc32", xz.WriterConfig{CheckSum: xz.CRC32}},
		{"sha256", xz.WriterConfig{CheckSum: xz.SHA256}},
		{"none", xz.WriterConfig{NoCheckSum: true}},
		{"blocks of 4 KiB", xz.WriterConfig{BlockSize: 4 << 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readXZ(xzOf(t, tc.cfg, xzText))
			if err != nil || !bytes.Equal(got, xzText) {
				t.Fatalf("read %d bytes, %v; want the %d bytes written", len(got), err, len(xzText))
			}
		})
	}
}

// TestXZReaderRefuses checks the container rules, and that a block which declares a dictionary
// larger than maxDictionary is refused before anything is allocated for it.
func TestXZReaderRefuses(t *testing.T) {
	good := xzOf(t, xz.WriterConfig{}, xzText)
	// The block header of the writer: size, flags 0, LZMA2 with one byte of properties.
	prop := func(p byte) []byte { return []byte{xzLZMA2, 1, p} }
	if got := withHeader(t, good, 0, nil, prop(good[xzStreamHeaderLen+4])); !bytes.Equal(got, good) {
		t.Fatal("withHeader does not rebuild the writer's block header")
	}
	footer := good[len(good)-12:]
	indexStart := len(good) - 12 - (int(binary.LittleEndian.Uint32(footer[4:8]))+1)*4
	badCheck := append([]byte{}, good...)
	badCheck[indexStart-1] ^= 0xff
	badHeaderCRC := append([]byte{}, good...)
	badHeaderCRC[xzStreamHeaderLen+4] ^= 1
	for _, tc := range []struct {
		name string
		b    []byte
		want string
	}{
		{"empty", nil, "xz: stream header: unexpected EOF"},
		{"magic", append([]byte("not xz"), good[6:]...), "xz: not an xz stream"},
		{"stream header CRC", withStreamFlags(good, [2]byte{0, xzCheckCRC32}, false), "the stream header fails its CRC32"},
		{"unknown check", withStreamFlags(good, [2]byte{0, 0x02}, true), "unsupported stream flags 0x00 0x02"},
		{"reserved stream flags", withStreamFlags(good, [2]byte{1, xzCheckCRC64}, true), "unsupported stream flags 0x01 0x04"},
		{"no block", good[:xzStreamHeaderLen], "xz: block 1: unexpected EOF"},
		{"truncated block header", good[:xzStreamHeaderLen+3], "xz: block 1: header: unexpected EOF"},
		{"block header CRC", badHeaderCRC, "xz: block 1: the header fails its CRC32"},
		{"reserved block flags", withHeader(t, good, 0x04, nil, prop(22)), "reserved block flags 0x04"},
		{"two filters", withHeader(t, good, 0x01, nil, prop(22)), "a chain of 2 filters; only LZMA2 alone is read"},
		{"delta filter", withHeader(t, good, 0, nil, []byte{0x03, 1, 0}), "the filter is not LZMA2 with one byte of properties"},
		{"two property bytes", withHeader(t, good, 0, nil, []byte{xzLZMA2, 2, 22, 0}), "the filter is not LZMA2"},
		{"malformed size", withHeader(t, good, 0x40, []byte{0x80, 0x00}, prop(22)), "a malformed size in the block header"},
		{"padding", withHeader(t, good, 0, nil, []byte{xzLZMA2, 1, 22, 0, 7}), "the block header padding is not zero"},
		{"property 41", withHeader(t, good, 0, nil, prop(41)), "LZMA2 dictionary property 41, at most 40"},
		{"4 GiB dictionary", withHeader(t, good, 0, nil, prop(40)), "declares a dictionary of 4294967295 bytes, at most 67108864"},
		{"96 MiB dictionary", withHeader(t, good, 0, nil, prop(29)), "declares a dictionary of 100663296 bytes, at most 67108864"},
		{"check", badCheck, "xz: block 1 fails its check"},
		{"truncated data", good[:len(good)/2], "unexpected EOF"},
		{"truncated check", good[:indexStart-3], "xz: block 1: padding and check: unexpected EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readXZ(tc.b)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("read %d bytes, %v; want an error containing %q", len(got), err, tc.want)
			}
		})
	}
	// Sizes in the block header are read past and not compared.
	if got, err := readXZ(withHeader(t, good, 0xc0, []byte{0x01, 0x01}, prop(good[xzStreamHeaderLen+4]))); err != nil || !bytes.Equal(got, xzText) {
		t.Errorf("a block header with sizes: %d bytes, %v", len(got), err)
	}
}

// TestXZPadding checks that the block padding must be zero.
func TestXZPadding(t *testing.T) {
	for n := range 4 {
		b := xzOf(t, xz.WriterConfig{}, xzText[:len(xzText)-n])
		footer := b[len(b)-12:]
		index := b[len(b)-12-(int(binary.LittleEndian.Uint32(footer[4:8]))+1)*4:]
		pos := 2 // the indicator and the number of records, one
		unpadded, ok := xzVarint(index, &pos)
		if !ok {
			t.Fatal("no index record")
		}
		padded := (unpadded + 3) &^ 3
		if padded == unpadded {
			continue
		}
		// The first padding byte follows the header and the data, before the 8 bytes of CRC64.
		b[xzStreamHeaderLen+int(unpadded)-8] = 1 //nolint:gosec // the unpadded size of this test's block, a few KiB
		if _, err := readXZ(b); err == nil || !strings.Contains(err.Error(), "xz: block 1: the padding is not zero") {
			t.Fatalf("read = %v, want the padding error", err)
		}
		return
	}
	t.Fatal("no input length gave a block with padding")
}

func TestXZVarint(t *testing.T) {
	for _, tc := range []struct {
		b    []byte
		v    uint64
		ok   bool
		next int
	}{
		{[]byte{0x00}, 0, true, 1},
		{[]byte{0x7f}, 127, true, 1},
		{[]byte{0x80, 0x01}, 128, true, 2},
		{[]byte{0x80, 0x00}, 0, false, 2}, // a superfluous zero byte
		{[]byte{0x80}, 0, false, 1},       // b ends first
		{nil, 0, false, 0},
		{bytes.Repeat([]byte{0xff}, 10), 0, false, 9}, // more than nine bytes
	} {
		pos := 0
		v, ok := xzVarint(tc.b, &pos)
		if ok != tc.ok || (ok && v != tc.v) || pos != tc.next {
			t.Errorf("xzVarint(%x) = %d, %v, pos %d; want %d, %v, pos %d", tc.b, v, ok, pos, tc.v, tc.ok, tc.next)
		}
	}
}

// TestXZStopsAtTheIndex checks that nothing after the index indicator is read: a second stream
// or garbage after the first is not looked at.
func TestXZStopsAtTheIndex(t *testing.T) {
	b := append(xzOf(t, xz.WriterConfig{}, xzText), "garbage after the stream"...)
	if got, err := readXZ(b); err != nil || !bytes.Equal(got, xzText) {
		t.Errorf("read %d bytes, %v", len(got), err)
	}
	r, err := newXZReader(bytes.NewReader(xzOf(t, xz.WriterConfig{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := r.Read(make([]byte, 10)); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("Read of an empty stream = %d, %v; want io.EOF", n, err)
	}
}

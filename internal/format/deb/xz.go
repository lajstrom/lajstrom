package deb

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"

	"github.com/ulikunitz/xz/lzma"
)

// maxDictionary is the largest dictionary, or window, a compressed control member may declare:
// 64 MiB, what xz -9 and zstd --ultra -21 use. A decoder allocates what the stream declares
// before it decodes a byte, so a larger one is refused rather than allocated.
const maxDictionary = 64 << 20

// maxXZBlocks is the most blocks a control.tar.xz may have (#243). The decoder allocates the
// dictionary of each block afresh, so their number bounds what one parse allocates, as
// maxDictionary bounds a block: 16 empty blocks of 64 MiB are a GiB and about 20 ms, 20 000 of
// them were 8 s. dpkg-deb writes one block: xz's encoder makes a stream one block, and its
// multithreaded encoder a block per three dictionaries, at least 1 MiB, so at most four of the
// MaxControl bytes Parse reads.
const maxXZBlocks = 16

// xz container constants (The .xz File Format 1.2.1, §2).
const (
	xzStreamHeaderLen = 12
	xzLZMA2           = 0x21 // the filter ID of LZMA2
	xzCheckNone       = 0x00
	xzCheckCRC32      = 0x01
	xzCheckCRC64      = 0x04
	xzCheckSHA256     = 0x0a
)

var xzMagic = []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}

var crc64Table = crc64.MakeTable(crc64.ECMA)

// xzReader decodes the first stream of an xz file: the stream header, then each block (its
// header, the LZMA2 data, the padding and the check, which it verifies) up to the index, where
// it stops; the index, the footer and any further stream are not read. A control.tar.xz is one
// stream, as xz and dpkg-deb write it. The container is read here and only the LZMA2 data goes
// to github.com/ulikunitz/xz/lzma, because the xz reader of that module allocates whatever
// dictionary a block header declares, up to 4 GiB, before it decodes anything: here a block
// that declares more than maxDictionary is refused first. Only LZMA2 is accepted as the filter
// chain (no BCJ or delta filter in front of it: dpkg-deb uses none), and the sizes a block
// header may declare are not compared with the block.
type xzReader struct {
	r     *bufio.Reader
	check byte
	n     int // blocks started

	block io.Reader      // the LZMA2 decoder of the current block; nil between blocks
	sum   hash.Hash      // the check of the current block, over its uncompressed bytes
	cr    countingReader // the compressed bytes of the current block
	hlen  int            // the header size of the current block
	done  bool           // the index indicator was read
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// newXZReader reads and checks the stream header of r.
func newXZReader(r io.Reader) (*xzReader, error) {
	x := &xzReader{r: bufio.NewReader(r)}
	var h [xzStreamHeaderLen]byte
	if _, err := io.ReadFull(x.r, h[:]); err != nil {
		return nil, fmt.Errorf("xz: stream header: %w", unexpected(err))
	}
	switch {
	case !bytes.Equal(h[:6], xzMagic):
		return nil, errors.New("xz: not an xz stream")
	case binary.LittleEndian.Uint32(h[8:]) != crc32.ChecksumIEEE(h[6:8]):
		return nil, errors.New("xz: the stream header fails its CRC32")
	case h[6] != 0 || (h[7] != xzCheckNone && newCheck(h[7]) == nil):
		return nil, fmt.Errorf("xz: unsupported stream flags %#02x %#02x", h[6], h[7])
	}
	x.check = h[7]
	return x, nil
}

// newCheck returns the hash of check type c, nil for none or an unknown type.
func newCheck(c byte) hash.Hash {
	switch c {
	case xzCheckCRC32:
		return crc32.NewIEEE()
	case xzCheckCRC64:
		return crc64.New(crc64Table)
	case xzCheckSHA256:
		return sha256.New()
	}
	return nil
}

// Read implements io.Reader.
func (x *xzReader) Read(p []byte) (int, error) {
	for {
		if x.done {
			return 0, io.EOF
		}
		if x.block == nil {
			if err := x.startBlock(); err != nil {
				return 0, err
			}
			continue
		}
		n, err := x.block.Read(p)
		if x.sum != nil {
			x.sum.Write(p[:n])
		}
		if errors.Is(err, io.EOF) {
			err = x.endBlock()
		}
		if n > 0 || err != nil {
			return n, err
		}
	}
}

// startBlock reads the next block header and starts the LZMA2 decoder of the block, or sets
// done at the index indicator.
func (x *xzReader) startBlock() error {
	first, err := x.r.ReadByte()
	if err != nil {
		return fmt.Errorf("xz: block %d: %w", x.n+1, unexpected(err))
	}
	if first == 0 { // the index indicator: no more blocks
		x.done = true
		return nil
	}
	if x.n == maxXZBlocks {
		return fmt.Errorf("xz: more than %d blocks", maxXZBlocks)
	}
	x.n++
	h := make([]byte, (int(first)+1)*4)
	h[0] = first
	if _, err := io.ReadFull(x.r, h[1:]); err != nil {
		return fmt.Errorf("xz: block %d: header: %w", x.n, unexpected(err))
	}
	body, sum := h[:len(h)-4], h[len(h)-4:]
	if binary.LittleEndian.Uint32(sum) != crc32.ChecksumIEEE(body) {
		return fmt.Errorf("xz: block %d: the header fails its CRC32", x.n)
	}
	dict, err := lzma2Dictionary(body)
	if err != nil {
		return fmt.Errorf("xz: block %d: %w", x.n, err)
	}
	if dict > maxDictionary {
		return fmt.Errorf("xz: block %d declares a dictionary of %d bytes, at most %d", x.n, dict, maxDictionary)
	}
	x.cr = countingReader{r: x.r}
	block, err := lzma.Reader2Config{DictCap: max(int(dict), lzma.MinDictCap)}.NewReader2(&x.cr)
	if err != nil {
		return fmt.Errorf("xz: block %d: %w", x.n, err)
	}
	x.block, x.sum, x.hlen = block, newCheck(x.check), len(h)
	return nil
}

// endBlock reads the padding and the check that follow the LZMA2 data of a block, and verifies
// the check.
func (x *xzReader) endBlock() error {
	x.block = nil
	want := x.checkBytes()
	pad := int((4 - (int64(x.hlen)+x.cr.n)%4) % 4)
	b := make([]byte, pad+len(want))
	if _, err := io.ReadFull(x.r, b); err != nil {
		return fmt.Errorf("xz: block %d: padding and check: %w", x.n, unexpected(err))
	}
	if !bytes.Equal(b[:pad], make([]byte, pad)) {
		return fmt.Errorf("xz: block %d: the padding is not zero", x.n)
	}
	if !bytes.Equal(b[pad:], want) {
		return fmt.Errorf("xz: block %d fails its check", x.n)
	}
	return nil
}

// checkBytes returns the check of the current block as the container stores it: CRC32 and
// CRC64 little-endian, SHA-256 as it is, nothing for none.
func (x *xzReader) checkBytes() []byte {
	switch s := x.sum.(type) {
	case hash.Hash32:
		return binary.LittleEndian.AppendUint32(nil, s.Sum32())
	case hash.Hash64:
		return binary.LittleEndian.AppendUint64(nil, s.Sum64())
	case hash.Hash:
		return s.Sum(nil)
	}
	return nil
}

// lzma2Dictionary reads a block header without its CRC32 (size, flags, the optional sizes, the
// filter flags, zero padding) and returns the dictionary size of its one filter, LZMA2.
func lzma2Dictionary(h []byte) (uint64, error) {
	flags := h[1]
	switch {
	case flags&0x3c != 0:
		return 0, fmt.Errorf("reserved block flags %#02x", flags)
	case flags&0x03 != 0:
		return 0, fmt.Errorf("a chain of %d filters; only LZMA2 alone is read", flags&0x03+1)
	}
	pos := 2
	for _, bit := range []byte{0x40, 0x80} { // the compressed and the uncompressed size
		if flags&bit != 0 {
			if _, ok := xzVarint(h, &pos); !ok {
				return 0, errors.New("a malformed size in the block header")
			}
		}
	}
	id, idOK := xzVarint(h, &pos)
	size, sizeOK := xzVarint(h, &pos)
	if !idOK || !sizeOK || id != xzLZMA2 || size != 1 || pos >= len(h) {
		return 0, errors.New("the filter is not LZMA2 with one byte of properties")
	}
	prop := h[pos]
	if !bytes.Equal(h[pos+1:], make([]byte, len(h)-pos-1)) {
		return 0, errors.New("the block header padding is not zero")
	}
	switch {
	case prop > 40:
		return 0, fmt.Errorf("LZMA2 dictionary property %d, at most 40", prop)
	case prop == 40:
		return 1<<32 - 1, nil
	}
	return uint64(2|prop&1) << (prop/2 + 11), nil
}

// xzVarint reads the multibyte integer of the xz format at *pos in b and advances *pos: seven
// bits per byte, least significant first, at most nine bytes, no superfluous zero byte at the
// end. ok is false when b ends first or the integer is malformed.
func xzVarint(b []byte, pos *int) (v uint64, ok bool) {
	for i := range 9 {
		if *pos >= len(b) {
			return 0, false
		}
		c := b[*pos]
		*pos++
		v |= uint64(c&0x7f) << (7 * i)
		if c&0x80 == 0 {
			return v, i == 0 || c != 0
		}
	}
	return 0, false
}

// unexpected turns the io.EOF of a structure that has begun into io.ErrUnexpectedEOF.
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

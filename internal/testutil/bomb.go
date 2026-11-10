package testutil

import (
	"encoding/binary"
	"hash/crc32"
	"hash/crc64"
)

// The bombs of #243 (docs/security/threat-model.md, F-4): archives small to send and costly to
// read, for the bounds of the parsers. Building one costs neither: what repeats is written, or
// compressed, once and copied.

// xzEmptyBlocks returns data as an xz stream with CRC64 checks: empty blocks, each a header that
// declares LZMA2 with a 64 MiB dictionary and then only the LZMA2 end marker, then one block of
// data in uncompressed LZMA2 chunks, the index and the footer (The .xz File Format 1.2.1).
func xzEmptyBlocks(empty int, data []byte) []byte {
	le32 := binary.LittleEndian.AppendUint32
	b := append([]byte{0xfd, '7', 'z', 'X', 'Z', 0, 0, 0x04}, le32(nil, crc32.ChecksumIEEE([]byte{0, 0x04}))...)
	header := []byte{2, 0, 0x21, 1, 28, 0, 0, 0} // 12 bytes; one filter, LZMA2, property 28: 64 MiB
	header = le32(header, crc32.ChecksumIEEE(header))
	var records []byte
	var blocks uint64
	block := func(lzma2, data []byte) {
		b = append(append(b, header...), lzma2...)
		b = append(b, make([]byte, (4-len(lzma2)%4)%4)...)
		b = binary.LittleEndian.AppendUint64(b, crc64.Checksum(data, crc64.MakeTable(crc64.ECMA)))
		records = binary.AppendUvarint(records, uint64(len(header)+len(lzma2)+8))
		records = binary.AppendUvarint(records, uint64(len(data)))
		blocks++
	}
	for range empty {
		block([]byte{0}, nil)
	}
	var chunks []byte
	for i := 0; i < len(data); i += 1 << 16 {
		n := min(len(data)-i, 1<<16) - 1
		control := byte(2) // uncompressed
		if i == 0 {
			control = 1 // uncompressed, and the dictionary reset that starts a block
		}
		chunks = append(append(chunks, control, byte(n>>8), byte(n)), data[i:i+n+1]...) //nolint:gosec // n < 1<<16
	}
	block(append(chunks, 0), data)
	index := append(binary.AppendUvarint([]byte{0}, blocks), records...)
	index = append(index, make([]byte, (4-len(index)%4)%4)...)
	b = append(b, le32(index, crc32.ChecksumIEEE(index))...)
	footer := append(le32(nil, uint32(len(index)/4)), 0, 0x04) //nolint:gosec // (index and CRC32)/4 - 1
	return append(append(le32(b, crc32.ChecksumIEEE(footer)), footer...), 'Y', 'Z')
}

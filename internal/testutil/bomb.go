package testutil

import (
	"archive/tar"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"hash/crc32"
	"hash/crc64"
	"strconv"
)

// The bombs of #243 (docs/security/threat-model.md, F-4): archives small to send and costly to
// read, for the bounds of the parsers. Building one costs neither: what repeats is written, or
// compressed, once and copied.

// ZipDirectoryBomb returns a zip archive that is a central directory of entries empty stored
// members, named 0, 1, … (without the local headers that would come before it, which a reader
// of the directory does not look at), and an end record that says records; records of 0xffff
// or more take a zip64 end record and its locator. records other than entries is an end record
// that lies, which archive/zip reads all the same when the two agree modulo 65 536.
func ZipDirectoryBomb(entries int, records uint64) []byte {
	le16, le32, le64 := binary.LittleEndian.AppendUint16, binary.LittleEndian.AppendUint32, binary.LittleEndian.AppendUint64
	var b []byte
	for i := range entries {
		name := strconv.Itoa(i)
		b = le32(b, 0x02014b50)
		b = le16(le16(b, 20), 20)          // made by and needed: 2.0
		b = append(b, make([]byte, 20)...) // flags, method, time, date, crc-32, both sizes
		b = le16(b, uint16(len(name)))     //nolint:gosec // a decimal number, a few bytes
		b = append(b, make([]byte, 16)...) // extra, comment, disk, attributes, offset
		b = append(b, name...)
	}
	dirSize := uint64(len(b))
	if records >= 0xffff {
		end64 := uint64(len(b))
		b = le64(le32(b, 0x06064b50), 44)
		b = le16(le16(b, 45), 45)
		b = le64(le64(le64(le64(le64(b, 0), records), records), dirSize), 0) // disks, records twice, size, offset
		b = le32(le64(le32(le32(b, 0x07064b50), 0), end64), 1)
	}
	b = le32(le32(b, 0x06054b50), 0)
	b = le16(le16(b, uint16(min(records, 0xffff))), uint16(min(records, 0xffff)))
	return le16(le32(le32(b, uint32(min(dirSize, 0xffffffff))), 0), 0) // size, offset, no comment
}

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

// SdistBomb returns the sdist of Sdist(opts) with, between its top directory and PKG-INFO,
// entries empty files and then a file of zeros zero bytes, rounded up to 512 (none when 0):
// small to send, and costly to read up to PKG-INFO, which some build backends write last. A
// GiB of zeros is about a MiB of gzip.
func SdistBomb(opts SdistOptions, entries int, zeros int64) ([]byte, SdistInfo) {
	members, info := sdistMembers(opts)
	parts := []repeated{{tarOf(members[:1])[:512], 1}, {tarHeader(info.Root+"/empty", 0), entries}}
	if blocks := int((zeros + 511) / 512); blocks > 0 {
		parts = append(parts, repeated{tarHeader(info.Root+"/zeros", int64(blocks)*512), 1}, repeated{make([]byte, 512), blocks})
	}
	b := gzipOfRepeated(append(parts, repeated{tarOf(members[1:]), 1})...)
	info.Size = int64(len(b))
	info.Checksums = checksumsOf(b)
	return b, info
}

// tarHeader returns the ustar header of a file of size bytes, as tarOf writes it.
func tarHeader(name string, size int64) []byte {
	var buf bytes.Buffer
	must(tar.NewWriter(&buf).WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: size, ModTime: fixtureTime,
		Uname: "root", Gname: "root", Format: tar.FormatUSTAR}))
	return buf.Bytes()
}

// repeated is count copies of unit, a part of what gzipOfRepeated compresses.
type repeated struct {
	unit  []byte
	count int
}

// gzipOfRepeated returns the parts one after another, compressed with gzip. Each part is
// compressed a MiB at a time by a new writer, flushed to a byte boundary, so the compressed
// bytes of a MiB stand for it wherever they are copied: a GiB costs the time of a MiB.
func gzipOfRepeated(parts ...repeated) []byte {
	out := []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff} // no name, no time
	var sum, size uint32
	for _, p := range parts {
		per := max(1, (1<<20)/max(1, len(p.unit)))
		var chunk, z []byte
		for left := p.count; left > 0; left -= per {
			if k := min(per, left); len(chunk) != k*len(p.unit) {
				chunk = bytes.Repeat(p.unit, k)
				z = deflated(chunk)
			}
			out = append(out, z...)
			sum = crc32.Update(sum, crc32.IEEETable, chunk)
			size += uint32(len(chunk)) //nolint:gosec // gzip keeps the size modulo 2^32
		}
	}
	out = append(out, 0x03, 0x00) // the final block: empty, fixed codes
	return binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(out, sum), size)
}

// deflated returns b compressed by a new flate writer, flushed and not closed: no block is the
// final one, and the last ends on a byte boundary.
func deflated(b []byte) []byte {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	must(err)
	_, err = w.Write(b)
	must(err)
	must(w.Flush())
	return buf.Bytes()
}

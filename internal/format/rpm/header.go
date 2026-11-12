package rpm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"unicode/utf8"

	"github.com/lajstrom/lajstrom/internal/format"
)

// The layout of the head of an rpm (the rpm file format, as rpm 4 writes it): a lead of 96
// bytes, the signature header padded to a multiple of 8 bytes, then the header. A header is an
// intro of 16 bytes (the magic, four reserved bytes, the number of index entries and the size
// of the data store, both big-endian), the index, 16 bytes an entry (tag, type, offset into the
// data store, count), and the data store.
const (
	leadLen  = 96
	introLen = 16
	entryLen = 16
	// maxEntries is the most index entries a header may have, as rpm bounds them.
	maxEntries = 0xffff
	// maxDecoded bounds what the values read out of one header may take in memory: the strings,
	// the numbers and the structures made of them, the joined file paths among them. A header
	// whose tags point into the same bytes, or whose file names share a long directory, would
	// otherwise make far more than its own size; a real one decodes to about its size.
	maxDecoded = 64 << 20
	// sigTypeHeader is the signature type of the lead of every rpm of rpm 3 and later: the
	// signature is a header.
	sigTypeHeader = 5
)

var (
	leadMagic   = []byte{0xed, 0xab, 0xee, 0xdb}
	headerMagic = []byte{0x8e, 0xad, 0xe8, 0x01}
)

// The types of the values of a header.
const (
	typeNull uint32 = iota
	typeChar
	typeInt8
	typeInt16
	typeInt32
	typeInt64
	typeString
	typeBin
	typeStringArray
	typeI18NString
)

// What a string, a Relation, a File and a Changelog take besides the bytes of their strings,
// on a 64-bit machine, for spend.
const (
	stringSize    = 16
	relationSize  = 5*stringSize + 16
	fileSize      = 2 * stringSize
	changelogSize = 2*stringSize + 8
)

// typeSize is the size of one value of the fixed-size types.
var typeSize = map[uint32]uint64{typeChar: 1, typeInt8: 1, typeInt16: 2, typeInt32: 4, typeInt64: 8, typeBin: 1}

// entry is one entry of the index of a header.
type entry struct {
	typ, offset, count uint32
}

// header is one header structure: its index by tag and its data store.
type header struct {
	what    string // "the signature header" or "the header", for the errors
	entries map[int32]entry
	data    []byte
	spent   int64 // what the values read so far take, against maxDecoded
}

// spend counts n bytes against maxDecoded before they are allocated; past it, it fails with an
// error wrapping format.ErrInvalid.
func (h *header) spend(n int64) error {
	h.spent += n
	if h.spent > maxDecoded {
		return fmt.Errorf("%w: %s decodes to more than %d bytes", format.ErrInvalid, h.what, maxDecoded)
	}
	return nil
}

// headerLen returns the size of the header whose intro is b, at least introLen bytes: the
// intro, the index and the data store. An intro without the magic, or with more entries than
// maxEntries, wraps format.ErrInvalid.
func headerLen(what string, b []byte) (int64, error) {
	switch {
	case string(b[:3]) != string(headerMagic[:3]):
		return 0, fmt.Errorf("%w: %s does not start with the header magic", format.ErrInvalid, what)
	case b[3] != headerMagic[3]:
		return 0, fmt.Errorf("%w: %s is of version %d, not 1", format.ErrInvalid, what, b[3])
	}
	il, dl := binary.BigEndian.Uint32(b[8:12]), binary.BigEndian.Uint32(b[12:16])
	if il == 0 || il > maxEntries {
		return 0, fmt.Errorf("%w: %s has %d index entries, 1 to %d", format.ErrInvalid, what, il, maxEntries)
	}
	return introLen + int64(il)*entryLen + int64(dl), nil
}

// parseHeader reads the header structure b, whose length headerLen gave. Every entry must be
// of a known type and lie within the data store, with as many values as its count says; a tag
// may appear once. The values are decoded when they are asked for. Errors wrap
// format.ErrInvalid.
func parseHeader(what string, b []byte) (*header, error) {
	il := int(binary.BigEndian.Uint32(b[8:12]))
	h := &header{what: what, entries: make(map[int32]entry, il), data: b[introLen+il*entryLen:]}
	for i := range il {
		e := b[introLen+i*entryLen:]
		tag := int32(binary.BigEndian.Uint32(e)) //nolint:gosec // a tag is a signed number in rpm
		en := entry{typ: binary.BigEndian.Uint32(e[4:]), offset: binary.BigEndian.Uint32(e[8:]), count: binary.BigEndian.Uint32(e[12:])}
		if _, dup := h.entries[tag]; dup {
			return nil, fmt.Errorf("%w: %s has the tag %d twice", format.ErrInvalid, what, tag)
		}
		if err := h.check(tag, en); err != nil {
			return nil, err
		}
		h.entries[tag] = en
	}
	return h, nil
}

// check checks that the entry of tag fits the data store.
func (h *header) check(tag int32, e entry) error {
	left := uint64(len(h.data)) - uint64(e.offset)
	switch {
	case e.typ > typeI18NString || e.typ == typeNull:
		return fmt.Errorf("%w: %s: tag %d has the type %d", format.ErrInvalid, h.what, tag, e.typ)
	case e.count == 0:
		return fmt.Errorf("%w: %s: tag %d has no value", format.ErrInvalid, h.what, tag)
	case uint64(e.offset) >= uint64(len(h.data)):
		return fmt.Errorf("%w: %s: tag %d starts at %d, past the data store of %d bytes", format.ErrInvalid, h.what, tag, e.offset, len(h.data))
	case e.typ == typeString && e.count != 1:
		return fmt.Errorf("%w: %s: the string of tag %d has a count of %d", format.ErrInvalid, h.what, tag, e.count)
	}
	if size, fixed := typeSize[e.typ]; fixed {
		if uint64(e.count)*size > left {
			return fmt.Errorf("%w: %s: tag %d has %d values past the end of the data store", format.ErrInvalid, h.what, tag, e.count)
		}
		return nil
	}
	// Every string ends with its NUL inside the data store.
	if uint64(e.count) > left {
		return fmt.Errorf("%w: %s: tag %d has %d strings in %d bytes", format.ErrInvalid, h.what, tag, e.count, left)
	}
	at := int(e.offset)
	for n := range e.count {
		i := bytes.IndexByte(h.data[at:], 0)
		if i < 0 {
			return fmt.Errorf("%w: %s: string %d of tag %d has no end in the data store", format.ErrInvalid, h.what, n+1, tag)
		}
		at += i + 1
	}
	return nil
}

// has reports whether the header has tag.
func (h *header) has(tag int32) bool {
	_, ok := h.entries[tag]
	return ok
}

// strings returns the strings of tag: of a STRING_ARRAY or an I18NSTRING every one, of a
// STRING the one; nil without the tag. A string that is not UTF-8 is read as Latin-1, as old
// packages wrote it. Another type wraps format.ErrInvalid.
func (h *header) strings(tag int32) ([]string, error) {
	e, ok := h.entries[tag]
	if !ok {
		return nil, nil
	}
	if e.typ != typeString && e.typ != typeStringArray && e.typ != typeI18NString {
		return nil, fmt.Errorf("%w: %s: tag %d is of type %d, not a string", format.ErrInvalid, h.what, tag, e.typ)
	}
	return h.stringsOf(e, int(e.count))
}

// stringsOf returns the first n strings of e, which check made sure end in the data store.
func (h *header) stringsOf(e entry, n int) ([]string, error) {
	end := int(e.offset)
	for range n {
		end += bytes.IndexByte(h.data[end:], 0) + 1
	}
	if err := h.spend(int64(n)*stringSize + int64(end-int(e.offset))*2); err != nil { // a Latin-1 byte may take two
		return nil, err
	}
	out := make([]string, 0, n)
	for at := int(e.offset); at < end; {
		k := bytes.IndexByte(h.data[at:], 0)
		out = append(out, text(h.data[at:at+k]))
		at += k + 1
	}
	return out, nil
}

// str returns the string of tag, the first of an array or an I18NSTRING (the C locale's): ok is
// false without the tag.
func (h *header) str(tag int32) (s string, ok bool, err error) {
	e, ok := h.entries[tag]
	if !ok {
		return "", false, nil
	}
	if e.typ != typeString && e.typ != typeStringArray && e.typ != typeI18NString {
		return "", false, fmt.Errorf("%w: %s: tag %d is of type %d, not a string", format.ErrInvalid, h.what, tag, e.typ)
	}
	all, err := h.stringsOf(e, 1)
	if err != nil {
		return "", false, err
	}
	return all[0], true, nil
}

// ints returns the numbers of tag, of any integer type, unsigned; nil without the tag. Another
// type wraps format.ErrInvalid, and so does an INT64 above 2^63-1.
func (h *header) ints(tag int32) ([]int64, error) {
	e, ok := h.entries[tag]
	if !ok {
		return nil, nil
	}
	size := typeSize[e.typ]
	if e.typ < typeChar || e.typ > typeInt64 {
		return nil, fmt.Errorf("%w: %s: tag %d is of type %d, not a number", format.ErrInvalid, h.what, tag, e.typ)
	}
	if err := h.spend(int64(e.count) * 8); err != nil {
		return nil, err
	}
	out := make([]int64, e.count)
	b := h.data[e.offset:]
	for i := range out {
		v := b[uint64(i)*size:]
		switch size {
		case 1:
			out[i] = int64(v[0])
		case 2:
			out[i] = int64(binary.BigEndian.Uint16(v))
		case 4:
			out[i] = int64(binary.BigEndian.Uint32(v))
		default:
			n := binary.BigEndian.Uint64(v)
			if n > 1<<63-1 {
				return nil, fmt.Errorf("%w: %s: tag %d has the number %d", format.ErrInvalid, h.what, tag, n)
			}
			out[i] = int64(n)
		}
	}
	return out, nil
}

// num returns the first number of tag; ok is false without the tag.
func (h *header) num(tag int32) (n int64, ok bool, err error) {
	all, err := h.ints(tag)
	if err != nil || len(all) == 0 {
		return 0, false, err
	}
	return all[0], true, nil
}

// text returns b as a string: as it is when it is UTF-8, else each byte as the Latin-1
// character of that code, so that every string of a package goes into JSON and XML without a
// byte lost to a replacement character.
func text(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

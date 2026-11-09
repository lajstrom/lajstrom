package index

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// NewUID returns a new uid, the identity of a user (docs/spec/rbac.md §1): a UUID version 7
// (RFC 9562 §5.7) in its canonical text form, lowercase. Its first 48 bits are the milliseconds of
// at since the Unix epoch, so that uids sort by the time they were made; then the version, 74
// bits from crypto/rand and the variant.
func NewUID(at time.Time) string {
	var b [16]byte
	var ms [8]byte
	_, _ = rand.Read(b[6:]) // crypto/rand.Read never returns an error; it crashes the program instead
	binary.BigEndian.PutUint64(ms[:], uint64(at.UnixMilli()))
	copy(b[:6], ms[2:])     // the low 48 bits of the milliseconds
	b[6] = b[6]&0x0f | 0x70 // version 7
	b[8] = b[8]&0x3f | 0x80 // variant 10
	return format(b)
}

// format writes the 16 bytes of a UUID as 8-4-4-4-12 lowercase hex digits.
func format(b [16]byte) string {
	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:36], b[10:16])
	return string(s[:])
}

// IsUID reports whether s has the form of a uid: a UUID in its canonical text form, 8-4-4-4-12
// lowercase hex digits, of any version. A user name of that form is refused, so that user:<s> is
// a uid exactly when IsUID(s).
func IsUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

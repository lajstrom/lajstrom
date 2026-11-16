package rpm

import (
	"cmp"
	"strings"
)

// rpm's version order, rpmvercmp and the comparison of [epoch:]version[-release] that rpm
// builds on it, as rpm's documentation describes them (rpm.org, "RPM version comparison"),
// written from that text:
//
//   - two version strings are split into segments, each the longest run of ASCII digits or of
//     ASCII letters; every other character only separates them, but "~" and "^";
//   - from the left: a "~" comes before everything, the end of the string included (1.0~rc1 <
//     1.0); a "^" comes after the end of the string and before everything else (1.0 < 1.0^git1 <
//     1.0.1); then a segment of digits comes after one of letters, two of digits compare as
//     integers and two of letters by their bytes; when every segment compared equal, the string
//     with segments left comes after the other;
//   - two versions [epoch:]version[-release] compare by epoch, 0 when there is none, then by
//     version, then by release when both have one: 2.0 is equal to 2.0-1 and to 2.0-7.
//
// 1.0~rc1 < 1.0 < 1.0^git1 < 1.0a < 1.0.1 < 1.0.10 < 1:0.1; 2.0-1 < 2.0-1.el9.

// evr is a version split into its epoch, version and release.
type evr struct {
	epoch, version, release string
	hasRelease              bool
}

// splitVersion splits a version as rpm reads it: the epoch is what comes before the first ":"
// when it is all digits, "0" otherwise; the release what follows the last "-" when there is one.
func splitVersion(s string) evr {
	v := evr{epoch: "0"}
	if e, rest, ok := strings.Cut(s, ":"); ok && strings.Trim(e, "0123456789") == "" {
		if e != "" {
			v.epoch = e
		}
		s = rest
	}
	v.version = s
	if i := strings.LastIndexByte(s, '-'); i >= 0 {
		v.version, v.release, v.hasRelease = s[:i], s[i+1:], true
	}
	return v
}

// compareVersions compares two versions [epoch:]version[-release] in rpm's order: <0 when a
// comes before b, 0 when they are equal in it (1.0-1 and 0:1.0-1, 1.0 and 1.0-1), >0 when a
// comes after b. Every string is a version in that order.
func compareVersions(a, b string) int {
	x, y := splitVersion(a), splitVersion(b)
	if c := vercmp(x.epoch, y.epoch); c != 0 {
		return c
	}
	if c := vercmp(x.version, y.version); c != 0 || !x.hasRelease || !y.hasRelease {
		return c
	}
	return vercmp(x.release, y.release)
}

// vercmp compares two version strings, or two releases, segment by segment (rpmvercmp).
func vercmp(a, b string) int {
	if a == b {
		return 0
	}
	for a != "" || b != "" {
		a, b = skipSeparators(a), skipSeparators(b)
		if c, decided, skip := compareMarks(a, b); decided {
			return c
		} else if skip {
			a, b = a[1:], b[1:]
			continue
		}
		if a == "" || b == "" {
			return cmp.Compare(len(a), len(b))
		}
		c, i, j := compareSegments(a, b)
		if c != 0 {
			return c
		}
		a, b = a[i:], b[j:]
	}
	return 0
}

// compareMarks compares a and b when either starts with "~" or "^": decided when that orders
// them, as c; skip when both start with the same one, which the comparison passes over.
func compareMarks(a, b string) (c int, decided, skip bool) {
	switch ta, tb := strings.HasPrefix(a, "~"), strings.HasPrefix(b, "~"); {
	case ta && tb:
		return 0, false, true
	case ta: // "~" comes before everything
		return -1, true, false
	case tb:
		return 1, true, false
	}
	switch ca, cb := strings.HasPrefix(a, "^"), strings.HasPrefix(b, "^"); {
	case ca && cb:
		return 0, false, true
	case ca: // "^" comes after the end and before a segment
		if b == "" {
			return 1, true, false
		}
		return -1, true, false
	case cb:
		if a == "" {
			return -1, true, false
		}
		return 1, true, false
	}
	return 0, false, false
}

// compareSegments compares the segments a and b start with, of the kind of a's: digits come
// after letters, digits compare as integers and letters by their bytes. i and j are the lengths
// of the two segments.
func compareSegments(a, b string) (c, i, j int) {
	digits := isDigit(a[0])
	i, j = segment(a, digits), segment(b, digits)
	switch {
	case j == 0: // b's segment is of the other kind
		if digits {
			return 1, i, j
		}
		return -1, i, j
	case digits:
		x, y := strings.TrimLeft(a[:i], "0"), strings.TrimLeft(b[:j], "0")
		return cmp.Or(cmp.Compare(len(x), len(y)), strings.Compare(x, y)), i, j
	}
	return strings.Compare(a[:i], b[:j]), i, j
}

// segment is the length of the run of digits (or of letters) that s starts with.
func segment(s string, digits bool) int {
	i := 0
	for i < len(s) && (digits && isDigit(s[i]) || !digits && isLetter(s[i])) {
		i++
	}
	return i
}

// skipSeparators returns s without the characters it starts with that only separate segments:
// any byte but an ASCII letter or digit, "~" and "^".
func skipSeparators(s string) string {
	i := 0
	for i < len(s) && s[i] != '~' && s[i] != '^' && !isDigit(s[i]) && !isLetter(s[i]) {
		i++
	}
	return s[i:]
}

// isDigit reports whether c is an ASCII digit.
func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// isLetter reports whether c is an ASCII letter.
func isLetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

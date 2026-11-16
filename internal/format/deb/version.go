package deb

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
)

// dpkg's version order, as Debian Policy §5.6.12 specifies it, written from that text:
//
//   - a version is [epoch:]upstream_version[-debian_revision]: the epoch is what comes before
//     the first ":", 0 when there is none; the revision what follows the last "-", "" when there
//     is none, which compares as 0;
//   - two versions compare by epoch, as integers, then by upstream version, then by revision,
//     each of these two strings from left to right: first the longest part of non-digits of
//     each (one may be empty), compared character by character, a letter before any non-letter,
//     letters and the others by their ASCII codes, and "~" before everything, even the end of
//     the part; then the longest part of digits of each, as integers, an empty part 0; and so
//     on until one differs or both end.
//
// 1.0~rc1 < 1.0 = 1.0-0 = 0:1.0 < 1.0-1 < 1.0+b1 < 1.0.1 < 1:0.9; 1.09 = 1.9.

// debVersion is a version split into its parts.
type debVersion struct {
	epoch    string // its digits without leading zeros, "0" for zero and for none
	upstream string
	revision string
}

// parseVersion splits a version into its parts and checks them as dpkg does (lib/dpkg's
// parseversion): spaces around it are ignored, and it is refused when it is empty or has a space
// inside, when its epoch is not digits or above 2147483647, its upstream version is empty or does
// not start with a digit, or a "-" ends it. The upstream version has letters, digits and ".",
// "+", "~", "-" and ":"; the revision letters, digits and ".", "+" and "~". An error wraps
// format.ErrInvalid.
func parseVersion(s string) (debVersion, error) {
	invalid := func(why string) (debVersion, error) {
		return debVersion{}, fmt.Errorf("%w: %q is not a Debian version: %s", format.ErrInvalid, s, why)
	}
	t := strings.Trim(s, " \t\n\r\v\f")
	if t == "" {
		return invalid("it is empty")
	}
	if strings.ContainsAny(t, " \t\n\r\v\f") {
		return invalid("it has a space inside")
	}
	v := debVersion{epoch: "0"}
	if e, rest, ok := strings.Cut(t, ":"); ok {
		switch n, err := strconv.ParseInt(e, 10, 32); {
		case e == "":
			return invalid("its epoch is empty")
		case strings.Trim(e, "0123456789") != "":
			return invalid("its epoch is not a number")
		case err != nil:
			return invalid("its epoch is too big")
		default:
			v.epoch = strconv.FormatInt(n, 10)
		}
		t = rest
	}
	v.upstream = t
	if i := strings.LastIndexByte(t, '-'); i >= 0 {
		v.upstream, v.revision = t[:i], t[i+1:]
		if v.revision == "" {
			return invalid("its revision is empty")
		}
	}
	switch {
	case v.upstream == "":
		return invalid("its upstream version is empty")
	case !isDigit(v.upstream[0]):
		return invalid("its upstream version does not start with a digit")
	case strings.IndexFunc(v.upstream, func(r rune) bool { return !isAlnum(r) && !strings.ContainsRune(".+~-:", r) }) >= 0:
		return invalid("its upstream version has a character other than letters, digits, \".\", \"+\", \"~\", \"-\" and \":\"")
	case strings.IndexFunc(v.revision, func(r rune) bool { return !isAlnum(r) && !strings.ContainsRune(".+~", r) }) >= 0:
		return invalid("its revision has a character other than letters, digits, \".\", \"+\" and \"~\"")
	}
	return v, nil
}

// compareVersions compares two versions in dpkg's order: <0 when a comes before b, 0 when they
// are equal in it (1.0 and 0:1.0-0), >0 when a comes after b. A version that parseVersion
// refuses is an error that wraps format.ErrInvalid.
func compareVersions(a, b string) (int, error) {
	va, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	vb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	return va.compare(vb), nil
}

// compare compares v with w: by epoch, upstream version and revision.
func (v debVersion) compare(w debVersion) int {
	return cmp.Or(compareDigits(v.epoch, w.epoch), compareParts(v.upstream, w.upstream), compareParts(v.revision, w.revision))
}

// compareParts compares two upstream versions or two revisions: a part of non-digits, then a
// part of digits, in turn, until one differs or both strings end.
func compareParts(x, y string) int {
	for x != "" || y != "" {
		i, j := span(x, false), span(y, false)
		if c := compareLexical(x[:i], y[:j]); c != 0 {
			return c
		}
		x, y = x[i:], y[j:]
		i, j = span(x, true), span(y, true)
		if c := compareDigits(strings.TrimLeft(x[:i], "0"), strings.TrimLeft(y[:j], "0")); c != 0 {
			return c
		}
		x, y = x[i:], y[j:]
	}
	return 0
}

// compareLexical compares two parts of non-digits character by character, in lexicalRank's
// order; the end of a part ranks as 0.
func compareLexical(x, y string) int {
	for i := range max(len(x), len(y)) {
		var p, q int
		if i < len(x) {
			p = lexicalRank(x[i])
		}
		if i < len(y) {
			q = lexicalRank(y[i])
		}
		if c := cmp.Compare(p, q); c != 0 {
			return c
		}
	}
	return 0
}

// lexicalRank is the rank of a non-digit: "~" before the end of a part (0), then the letters,
// then the other characters, letters and others each in ASCII order.
func lexicalRank(c byte) int {
	switch {
	case c == '~':
		return -1
	case 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
		return int(c)
	default:
		return int(c) + 256
	}
}

// compareDigits compares two numbers written without leading zeros, an empty one 0: the
// longer is the larger.
func compareDigits(x, y string) int {
	return cmp.Or(cmp.Compare(len(x), len(y)), strings.Compare(x, y))
}

// span is the length of the longest start of s that is digits when digits is true, non-digits
// otherwise.
func span(s string, digits bool) int {
	i := 0
	for i < len(s) && isDigit(s[i]) == digits {
		i++
	}
	return i
}

// isDigit reports whether c is an ASCII digit.
func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// isAlnum reports whether r is an ASCII letter or digit.
func isAlnum(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9'
}

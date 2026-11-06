package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

const (
	hexDigits = "0123456789abcdef"

	// maxTagLen is the longest OCI tag: [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}.
	maxTagLen = 128
	// tagSeparator separates the escaped version from the escaped variant (§4.2).
	tagSeparator = "__"
	// hashedTagPrefix starts a hashed tag; Escape never produces it because "h" is not a hex digit.
	hashedTagPrefix = "_h"
	// hashedTagHexLen is the number of hex digits of sha256 in a hashed tag.
	hashedTagHexLen = 40
	// hashedComponentPrefix starts a hashed repository path component (§4.3); Escape never
	// produces "__", so no escaped value starts with it.
	hashedComponentPrefix = "h0__"
	// hashedComponentHexLen is the number of hex digits of sha256 in a hashed component.
	hashedComponentHexLen = 32
)

// componentRE is the OCI distribution grammar of one repository path component.
var componentRE = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$`)

// isSafe reports whether Escape copies the byte c (apart from a leading '.' or '-').
func isSafe(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '.' || c == '-'
}

// needsEscape reports whether Escape encodes the byte c at position i.
func needsEscape(c byte, i int) bool {
	return !isSafe(c) || i == 0 && (c == '.' || c == '-')
}

// Escape encodes s for use in an OCI tag (docs/spec/manifest-profile.md §4.3). It works on the
// bytes of s: A-Z a-z 0-9 . - are copied; every other byte, '_' included, becomes '_' and two
// lowercase hex digits; a leading '.' or '-' is encoded as well. The output matches
// ^[A-Za-z0-9_][A-Za-z0-9._-]*$ (it is empty only for empty s), never contains "__" and never
// starts with "_h". It is not length-limited; Tag applies the 128-character rule.
func Escape(s string) string {
	n := 0
	for i := range len(s) {
		if needsEscape(s[i], i) {
			n++
		}
	}
	if n == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 2*n)
	for i := range len(s) {
		c := s[i]
		if !needsEscape(c, i) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('_')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// Unescape is the inverse of Escape, for tooling and tests: no server code path decodes a tag
// or a repository path, reindex reads the lajstrom.* annotations. It accepts exactly the
// strings Escape produces, so Escape(Unescape(t)) == t whenever it succeeds. Errors wrap
// ErrInvalidEscape: a '_' not followed by two lowercase hex digits, a hashed tag ("_h…"), a
// byte Escape never writes, a leading '.' or '-', or an escape of a byte Escape copies. A hashed
// path component ("h0__…", see EscapeComponent) contains "__", which Escape never produces, and
// fails like any other string Escape cannot produce.
func Unescape(s string) (string, error) {
	if strings.HasPrefix(s, hashedTagPrefix) {
		return "", fmt.Errorf("%w: %q is a hashed tag", ErrInvalidEscape, s)
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		c, n, err := unescapeAt(s, i)
		if err != nil {
			return "", err
		}
		out = append(out, c)
		i += n
	}
	return string(out), nil
}

// unescapeAt decodes the byte that starts at s[i] and returns it with the number of input bytes
// it took.
func unescapeAt(s string, i int) (byte, int, error) {
	c := s[i]
	if c != '_' {
		if needsEscape(c, i) {
			return 0, 0, fmt.Errorf("%w: %q: byte %q at offset %d must be escaped", ErrInvalidEscape, s, c, i)
		}
		return c, 1, nil
	}
	if i+2 >= len(s) {
		return 0, 0, fmt.Errorf("%w: %q: truncated escape at offset %d", ErrInvalidEscape, s, i)
	}
	hi, ok1 := unhex(s[i+1])
	lo, ok2 := unhex(s[i+2])
	if !ok1 || !ok2 {
		return 0, 0, fmt.Errorf("%w: %q: %q at offset %d is not '_' and two lowercase hex digits",
			ErrInvalidEscape, s, s[i:i+3], i)
	}
	c = hi<<4 | lo
	if !needsEscape(c, i) {
		return 0, 0, fmt.Errorf("%w: %q: %q at offset %d escapes %q, which Escape copies",
			ErrInvalidEscape, s, s[i:i+3], i, c)
	}
	return c, 3, nil
}

// unhex decodes one lowercase hex digit.
func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

// EscapeComponent encodes s as one OCI repository path component (§4.3): ASCII letters are
// lower-cased (the original spelling stays in the annotations), the result is escaped with
// Escape, and when that does not match the path-component grammar
// ^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$ (empty, a leading '_', ".-", a trailing '.', …) it is
// replaced by "h0__" and the first 32 hex digits of sha256(s); Escape never produces "__", so
// a hashed component cannot collide with an escaped one. The output always matches the
// grammar. It is not injective: names that differ only in case share a component.
func EscapeComponent(s string) string {
	if e := Escape(lowerASCII(s)); componentRE.MatchString(e) {
		return e
	}
	sum := sha256.Sum256([]byte(s))
	return hashedComponentPrefix + hex.EncodeToString(sum[:hashedComponentHexLen/2])
}

// lowerASCII lower-cases A-Z and leaves every other byte alone.
func lowerASCII(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return 'A' <= r && r <= 'Z' }) < 0 {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// Tag returns the OCI tag of a (version, variant) pair (§4.2): Escape(version), followed by
// "__" and Escape(variant) when variant is not empty. A tag longer than 128 characters is
// replaced by "_h" and the first 40 hex digits of the sha256 of that unhashed tag string, which
// is injective in (version, variant) because Escape is prefix-free and never produces "__".
// Tags are keys: nothing decodes them. Tag does not check that version is non-empty.
func Tag(version, variant string) string {
	t := Escape(version)
	if variant != "" {
		t += tagSeparator + Escape(variant)
	}
	if len(t) <= maxTagLen {
		return t
	}
	sum := sha256.Sum256([]byte(t))
	return hashedTagPrefix + hex.EncodeToString(sum[:hashedTagHexLen/2])
}

// IsHashedTag reports whether tag is a hashed tag as Tag produces it: "_h" and 40 lowercase hex
// digits.
func IsHashedTag(tag string) bool {
	rest, ok := strings.CutPrefix(tag, hashedTagPrefix)
	if !ok || len(rest) != hashedTagHexLen {
		return false
	}
	for i := range len(rest) {
		if _, ok := unhex(rest[i]); !ok {
			return false
		}
	}
	return true
}

// RepositoryPath returns the OCI repository of a package (§4.1): prefix + format + "/" + repo +
// "/" + packagePath. prefix is the instance prefix of the backend registry and ends with "/"
// unless it is empty; format is the protocol format (apt, not deb); packagePath is the output
// of the plugin's EscapeName. Nothing is added, stripped or checked.
func RepositoryPath(prefix, format, repo, packagePath string) string {
	return prefix + format + "/" + repo + "/" + packagePath
}

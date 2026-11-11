package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
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
	// hashedComponentPrefix starts a hashed repository path component (§4.3, ADR 0017): Escape
	// writes '_' only before two hex digits, and 'h' is none, so no escaped value contains "_h".
	hashedComponentPrefix = "h0_h"
	// legacyHashedComponentPrefix started a hashed component before ADR 0017; reads accept it.
	legacyHashedComponentPrefix = "h0__"
	// hashedComponentHexLen is the number of hex digits of sha256 in a hashed component.
	hashedComponentHexLen = 32
)

// specComponentRE is the path-component grammar of the OCI distribution specification 1.1.
// EscapeComponent hashes a name whose escaped form does not match it, as it did before ADR 0017,
// so that a name is hashed in both forms or in neither.
var specComponentRE = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$`)

// reservedSuffixes end the path components that Zot's catalog and garbage collector skip, by
// strings.HasSuffix on the path; reservedComponents are the files of Zot's image layout, which
// a repository cannot share with a child repository of that name (ADR 0017); harborComponents
// are the route words that Harbor finds anywhere in a URL (its amendment).
var (
	reservedSuffixes   = []string{"blobs", ".uploads", ".sync"}
	reservedComponents = []string{"index.json", "oci-layout"}
	harborComponents   = []string{"manifests", "referrers"}
)

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
		writeEscape(&b, c)
	}
	return b.String()
}

// writeEscape writes c as '_' and two lowercase hex digits.
func writeEscape(b *strings.Builder, c byte) {
	b.WriteByte('_')
	b.WriteByte(hexDigits[c>>4])
	b.WriteByte(hexDigits[c&0x0f])
}

// Unescape is the inverse of Escape, for tooling and tests: no server code path decodes a tag
// or a repository path, reindex reads the lajstrom.* annotations. It accepts exactly the
// strings Escape produces, so Escape(Unescape(t)) == t whenever it succeeds. Errors wrap
// ErrInvalidEscape: a '_' not followed by two lowercase hex digits, a hashed tag ("_h…"), a
// byte Escape never writes, a leading '.' or '-', or an escape of a byte Escape copies. A hashed
// path component ("h0_h…", or "h0__…" before ADR 0017) fails like any other string Escape cannot
// produce; UnescapeComponent decodes path components.
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

// EscapeComponent encodes s as one OCI repository path component (§4.3, ADR 0017): ASCII
// letters are lower-cased (the original spelling stays in the annotations) and the result is
// escaped with Escape. When that does not match the path-component grammar of the distribution
// specification 1.1, ^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$ (empty, a leading '_', ".-", a
// trailing '.', …), it is replaced by "h0_h" and the first 32 hex digits of sha256(s), which no
// escaped value contains. Otherwise each '-' of a run of two or more is escaped, and so is the
// last byte of a component that ReservedComponent names: "a--b" is "a_2d_2db", "blobs" is
// "blob_73". The output matches the 1.0 grammar ^[a-z0-9]+(?:[._-][a-z0-9]+)*$, is no
// ReservedComponent, and is a name the components Escape writes cannot be, since Escape never
// escapes a byte it copies. It is not injective: names that differ only in case share a
// component. Before ADR 0017 a component was written without those escapes and hashed with the
// prefix "h0__", and before its amendment "manifests" and "referrers" as they are;
// EarlierPackagePaths and UnescapeComponent read those forms too. PackagePath joins components.
func EscapeComponent(s string) string { return EscapeComponentMax(s, 0) }

// EscapeComponentMax is EscapeComponent, and hashes s as well when the Escape of its lower-cased
// form is longer than maxLen bytes, if maxLen is positive: the 64-byte limit of a raw path's
// components (§4.1). The escapes of the reserved words and the dash runs come after the limit,
// so a component it keeps may be longer by those.
func EscapeComponentMax(s string, maxLen int) string {
	e := Escape(lowerASCII(s))
	if !specComponentRE.MatchString(e) || maxLen > 0 && len(e) > maxLen {
		sum := sha256.Sum256([]byte(s))
		return hashedComponentPrefix + hex.EncodeToString(sum[:hashedComponentHexLen/2])
	}
	return markComponent(e, true)
}

// ReservedComponent reports whether the path component c is a name that a registry keeps for
// itself (ADR 0017): it ends in "blobs", ".uploads" or ".sync", which Zot's catalog and garbage
// collector skip, or it is "index.json" or "oci-layout", files of Zot's image layout, or
// "manifests" or "referrers", which Harbor's routes find anywhere in a URL (its amendment).
// EscapeComponent never writes one.
func ReservedComponent(c string) bool { return reserved(c, true) }

// reserved is ReservedComponent, without Harbor's words unless harbor: as ADR 0017 had them before
// its amendment.
func reserved(c string, harbor bool) bool {
	for _, s := range reservedSuffixes {
		if strings.HasSuffix(c, s) {
			return true
		}
	}
	return slices.Contains(reservedComponents, c) || harbor && slices.Contains(harborComponents, c)
}

// markComponent returns e, an escaped component of the 1.1 grammar, with the escapes of ADR 0017:
// each '-' of a run of two or more, and the last byte when e is a ReservedComponent, of Harbor's
// words only with harbor (before the amendment, without). Neither changes the other: a run of
// dashes lies between two letters or digits.
func markComponent(e string, harbor bool) string {
	last := reserved(e, harbor)
	if !last && !strings.Contains(e, "--") {
		return e
	}
	var b strings.Builder
	for i := range len(e) {
		c := e[i]
		switch {
		case c == '-' && (i > 0 && e[i-1] == '-' || i+1 < len(e) && e[i+1] == '-'),
			last && i == len(e)-1:
			writeEscape(&b, c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// IsHashedComponent reports whether c is a hashed path component: "h0_h", or "h0__" as before
// ADR 0017, and 32 lowercase hex digits.
func IsHashedComponent(c string) bool {
	_, ok := hashedComponentHex(c)
	return ok
}

// hashedComponentHex returns the hex digits of a hashed path component of either form.
func hashedComponentHex(c string) (string, bool) {
	rest, ok := strings.CutPrefix(c, hashedComponentPrefix)
	if !ok {
		rest, ok = strings.CutPrefix(c, legacyHashedComponentPrefix)
	}
	if !ok || len(rest) != hashedComponentHexLen {
		return "", false
	}
	for i := range len(rest) {
		if _, ok := unhex(rest[i]); !ok {
			return "", false
		}
	}
	return rest, true
}

// legacyComponent is the component written before ADR 0017 when c is a component that
// EscapeComponent writes now (harbor) or wrote before the amendment of ADR 0017 (not harbor), else
// c. Harbor's words were written as they are before the amendment, so harbor reads both.
func legacyComponent(c string, harbor bool) string {
	if h, ok := hashedComponentHex(c); ok {
		return legacyHashedComponentPrefix + h
	}
	if u := unmark(c); u != c && specComponentRE.MatchString(u) && markComponent(u, harbor) == c {
		return u
	}
	return c
}

// unmark decodes each escape in c, after the first byte, of a byte that Escape copies: the
// escapes that markComponent writes, and only those when c is its output.
func unmark(c string) string {
	if !strings.Contains(c, "_") {
		return c
	}
	b := make([]byte, 0, len(c))
	for i := 0; i < len(c); i++ {
		if c[i] == '_' && i > 0 && i+2 < len(c) {
			hi, ok1 := unhex(c[i+1])
			lo, ok2 := unhex(c[i+2])
			if x := hi<<4 | lo; ok1 && ok2 && isSafe(x) {
				b = append(b, x)
				i += 2
				continue
			}
		}
		b = append(b, c[i])
	}
	return string(b)
}

// UnescapeComponent is the inverse of EscapeComponent for tooling and tests, in every form: the
// lower-cased name of a component that EscapeComponent writes, or wrote before ADR 0017 or its
// amendment, so that a name decodes the same in any form. Errors wrap ErrInvalidEscape: a hashed
// component (IsHashedComponent), whose name only the annotations hold, or a string that is no
// form of any name. The mark of "tags" depends on the next component: UnescapePackagePath
// decodes it.
func UnescapeComponent(c string) (string, error) {
	if IsHashedComponent(c) {
		return "", fmt.Errorf("%w: %q is a hashed path component", ErrInvalidEscape, c)
	}
	u, err := Unescape(legacyComponent(c, true))
	if err != nil {
		return "", err
	}
	if e := EscapeComponent(u); e != c && legacyComponent(e, true) != c {
		return "", fmt.Errorf("%w: %q is not a path component of %q", ErrInvalidEscape, c, u)
	}
	return u, nil
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

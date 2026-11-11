package npm

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// maxNameLen is the longest name npm takes for a new package, in bytes.
const maxNameLen = 214

// maxVersionLen is the longest version npm's semver parses.
const maxVersionLen = 256

// maxSafeInteger is the largest major, minor and patch npm's semver takes, JavaScript's
// Number.MAX_SAFE_INTEGER.
const maxSafeInteger = 1<<53 - 1

// splitName returns the scope of a name, without its @, and the name without the scope:
// "acme", "widgets" for @acme/widgets, "", "lodash" for lodash. A name that begins with @ and
// has not one / between two non-empty parts, or an unscoped name with a /, wraps
// format.ErrInvalid.
func splitName(name string) (scope, base string, err error) {
	rest, scoped := strings.CutPrefix(name, "@")
	if !scoped {
		if strings.Contains(name, "/") {
			return "", "", fmt.Errorf("%w: %q: a name with a / is a scoped name, @scope/name", format.ErrInvalid, name)
		}
		return "", name, nil
	}
	scope, base, ok := strings.Cut(rest, "/")
	if !ok || scope == "" || base == "" || strings.Contains(base, "/") {
		return "", "", fmt.Errorf("%w: %q is not a scoped name, @scope/name", format.ErrInvalid, name)
	}
	return scope, base, nil
}

// uriSafe reports whether encodeURIComponent leaves the byte as it is.
func uriSafe(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0
}

// CheckName returns nil when npm takes name for a package that exists already, as the names of
// npmjs.org's oldest packages are: not empty; not beginning with . or _; the scope and the name
// without the scope each made of the characters that encodeURIComponent does not change, and
// neither . nor ..; not node_modules or favicon.ico; at most 214 bytes. Upper-case letters
// are allowed (JSONStream). Errors wrap format.ErrInvalid and name the rule.
func CheckName(name string) error {
	scope, base, err := splitName(name)
	switch {
	case err != nil:
		return err
	case name == "":
		return fmt.Errorf("%w: the package name is empty", format.ErrInvalid)
	case len(name) > maxNameLen:
		return fmt.Errorf("%w: %q is %d bytes, at most %d", format.ErrInvalid, name, len(name), maxNameLen)
	case name[0] == '.' || name[0] == '_':
		return fmt.Errorf("%w: %q begins with %q", format.ErrInvalid, name, name[:1])
	case strings.EqualFold(name, "node_modules") || strings.EqualFold(name, "favicon.ico"):
		return fmt.Errorf("%w: %q is not a package name", format.ErrInvalid, name)
	}
	for _, part := range []string{scope, base} {
		if part == "." || part == ".." {
			return fmt.Errorf("%w: %q has the part %q", format.ErrInvalid, name, part)
		}
		for i := range len(part) {
			if !uriSafe(part[i]) {
				return fmt.Errorf("%w: %q has %q, which a URL escapes", format.ErrInvalid, name, part[i:i+1])
			}
		}
	}
	return nil
}

// CheckNewName returns nil when npm takes name for a new package: CheckName, and no upper-case
// letter (docs/spec/http-routing.md, npm). Errors wrap format.ErrInvalid and name the rule.
func CheckNewName(name string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if strings.ToLower(name) != name {
		return fmt.Errorf("%w: %q has upper-case letters, which a new package name may not have", format.ErrInvalid, name)
	}
	return nil
}

// packagePath returns the package path of a name CheckName takes (docs/spec/manifest-profile.md
// §4.1): EscapeComponent of the name, or for @scope/name PackagePath of EscapeComponent of scope and
// of name.
func packagePath(name string) string {
	scope, base, _ := splitName(name)
	if scope == "" {
		return profile.EscapeComponent(base)
	}
	return profile.PackagePath(profile.EscapeComponent(scope), profile.EscapeComponent(base))
}

// Filename returns the file name npm serves the tarball of a version under: the name without
// its scope, -, the version, .tgz (widgets-1.2.0.tgz for @acme/widgets 1.2.0).
func Filename(name, version string) string {
	_, base, _ := splitName(name)
	return base + "-" + version + ".tgz"
}

// semver is a version of SemVer 2.0.0 without build metadata, as parseVersion reads it.
type semver struct {
	core [3]uint64 // major, minor, patch
	pre  []string  // the dot-separated identifiers of the pre-release; nil for a release
}

// parseVersion reads a SemVer 2.0.0 version: major.minor.patch, numbers without leading zeros
// and at most JavaScript's largest safe integer, and an optional pre-release of dot-separated
// identifiers of [0-9A-Za-z-], numeric ones without leading zeros. Build metadata (+…) is
// refused: npm strips it on publish, so two versions never differ by it alone. Errors wrap
// format.ErrInvalid.
func parseVersion(v string) (semver, error) {
	bad := func(why string) (semver, error) {
		return semver{}, fmt.Errorf("%w: the version %q is not SemVer 2.0.0: %s", format.ErrInvalid, v, why)
	}
	if len(v) > maxVersionLen {
		return bad(fmt.Sprintf("more than %d bytes", maxVersionLen))
	}
	if strings.Contains(v, "+") {
		return bad("it has build metadata, which npm strips")
	}
	core, pre, hasPre := strings.Cut(v, "-")
	nums := strings.Split(core, ".")
	if len(nums) != 3 {
		return bad("not major.minor.patch")
	}
	var s semver
	for i, n := range nums {
		if !numeric(n) {
			return bad(fmt.Sprintf("%q is not a number without leading zeros", n))
		}
		x, err := strconv.ParseUint(n, 10, 64)
		if err != nil || x > maxSafeInteger {
			return bad(fmt.Sprintf("%s is larger than %d", n, uint64(maxSafeInteger)))
		}
		s.core[i] = x
	}
	if !hasPre {
		return s, nil
	}
	s.pre = strings.Split(pre, ".")
	for _, id := range s.pre {
		if id == "" || strings.Trim(id, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-") != "" {
			return bad(fmt.Sprintf("the pre-release identifier %q", id))
		}
		if allDigits(id) && !numeric(id) {
			return bad(fmt.Sprintf("the pre-release identifier %q has a leading zero", id))
		}
	}
	return s, nil
}

// CheckVersion returns nil when v is a version npm publishes: SemVer 2.0.0 without build
// metadata (parseVersion). Errors wrap format.ErrInvalid.
func CheckVersion(v string) error {
	_, err := parseVersion(v)
	return err
}

// numeric reports whether s is a SemVer numeric identifier: digits, without a leading zero.
func numeric(s string) bool { return allDigits(s) && (s == "0" || s[0] != '0') }

// allDigits reports whether s is one or more ASCII digits.
func allDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

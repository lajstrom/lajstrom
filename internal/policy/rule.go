package policy

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// ErrInvalid is wrapped by the errors of ParseRule: the API answers them 400. format.ErrInvalid
// is the same error; this package does not import format, which imports it.
var ErrInvalid = errors.New("policy: invalid")

// VersionOrder compares two versions of one format in its order (format.VersionComparer): <0,
// 0 or >0, and an error for a version the order does not parse.
type VersionOrder func(a, b string) (int, error)

// The bounds of a rule's fields (docs/spec/policy.md §1.3).
const (
	maxPattern = 256
	maxReason  = 512
)

// RuleError is a field of a rule that docs/spec/policy.md §1.3 refuses.
type RuleError struct {
	Field   string
	Problem string
}

func (e *RuleError) Error() string { return e.Field + ": " + e.Problem }

// Unwrap is ErrInvalid.
func (e *RuleError) Unwrap() error { return ErrInvalid }

// ParseRule checks r as docs/spec/policy.md §1.3 does, at now: its mask, patterns, version
// constraint, effect, reason and expiry. order returns the order of a format's versions, nil
// for a format without one (or nil for none at all); formats reports whether a format is served
// (nil accepts every name). The error is a *RuleError.
func ParseRule(r index.PolicyRule, now time.Time, order func(format string) VersionOrder, formats func(string) bool) error {
	mf, err := checkMask(r.RepoMask, formats)
	if err != nil {
		return err
	}
	switch {
	case r.Name == "":
		return &RuleError{"name", "must not be empty"}
	case len(r.Name) > maxPattern:
		return &RuleError{"name", fmt.Sprintf("longer than %d bytes", maxPattern)}
	case len(r.Variant) > maxPattern:
		return &RuleError{"variant", fmt.Sprintf("longer than %d bytes", maxPattern)}
	case r.Effect != index.PolicyDeny && r.Effect != index.PolicyAllow:
		return &RuleError{"effect", fmt.Sprintf("%q is neither deny nor allow", r.Effect)}
	case r.ExpiresAt != nil && !r.ExpiresAt.After(now):
		return &RuleError{"expiresAt", "must be in the future"}
	}
	if err := checkReason(r.Reason); err != nil {
		return &RuleError{"reason", err.Error()}
	}
	return checkVersion(r.Version, mf, order)
}

// checkMask checks a rule's mask and returns its format segment.
func checkMask(mask string, formats func(string) bool) (string, error) {
	mf, mr, err := auth.ParseMask(mask)
	switch {
	case err != nil:
		return "", &RuleError{"repoMask", err.Error()}
	case mf != "*" && formats != nil && !formats(mf):
		return "", &RuleError{"repoMask", fmt.Sprintf("%q is not a format of this server, nor *", mf)}
	case mr != "*" && (len(mr) > config.MaxNameLen || !config.RepoNamePattern.MatchString(mr)):
		return "", &RuleError{"repoMask", fmt.Sprintf("%q is not a repo name (docs/spec/manifest-profile.md §4.1) or *", mr)}
	}
	return mf, nil
}

// checkVersion checks a rule's constraint for the format segment mf of its mask: a range needs
// one format that orders its versions.
func checkVersion(version, mf string, order func(format string) VersionOrder) error {
	c, err := parseConstraint(version)
	if err != nil {
		return &RuleError{"version", err.Error()}
	}
	if !c.ranges() {
		return nil
	}
	if mf == "*" || order == nil || order(mf) == nil {
		return &RuleError{"version", fmt.Sprintf("a range needs a mask of one format that orders its versions; %s does not", mf)}
	}
	return nil
}

// checkReason checks a reason: 1 to 512 bytes of UTF-8 without control characters.
func checkReason(s string) error {
	switch {
	case s == "":
		return errors.New("must not be empty")
	case len(s) > maxReason:
		return fmt.Errorf("longer than %d bytes", maxReason)
	case !utf8.ValidString(s):
		return errors.New("is not UTF-8")
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f || (c >= 0x80 && c < 0xa0) {
			return errors.New("has a control character")
		}
	}
	return nil
}

// constraint is a parsed version constraint (docs/spec/policy.md §1.2): every clause must hold;
// any is "*".
type constraint struct {
	any     bool
	clauses []clause
}

// clause is one operator and its version.
type clause struct {
	op      string // ==, !=, <, <=, >, >=
	version string
}

// operators in the order they are tried: the two-byte ones first.
var operators = []string{"==", "!=", "<=", ">=", "<", ">"}

// parseConstraint parses s, a constraint of docs/spec/policy.md §1.2.
func parseConstraint(s string) (constraint, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "*":
		return constraint{any: true}, nil
	case s == "":
		return constraint{}, errors.New(`must not be empty: "*" is every version`)
	case len(s) > maxPattern:
		return constraint{}, fmt.Errorf("longer than %d bytes", maxPattern)
	}
	var c constraint
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		op := "=="
		for _, o := range operators {
			if rest, ok := strings.CutPrefix(part, o); ok {
				op, part = o, strings.TrimSpace(rest)
				break
			}
		}
		switch {
		case part == "":
			return constraint{}, fmt.Errorf("%q has a clause without a version", s)
		case strings.ContainsAny(part, " \t=<>!"):
			return constraint{}, fmt.Errorf("%q: %q is not a version", s, part)
		case op != "==" && op != "!=" && strings.Contains(part, "*"):
			return constraint{}, fmt.Errorf("%q: a * is a glob of == and != only", s)
		}
		c.clauses = append(c.clauses, clause{op: op, version: part})
	}
	return c, nil
}

// ranges reports whether c has a clause that needs an order.
func (c constraint) ranges() bool {
	for _, cl := range c.clauses {
		if cl.op != "==" && cl.op != "!=" {
			return true
		}
	}
	return false
}

// matches reports whether version satisfies c, in order when it is not nil.
func (c constraint) matches(version string, order VersionOrder) bool {
	if c.any {
		return true
	}
	for _, cl := range c.clauses {
		if !cl.matches(version, order) {
			return false
		}
	}
	return true
}

// matches reports whether version satisfies the clause. A range without an order, or with a
// version the order does not parse, does not hold.
func (cl clause) matches(version string, order VersionOrder) bool {
	switch cl.op {
	case "==":
		return equal(cl.version, version, order)
	case "!=":
		return !equal(cl.version, version, order)
	}
	if order == nil {
		return false
	}
	n, err := order(version, cl.version)
	if err != nil {
		return false
	}
	switch cl.op {
	case "<":
		return n < 0
	case "<=":
		return n <= 0
	case ">":
		return n > 0
	default:
		return n >= 0
	}
}

// equal reports whether version is the clause's: a glob with a *, else equal in the order when
// it parses both, else the same bytes.
func equal(want, version string, order VersionOrder) bool {
	if strings.Contains(want, "*") {
		return glob(want, version)
	}
	if order != nil {
		if n, err := order(version, want); err == nil {
			return n == 0
		}
	}
	return want == version
}

// glob reports whether pattern matches s: * matches any run of bytes, every other byte itself.
func glob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	rest, ok := strings.CutPrefix(s, parts[0])
	if !ok {
		return false
	}
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, part)
		if i < 0 {
			return false
		}
		rest = rest[i+len(part):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}

// matchName reports whether a name or variant pattern matches s, ignoring ASCII case, as the
// lists of a proxy match (docs/spec/proxy-and-group.md §3).
func matchName(pattern, s string) bool { return glob(lowerASCII(pattern), lowerASCII(s)) }

// lowerASCII maps the bytes A to Z to a to z and leaves every other byte as it is.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

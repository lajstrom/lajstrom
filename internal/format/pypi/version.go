package pypi

import (
	"cmp"
	"fmt"
	"regexp"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// PEP 440's version order, as the version specifiers specification of the Python Packaging
// User Guide (PEP 440 as it is maintained) defines it, written from that text:
//
//   - a version is [N!]N(.N)*[{a|b|rc}N][.postN][.devN][+local]: an epoch, a release of one
//     or more numbers, a pre-release, a post-release, a development release and a local label,
//     each but the release optional;
//   - it is normalised: letters lowercased, the whitespace around it and a leading "v" dropped,
//     numbers read as integers (01 is 1, 0!1.0 is 1.0), alpha, beta, c, pre and preview spelt
//     a, b, rc, rc and rc, rev and r spelt post, "-N" a post-release, a missing number 0, the
//     "-", "_" or "." around a suffix's letters dropped, and those between the segments of the
//     local label read as ".";
//   - two versions compare by epoch, then by release, the shorter padded with zeros (1.0 is
//     1.0.0), then by the suffixes: a development release of the release itself (1.0.dev1)
//     comes first, then the pre-releases (1.0a1 < 1.0b1 < 1.0rc1), the release, and its
//     post-releases; a development release comes before the version it is one of (1.0a1.dev1 <
//     1.0a1, 1.0.post1.dev1 < 1.0.post1); then by the local label, none first, segment by
//     segment, a number after a word, numbers as integers and words as strings, and a label
//     after the labels it starts with.
//
// 1.0.dev1 < 1.0a1 < 1.0b2.post3 < 1.0rc1 < 1.0 = 1.0.0 < 1.0+local < 1.0.post1 < 1!0.1.

// versionParts is the pattern of a version, PEP 440's own, with a group for each part; it is
// matched against the version lowercased, its ASCII letters only, without the whitespace around
// it. The groups: 1 the epoch, 2 the release, 3 and 4 the pre-release's letters and number, 5
// the number of a post-release written -N, 6 and 7 the letters and number of another, 8 and 9
// the development release's, 10 the local label.
var versionParts = regexp.MustCompile(`^v?` +
	`(?:([0-9]+)!)?` +
	`([0-9]+(?:\.[0-9]+)*)` +
	`(?:[-_.]?(alpha|a|beta|b|preview|pre|c|rc)[-_.]?([0-9]*))?` +
	`(?:-([0-9]+)|[-_.]?(post|rev|r)[-_.]?([0-9]*))?` +
	`(?:[-_.]?(dev)[-_.]?([0-9]*))?` +
	`(?:\+([a-z0-9]+(?:[-_.][a-z0-9]+)*))?$`)

// preSpellings are the pre-release letters as the normalised form spells them.
var preSpellings = map[string]string{"a": "a", "alpha": "a", "b": "b", "beta": "b", "c": "rc", "pre": "rc", "preview": "rc", "rc": "rc"}

// preRanks orders the pre-releases of a release.
var preRanks = map[string]int{"a": 0, "b": 1, "rc": 2}

// pyVersion is a version as PEP 440 reads it. Every number is kept as its digits without
// leading zeros, "0" for zero: PEP 440 bounds none of them.
type pyVersion struct {
	epoch   string
	release []string // as written, trailing zeros included
	pre     string   // a, b or rc; "" for none
	preN    string
	post    string // "" for none
	dev     string // "" for none
	local   []string
}

// parseVersion reads a PEP 440 version. An error wraps format.ErrInvalid.
func parseVersion(s string) (pyVersion, error) {
	t := strings.TrimSpace(s)
	for i := range len(t) {
		if t[i] >= 0x80 {
			return pyVersion{}, fmt.Errorf("%w: %q is not a PEP 440 version", format.ErrInvalid, s)
		}
	}
	m := versionParts.FindStringSubmatch(strings.ToLower(t))
	if m == nil {
		return pyVersion{}, fmt.Errorf("%w: %q is not a PEP 440 version", format.ErrInvalid, s)
	}
	v := pyVersion{epoch: number(m[1])}
	for n := range strings.SplitSeq(m[2], ".") {
		v.release = append(v.release, number(n))
	}
	if m[3] != "" {
		v.pre, v.preN = preSpellings[m[3]], number(m[4])
	}
	switch {
	case m[5] != "":
		v.post = number(m[5])
	case m[6] != "":
		v.post = number(m[7])
	}
	if m[8] != "" {
		v.dev = number(m[9])
	}
	if m[10] != "" {
		for seg := range strings.FieldsFuncSeq(m[10], isLocalSeparator) {
			if isDigits(seg) {
				seg = number(seg)
			}
			v.local = append(v.local, seg)
		}
	}
	return v, nil
}

// compareVersions compares two versions in PEP 440's order: <0 when a comes before b, 0 when
// they are equal in it (1.0 and 1.0.0, 1.0c1 and 1.0rc1), >0 when a comes after b. A version
// that is not PEP 440's is an error that wraps format.ErrInvalid.
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

// isPrerelease reports whether v is a pre-release: it has a pre-release or a development
// release part (1.0rc1, 1.0.dev2, 1.0.post1.dev1), as pip reads it; 1.0.post1 and 1.0+local are
// not.
func (v pyVersion) isPrerelease() bool { return v.pre != "" || v.dev != "" }

// String returns the normalised form of v: 01.0-ALPHA.1 is 1.0a1, 1.0-5 is 1.0.post5.
func (v pyVersion) String() string {
	var b strings.Builder
	if v.epoch != "0" {
		b.WriteString(v.epoch + "!")
	}
	b.WriteString(strings.Join(v.release, "."))
	if v.pre != "" {
		b.WriteString(v.pre + v.preN)
	}
	if v.post != "" {
		b.WriteString(".post" + v.post)
	}
	if v.dev != "" {
		b.WriteString(".dev" + v.dev)
	}
	if len(v.local) > 0 {
		b.WriteString("+" + strings.Join(v.local, "."))
	}
	return b.String()
}

// compare compares v with w in PEP 440's order.
func (v pyVersion) compare(w pyVersion) int {
	if c := compareNumbers(v.epoch, w.epoch); c != 0 {
		return c
	}
	x, y := trimZeros(v.release), trimZeros(w.release)
	for i := range min(len(x), len(y)) {
		if c := compareNumbers(x[i], y[i]); c != 0 {
			return c
		}
	}
	if c := cmp.Compare(len(x), len(y)); c != 0 {
		return c
	}
	if c := v.preKey().compare(w.preKey()); c != 0 {
		return c
	}
	// No post-release comes before every post-release, no development release after every one.
	if c := compareOptional(v.post, w.post, -1); c != 0 {
		return c
	}
	if c := compareOptional(v.dev, w.dev, +1); c != 0 {
		return c
	}
	return compareLocal(v.local, w.local)
}

// preKey is the place of a version's pre-release among the suffixes of its release.
type preKey struct {
	// class is 0 for a development release of the release itself (no pre- and no
	// post-release), 1 for a pre-release, 2 for none: the release and its post-releases.
	class int
	rank  int
	n     string
}

func (v pyVersion) preKey() preKey {
	switch {
	case v.pre != "":
		return preKey{class: 1, rank: preRanks[v.pre], n: v.preN}
	case v.post == "" && v.dev != "":
		return preKey{class: 0}
	default:
		return preKey{class: 2}
	}
}

func (k preKey) compare(l preKey) int {
	if c := cmp.Compare(k.class, l.class); c != 0 || k.class != 1 {
		return c
	}
	if c := cmp.Compare(k.rank, l.rank); c != 0 {
		return c
	}
	return compareNumbers(k.n, l.n)
}

// compareOptional compares two numbers either of which may be absent (""): an absent one
// compares as absent, before every number when absent is -1, after every one when it is +1.
func compareOptional(x, y string, absent int) int {
	switch {
	case x == "" && y == "":
		return 0
	case x == "":
		return absent
	case y == "":
		return -absent
	}
	return compareNumbers(x, y)
}

// compareLocal compares two local labels: none first, then segment by segment, a number after a
// word, and a label after the labels it starts with.
func compareLocal(x, y []string) int {
	for i := range min(len(x), len(y)) {
		p, q := x[i], y[i]
		var c int
		switch pn, qn := isDigits(p), isDigits(q); {
		case pn && qn:
			c = compareNumbers(p, q)
		case pn:
			c = 1
		case qn:
			c = -1
		default:
			c = strings.Compare(p, q)
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(x), len(y))
}

// compareNumbers compares two numbers written without leading zeros: the longer is the larger.
func compareNumbers(x, y string) int {
	return cmp.Or(cmp.Compare(len(x), len(y)), strings.Compare(x, y))
}

// number is the digits of s without their leading zeros, "0" for zero and for none.
func number(s string) string {
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0"
	}
	return s
}

// trimZeros returns the release without its trailing zeros, which do not count in the order.
func trimZeros(release []string) []string {
	n := len(release)
	for n > 0 && release[n-1] == "0" {
		n--
	}
	return release[:n]
}

// isDigits reports whether s is ASCII digits, at least one.
func isDigits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// isLocalSeparator reports whether r separates the segments of a local label.
func isLocalSeparator(r rune) bool { return r == '.' || r == '-' || r == '_' }

// LatestVersion returns the version that pip installs without a specifier (format.LatestPicker):
// of the versions that are PEP 440 versions, those that are not pre-releases, or all of them when
// every one is; of those the highest that is not yanked in the journal state, else the highest
// yanked one. Versions equal in the order compare by their bytes.
func (Plugin) LatestVersion(versions []string, state []index.JournalState) string {
	yanked := map[string]bool{}
	for _, s := range state {
		if s.Kind == index.JournalYanked {
			yanked[s.Key] = true
		}
	}
	var finals, pres []string
	parsed := map[string]pyVersion{}
	for _, s := range versions {
		v, err := parseVersion(s)
		switch {
		case err != nil: // pip ignores it
		case v.isPrerelease():
			pres = append(pres, s)
		default:
			finals = append(finals, s)
		}
		parsed[s] = v
	}
	candidates := finals
	if len(candidates) == 0 {
		candidates = pres
	}
	highest := func(keep func(string) bool) string {
		best := ""
		for _, s := range candidates {
			if keep(s) && (best == "" || cmp.Or(parsed[s].compare(parsed[best]), strings.Compare(s, best)) > 0) {
				best = s
			}
		}
		return best
	}
	if best := highest(func(s string) bool { return !yanked[s] }); best != "" {
		return best
	}
	return highest(func(string) bool { return true })
}

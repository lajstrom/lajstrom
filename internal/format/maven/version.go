package maven

import (
	"cmp"
	"slices"
	"strings"
)

// Maven's version order, as the Maven documentation specifies it (the POM reference, "Version
// Order Specification"; the rules of ComparableVersion), written from that text:
//
//   - the version is lowercased and split into tokens at ".", "-" and "_" and at each transition
//     between ASCII digits and other characters, which counts as "-"; each token keeps its
//     separator, and an empty token is "0";
//   - a token of digits is a number; any other token is a qualifier, and "a", "b" and "m"
//     directly followed by a digit are alpha, beta and milestone, cr is rc, and ga, final and
//     release are the empty qualifier, the release;
//   - the null tokens (0 and the empty qualifier) at the end of the version are trimmed, and then
//     those at the end of the part before each "-", from the last to the first;
//   - two versions are compared token by token, the shorter padded with nulls: 0 where the other
//     has a number after a ".", the empty qualifier where it has anything else;
//   - a qualifier, whatever its separator, comes before a number after "-", which comes before a
//     number after "."; numbers compare as integers; qualifiers in the order alpha < beta <
//     milestone < rc < snapshot < "" < sp, and the qualifiers that are not one of these after
//     them, in the order of their bytes.
//
// 1.0-alpha-1 < 1.0-beta < 1.0-rc1 < 1.0 < 1.0-sp1 < 1.0.1 < 1.10, and 1.0 = 1 = 1.ga.

// versionItem is a token of a version, after the split and the trim.
type versionItem struct {
	// hyphen is true when the separator before the token is "-", "_" or a transition, false
	// for "." and for the first token.
	hyphen bool
	// number is true for a token of ASCII digits.
	number bool
	// value is a number without its leading zeros ("0" for zero), or a qualifier lowercased and
	// with its alias resolved ("" for ga, final and release).
	value string
	// rank is the place of a qualifier among the known ones, unknownQualifier for another.
	rank int
}

// qualifierRanks are the known qualifiers in Maven's order; the empty one is the release.
var qualifierRanks = map[string]int{"alpha": 0, "beta": 1, "milestone": 2, "rc": 3, "snapshot": 4, "": 5, "sp": 6}

// unknownQualifier is the rank of a qualifier that is not in qualifierRanks: after them all.
const unknownQualifier = 7

// qualifierAliases are the qualifiers that are another one.
var qualifierAliases = map[string]string{"cr": "rc", "final": "", "ga": "", "release": ""}

// shortQualifiers are the qualifiers that a single letter stands for when a digit follows it
// directly: 1.0-a1 is 1.0-alpha-1, and 1.0-a is the qualifier a.
var shortQualifiers = map[string]string{"a": "alpha", "b": "beta", "m": "milestone"}

// compareVersions compares two versions in Maven's version order: -1 when a comes before b, 0
// when they are equal in that order (1.0 and 1), +1 when a comes after b.
func compareVersions(a, b string) int {
	return compareItems(parseVersion(a), parseVersion(b))
}

// sortVersions sorts versions in Maven's version order, oldest first; versions that are equal
// in that order are sorted by their bytes, so that the order does not depend on the input's.
func sortVersions(versions []string) {
	parsed := make(map[string][]versionItem, len(versions))
	for _, v := range versions {
		parsed[v] = parseVersion(v)
	}
	slices.SortFunc(versions, func(a, b string) int {
		return cmp.Or(compareItems(parsed[a], parsed[b]), strings.Compare(a, b))
	})
}

// isPrerelease reports whether a version is a pre-release: it has a qualifier that comes before
// the release, alpha, beta, milestone, rc or snapshot (1.0-rc1, 2.0.0.M3, 1.0.0-beta.2), and not
// only qualifiers after it (1.0-sp1, 31.1-jre).
func isPrerelease(version string) bool {
	for _, item := range parseVersion(version) {
		if !item.number && item.rank < qualifierRanks[""] {
			return true
		}
	}
	return false
}

// parseVersion splits a version into its tokens and trims the nulls.
func parseVersion(version string) []versionItem {
	s := strings.ToLower(version)
	var items []versionItem
	hyphen := false
	for i := 0; ; {
		j := i
		if j < len(s) && !isVersionSeparator(s[j]) {
			digits := isDigit(s[j])
			for j < len(s) && !isVersionSeparator(s[j]) && isDigit(s[j]) == digits {
				j++
			}
		}
		// s[i:j] is the token, empty at a separator or at the end; a qualifier with a digit
		// after it ends at a transition.
		items = append(items, newVersionItem(hyphen, s[i:j], j < len(s) && isDigit(s[j])))
		switch {
		case j == len(s):
			return trimNulls(items)
		case isVersionSeparator(s[j]):
			hyphen, i = s[j] != '.', j+1
		default: // a transition between digits and other characters, which is a "-"
			hyphen, i = true, j
		}
	}
}

// newVersionItem returns the item of a token: a number, or a qualifier with its alias
// resolved; beforeDigit says that a digit follows the token directly.
func newVersionItem(hyphen bool, token string, beforeDigit bool) versionItem {
	if token == "" || isDigit(token[0]) {
		value := strings.TrimLeft(token, "0")
		if value == "" {
			value = "0"
		}
		return versionItem{hyphen: hyphen, number: true, value: value}
	}
	if long, ok := shortQualifiers[token]; ok && beforeDigit {
		token = long
	}
	if alias, ok := qualifierAliases[token]; ok {
		token = alias
	}
	rank, known := qualifierRanks[token]
	if !known {
		rank = unknownQualifier
	}
	return versionItem{hyphen: hyphen, value: token, rank: rank}
}

// trimNulls removes the null items at the end of each part of the version that starts at a
// "-" (and of the part before the first): 1.0.0-foo.0.0 is 1-foo, 1.0-0 is 1.
func trimNulls(items []versionItem) []versionItem {
	out := make([]versionItem, 0, len(items))
	start := 0
	for i := 1; i <= len(items); i++ {
		if i < len(items) && !items[i].hyphen {
			continue
		}
		end := i
		for end > start && items[end-1].isNull() {
			end--
		}
		out = append(out, items[start:end]...)
		start = i
	}
	return out
}

// compareItems compares two parsed versions token by token, the shorter padded with nulls.
func compareItems(x, y []versionItem) int {
	for i := range max(len(x), len(y)) {
		var p, q versionItem
		switch {
		case i >= len(x):
			q = y[i]
			p = q.null()
		case i >= len(y):
			p = x[i]
			q = p.null()
		default:
			p, q = x[i], y[i]
		}
		if c := p.compare(q); c != 0 {
			return c
		}
	}
	return 0
}

// isNull reports whether the item is 0 or the empty qualifier, which the trim removes.
func (x versionItem) isNull() bool {
	return x.value == "" || (x.number && x.value == "0")
}

// null is the item that pads a version where the other one has x: 0 for a number after ".",
// the empty qualifier for anything else. A qualifier after "." is padded with the empty
// qualifier too, as one after "-": the two compare alike, so 1.foo is 1-foo and after 1.
func (x versionItem) null() versionItem {
	if x.number && !x.hyphen {
		return versionItem{number: true, value: "0"}
	}
	return versionItem{hyphen: x.hyphen, rank: qualifierRanks[""]}
}

// class orders the kinds of items: a qualifier, then a number after "-", then a number after
// ".".
func (x versionItem) class() int {
	switch {
	case !x.number:
		return 0
	case x.hyphen:
		return 1
	default:
		return 2
	}
}

// compare compares two items of the same place in their versions.
func (x versionItem) compare(y versionItem) int {
	if c := cmp.Compare(x.class(), y.class()); c != 0 {
		return c
	}
	if x.number { // no leading zeros: the longer is the larger
		return cmp.Or(cmp.Compare(len(x.value), len(y.value)), strings.Compare(x.value, y.value))
	}
	if c := cmp.Compare(x.rank, y.rank); c != 0 || x.rank != unknownQualifier {
		return c
	}
	return strings.Compare(x.value, y.value)
}

// isVersionSeparator reports whether c separates the tokens of a version.
func isVersionSeparator(c byte) bool { return c == '.' || c == '-' || c == '_' }

// isDigit reports whether c is an ASCII digit.
func isDigit(c byte) bool { return '0' <= c && c <= '9' }

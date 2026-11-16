package deb

import (
	"errors"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
)

// The vectors of this file are those of the reference implementation, dpkg (main at
// f4941036b04e787c2803d6359802d6725d2476c6): the comparisons of scripts/t/Dpkg_Version.t, which
// it checks against dpkg --compare-versions, and the comparisons and the versions parseversion
// accepts and refuses of lib/dpkg/t/t-version.c (9549c9bea6ce955ded06bc0bc34c023800645ed0).

// TestCompareVersions compares each pair both ways.
func TestCompareVersions(t *testing.T) {
	const lt, eq, gt = -1, 0, 1
	for _, tt := range []struct {
		a    string
		want int
		b    string
	}{
		// scripts/t/Dpkg_Version.t.
		{"1.0-1", lt, "2.0-2"}, {"2.2~rc-4", lt, "2.2-1"}, {"2.2-1", gt, "2.2~rc-4"}, {"1.0000-1", eq, "1.0-1"},
		{"1", eq, "0:1"}, {"0", eq, "0:0-0"}, {"2:2.5", gt, "1:7.5"}, {"1:0foo", gt, "0foo"}, {"0:0foo", eq, "0foo"},
		{"0foo", eq, "0foo"}, {"0foo-0", eq, "0foo"}, {"0foo", eq, "0foo-0"}, {"0foo", gt, "0fo"},
		{"0foo-0", lt, "0foo+"}, {"0foo~1", lt, "0foo"}, {"0foo~foo+Bar", lt, "0foo~foo+bar"}, {"0foo~~", lt, "0foo~"},
		{"1~", lt, "1"}, {"12345+that-really-is-some-ver-0", lt, "12345+that-really-is-some-ver-10"},
		{"0foo-0", lt, "0foo-01"}, {"0foo.bar", gt, "0foobar"}, {"0foo.bar", gt, "0foo1bar"},
		{"0foo.bar", gt, "0foo0bar"}, {"0foo1bar-1", lt, "0foobar-1"}, {"0foo2.0", gt, "0foo2"},
		{"0foo2.0.0", lt, "0foo2.10.0"}, {"0foo2.0", lt, "0foo2.0.0"}, {"0foo2.0", lt, "0foo2.10"},
		{"0foo2.1", lt, "0foo2.10"}, {"1.09", eq, "1.9"}, {"1.0.8+nmu1", gt, "1.0.8"}, {"3.11", gt, "3.10+nmu1"},
		{"0.9j-20080306-4", gt, "0.9i-20070324-2"}, {"1.2.0~b7-1", gt, "1.2.0~b6-1"}, {"1.011-1", gt, "1.06-2"},
		{"0.0.9+dfsg1-1", gt, "0.0.8+dfsg1-3"}, {"4.6.99+svn6582-1", gt, "4.6.99+svn6496-1"}, {"53", gt, "52"},
		{"0.9.9~pre122-1", gt, "0.9.9~pre111-1"}, {"2:2.3.2-2+lenny2", gt, "2:2.3.2-2"}, {"1:3.8.1-1", gt, "3.8.GA-1"},
		{"1.0.1+gpl-1", gt, "1.0.1-2"}, {"1a", lt, "1000a"},

		// lib/dpkg/t/t-version.c, test_version_compare and test_version_parse.
		{"0:0-0", eq, "0:0-0"}, {"0:0-00", eq, "0:00-0"}, {"1:2-3", eq, "1:2-3"}, {"0:0-0", lt, "1:0-0"},
		{"0:1-1", lt, "0:2-1"}, {"0:1-1", lt, "0:1-2"}, {"0", eq, "0:0"}, {"0:0.0-0.0", eq, "0.0-0.0"},
		{"1:0", gt, "0"}, {"5:1", gt, "4:9"}, {"0:0-0-0", eq, "0-0-0"}, {"0:0:0-0", eq, "0:0:0-0"},
		{"0:0-0:0-0", eq, "0:0-0:0-0"}, {"  \t0:0-1", eq, "0:0-1\t  "},

		// Debian Policy §5.6.12: ~~ < ~~a < ~ < the end < a.
		{"1.0~~", lt, "1.0~~a"}, {"1.0~~a", lt, "1.0~"}, {"1.0~", lt, "1.0"}, {"1.0", lt, "1.0a"},
		// Letters before the other characters, and numbers past 64 bits.
		{"1.0a", lt, "1.0+"}, {"1.0+", lt, "1.0."}, {"1.0z-1", lt, "1.0-z-1"},
		{"99999999999999999999999", lt, "100000000000000000000000"}, {"2147483647:1", gt, "2147483646:9"},
	} {
		for _, c := range []struct {
			a, b string
			want int
		}{{tt.a, tt.b, tt.want}, {tt.b, tt.a, -tt.want}} {
			got, err := compareVersions(c.a, c.b)
			if err != nil {
				t.Errorf("compareVersions(%q, %q): %v", c.a, c.b, err)
				continue
			}
			if got = max(-1, min(1, got)); got != c.want {
				t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
			}
		}
	}
}

// TestParseVersion: the parts of a version, and the versions dpkg refuses (t-version.c's
// test_version_parse, both its errors and its warnings).
func TestParseVersion(t *testing.T) {
	for in, want := range map[string]debVersion{
		"0":                  {epoch: "0", upstream: "0"},
		"1:0":                {epoch: "1", upstream: "0"},
		"0:0-0-0-0":          {epoch: "0", upstream: "0-0-0", revision: "0"},
		"0:0:0:0-0":          {epoch: "0", upstream: "0:0:0", revision: "0"},
		"0:09azAZ.-+~:-0":    {epoch: "0", upstream: "09azAZ.-+~:", revision: "0"},
		"0:0-azAZ09.+~":      {epoch: "0", upstream: "0", revision: "azAZ09.+~"},
		"007:1.0":            {epoch: "7", upstream: "1.0"},
		" 2147483647:1.0-1 ": {epoch: "2147483647", upstream: "1.0", revision: "1"},
	} {
		got, err := parseVersion(in)
		if err != nil || got != want {
			t.Errorf("parseVersion(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	invalid := []string{"", "  ", "0:", ":1.0", "1.0-", "0:0 0-1", "-1:0-1", "999999999999999999999999:0-1",
		"2147483648:0-1", "a:0-0", "A:0-0", "-0", "0:-0", "0:abc3-0", "0:0-0:0", "foo5.2", "10a:5.2"}
	for _, c := range "!#@$%&/|\\<>()[]{};,_=*^'" {
		invalid = append(invalid, "0:0"+string(c)+"-0")
	}
	for _, c := range "!#@$%&/|\\<>()[]{}:;,_=*^'" {
		invalid = append(invalid, "0:0-"+string(c))
	}
	for _, s := range invalid {
		if _, err := parseVersion(s); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("parseVersion(%q): %v, want format.ErrInvalid", s, err)
		}
		if _, err := compareVersions("1.0", s); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("compareVersions(1.0, %q): %v, want format.ErrInvalid", s, err)
		}
	}
}

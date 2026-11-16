package rpm

import "testing"

// TestVercmp compares version strings with the vectors of the reference implementation, rpm's
// tests/rpmvercmp.at (master at 5dae33c3d0c51c97d5834fe2ecf768b6f5483b81), each pair both ways,
// including the cases it keeps commented out as arguably buggy, which rpm answers as below.
func TestVercmp(t *testing.T) {
	const lt, eq, gt = -1, 0, 1
	for _, tt := range []struct {
		a    string
		want int
		b    string
	}{
		{"1.0", eq, "1.0"}, {"1.0", lt, "2.0"}, {"2.0.1", eq, "2.0.1"}, {"2.0", lt, "2.0.1"},
		{"2.0.1a", eq, "2.0.1a"}, {"2.0.1a", gt, "2.0.1"}, {"5.5p1", eq, "5.5p1"}, {"5.5p1", lt, "5.5p2"},
		{"5.5p10", eq, "5.5p10"}, {"5.5p1", lt, "5.5p10"}, {"10xyz", lt, "10.1xyz"}, {"xyz10", eq, "xyz10"},
		{"xyz10", lt, "xyz10.1"}, {"xyz.4", eq, "xyz.4"}, {"xyz.4", lt, "8"}, {"xyz.4", lt, "2"},
		{"5.5p2", lt, "5.6p1"}, {"5.6p1", lt, "6.5p1"}, {"6.0.rc1", gt, "6.0"}, {"10b2", gt, "10a1"},
		{"10a2", lt, "10b2"}, {"1.0aa", eq, "1.0aa"}, {"1.0a", lt, "1.0aa"}, {"10.0001", eq, "10.0001"},
		{"10.0001", eq, "10.1"}, {"10.0001", lt, "10.0039"}, {"4.999.9", lt, "5.0"}, {"20101121", eq, "20101121"},
		{"20101121", lt, "20101122"}, {"2_0", eq, "2_0"}, {"2.0", eq, "2_0"},
		// RhBug:178798.
		{"a", eq, "a"}, {"a+", eq, "a+"}, {"a+", eq, "a_"}, {"+a", eq, "+a"}, {"+a", eq, "_a"}, {"+_", eq, "+_"},
		{"_+", eq, "+_"}, {"_+", eq, "_+"}, {"+", eq, "_"},
		// "~".
		{"1.0~rc1", eq, "1.0~rc1"}, {"1.0~rc1", lt, "1.0"}, {"1.0~rc1", lt, "1.0~rc2"},
		{"1.0~rc1~git123", eq, "1.0~rc1~git123"}, {"1.0~rc1~git123", lt, "1.0~rc1"},
		// "^".
		{"1.0^", eq, "1.0^"}, {"1.0^", gt, "1.0"}, {"1.0^git1", eq, "1.0^git1"}, {"1.0^git1", gt, "1.0"},
		{"1.0^git1", lt, "1.0^git2"}, {"1.0^git1", lt, "1.01"}, {"1.0^20160101", eq, "1.0^20160101"},
		{"1.0^20160101", lt, "1.0.1"}, {"1.0^20160101^git1", eq, "1.0^20160101^git1"},
		{"1.0^20160102", gt, "1.0^20160101^git1"},
		// "~" and "^".
		{"1.0~rc1^git1", eq, "1.0~rc1^git1"}, {"1.0~rc1^git1", gt, "1.0~rc1"}, {"1.0^git1~pre", eq, "1.0^git1~pre"},
		{"1.0^git1", gt, "1.0^git1~pre"},
		// The oddities: RhBug:811992, and non-ASCII characters, which only separate.
		{"1b.fc17", eq, "1b.fc17"}, {"1b.fc17", lt, "1.fc17"}, {"1g.fc17", eq, "1g.fc17"}, {"1g.fc17", gt, "1.fc17"},
		{"1.1.α", eq, "1.1.α"}, {"1.1.α", eq, "1.1.β"}, {"1.1.αα", eq, "1.1.α"}, {"1.1.ββ", eq, "1.1.αα"},
		// Numbers past 64 bits.
		{"99999999999999999999999", lt, "100000000000000000000000"},
	} {
		check(t, "vercmp", vercmp, tt.a, tt.want, tt.b)
	}
}

// TestCompareVersions compares versions [epoch:]version[-release] as rpm's rpmverCmp does: the
// epoch first, 0 when there is none, then the version, then the release when both have one.
func TestCompareVersions(t *testing.T) {
	const lt, eq, gt = -1, 0, 1
	for _, tt := range []struct {
		a    string
		want int
		b    string
	}{
		{"1.0-1", eq, "0:1.0-1"}, {"1.0-1", eq, ":1.0-1"}, {"1:1.0-1", gt, "9.9-9"}, {"10:1.0-1", gt, "9:2.0-1"},
		{"007:1.0-1", eq, "7:1.0-1"}, {"1.0-1", lt, "1.0-2"}, {"1.0-2", lt, "1.0-10"}, {"1.0-1", lt, "1.0-1.el9"},
		{"1.0-1.el9", lt, "1.0.1-1"}, {"2.0", eq, "2.0-1"}, {"2.0", eq, "2.0-7"}, {"2.0", lt, "2.0.1-1"},
		{"1.0~rc1-1", lt, "1.0-1"}, {"1.0-1", lt, "1.0^git1-1"}, {"1.0-1~beta", lt, "1.0-1"},
		{"1.2.3-4.fc40", gt, "1.2.3-4.fc39"}, {"a:1.0-1", lt, "a:2.0-1"},
	} {
		check(t, "compareVersions", compareVersions, tt.a, tt.want, tt.b)
	}
}

// check compares a and b both ways with compare.
func check(t *testing.T, name string, compare func(a, b string) int, a string, want int, b string) {
	t.Helper()
	if got := max(-1, min(1, compare(a, b))); got != want {
		t.Errorf("%s(%q, %q) = %d, want %d", name, a, b, got, want)
	}
	if got := max(-1, min(1, compare(b, a))); got != -want {
		t.Errorf("%s(%q, %q) = %d, want %d", name, b, a, got, -want)
	}
}

// TestLatestVersion: the highest version in rpm's order, what dnf installs from one repo.
func TestLatestVersion(t *testing.T) {
	for _, tt := range []struct {
		versions []string
		want     string
	}{
		{[]string{"1.0~rc1-1", "1.0-1", "0.9-3"}, "1.0-1"},
		{[]string{"1.0-1", "1.0^git1-1"}, "1.0^git1-1"},
		{[]string{"1.10-1", "1.9-1"}, "1.10-1"},
		{[]string{"2.0-1", "1:0.9-1"}, "1:0.9-1"},
		{[]string{"1.0-1.el9", "1.0-10.el9", "1.0-2.el9"}, "1.0-10.el9"},
		{[]string{"1.0-1", "0:1.0-1"}, "1.0-1"},
		{nil, ""},
	} {
		if got := (Plugin{}).LatestVersion(tt.versions, nil); got != tt.want {
			t.Errorf("LatestVersion(%q) = %q, want %q", tt.versions, got, tt.want)
		}
	}
}

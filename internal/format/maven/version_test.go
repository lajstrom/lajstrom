package maven

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// TestCompareVersions checks Maven's version order on pairs: the examples of the Maven
// documentation (the POM reference, "Version Order Specification"), and versions as releases
// spell them, whose order is the one Maven 3.9's own tool (java -jar maven-artifact.jar a b)
// prints. Each pair is compared both ways.
func TestCompareVersions(t *testing.T) {
	const lt, eq = -1, 0
	for _, tt := range []struct {
		a    string
		want int
		b    string
	}{
		// The order of issue #55.
		{"1.0-alpha-1", lt, "1.0-beta"},
		{"1.0-beta", lt, "1.0-rc1"},
		{"1.0-rc1", lt, "1.0"},
		{"1.0", lt, "1.0-sp1"},
		{"1.0-sp1", lt, "1.0.1"},
		{"1.0.1", lt, "1.10"},

		// The examples of the documentation.
		{"1", lt, "1.1"},
		{"1-snapshot", lt, "1"},
		{"1", lt, "1-sp"},
		{"1-foo2", lt, "1-foo10"},
		{"1.foo", eq, "1-foo"},
		{"1-foo", lt, "1-1"},
		{"1-1", lt, "1.1"},
		{"1.ga", eq, "1"},
		{"1-ga", eq, "1"},
		{"1-0", eq, "1"},
		{"1_0", eq, "1"}, // "_" separates, as the documentation says; Maven 3.9 reads it as a qualifier
		{"1.0", eq, "1"},
		{"1-ga", lt, "1-sp"},
		{"1-ga.1", lt, "1-sp.1"},
		{"1-sp-1", lt, "1-ga-1"},
		{"1-a1", eq, "1-alpha-1"},
		{"1.0-alpha1", eq, "1.0-ALPHA1"},
		{"1.K", lt, "1.7"},
		{"5.aardvark", lt, "5.zebra"},
		{"1.0.0", eq, "1"},
		{"1.final", eq, "1"},
		{"1.", eq, "1"},
		{"1-", eq, "1"},
		{"1.0.0-foo.0.0", eq, "1-foo"},
		{"1.0.0-0.0.0", eq, "1"},
		{"1-alpha", lt, "1-a1"},
		{"1-a1", lt, "1-beta"},
		{"1-beta", lt, "1-b1"},
		{"1-b1", lt, "1-milestone"},
		{"1-milestone", lt, "1-m1"},
		{"1-m1", lt, "1-rc"},
		{"1-rc", eq, "1-cr"},
		{"1-rc", lt, "1-snapshot"},
		{"1-final", eq, "1-release"},
		{"1-sp", lt, "1-zzz"},

		// Releases as they are spelt.
		{"1.0-RC1", eq, "1.0-rc-1"},
		{"1.0-rc1", eq, "1.0-CR1"},
		{"1.0-rc2", lt, "1.0-RC10"},
		{"1.0-M2", lt, "1.0-M10"},
		{"1.0-M10", lt, "1.0-RC1"},
		{"1.0-RC1", lt, "1.0-SNAPSHOT"},
		{"1.0-SNAPSHOT", lt, "1.0"},
		{"1.0", eq, "1.0.Final"},
		{"1.0", eq, "1.0.GA"},
		{"1.0", eq, "1.0.RELEASE"},
		{"1.0", lt, "1.0-a"}, // a without a digit after it is no alpha: an unknown qualifier, after sp
		{"1.0-sp", lt, "1.0-a"},
		{"2.0.0.M3", lt, "2.0.0.RC1"},
		{"2.0.0.RC1", lt, "2.0.0.RELEASE"},
		{"2.0.0.RELEASE", lt, "2.0.1.RELEASE"},
		{"2.0.0-beta.2", lt, "2.0.0-beta.11"},
		{"2.0.0-alpha.beta", lt, "2.0.0-alpha.1"},
		{"2.0.0-rc.1", lt, "2.0.0"},
		{"2.0.0", lt, "2.0.0+1"},
		{"31.1-android", lt, "31.1-jre"},
		{"31.1-jre", lt, "32.0.0-jre"},
		{"4.13-beta-3", lt, "4.13"},
		{"4.13", lt, "4.13.2"},
		{"1.0", lt, "1.0-1"},
		{"1.0-1", lt, "1.0.1"},
		{"1.2", lt, "1.2-20231010"},
		{"1.0-20231010", lt, "1.0-20231010.120000-1"},
		{"1.9", lt, "1.10"},
		{"1.01", eq, "1.1"},
		{"00001", eq, "1"},
		{"1.0.0.0.1", lt, "1.0.1"},
		{"99999999999999999999", lt, "100000000000000000000"},
		{"1.0.foo.1", lt, "1.0.1"},
	} {
		if got := compareVersions(tt.a, tt.b); got != tt.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
		if got := compareVersions(tt.b, tt.a); got != -tt.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tt.b, tt.a, got, -tt.want)
		}
	}
}

func TestIsPrerelease(t *testing.T) {
	for _, tt := range []struct {
		version string
		want    bool
	}{
		{"1.0-alpha-1", true}, {"1.0-a1", true}, {"1.0-BETA", true}, {"1.0-b2", true}, {"2.0.0-M3", true}, {"2.0.0.M3", true},
		{"1.0-milestone-1", true}, {"1.0-rc1", true}, {"1.0.0-rc.1", true}, {"1.0-CR2", true}, {"1.0-SNAPSHOT", true},
		{"1.0-rc1-patch", true},
		{"1.0", false}, {"1.0.Final", false}, {"1.0.RELEASE", false}, {"1.0-ga", false}, {"1.0-sp1", false}, {"31.1-jre", false},
		{"1.0-a", false}, {"1.0-20231010", false}, {"2.0.0+build", false}, {"1.0-1", false},
	} {
		if got := isPrerelease(tt.version); got != tt.want {
			t.Errorf("isPrerelease(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}

// TestSortVersions sorts a shuffled list of versions into Maven's order: the golden order is the
// one Maven 3.9's tool prints for it, every version before the next. Versions that are equal in
// that order are sorted by their bytes.
func TestSortVersions(t *testing.T) {
	want := []string{
		"1.0-alpha", "1.0-alpha-1", "1.0-a2", "1.0-beta", "1.0-b2", "1.0-beta-10", "1.0-M1", "1.0-milestone-2", "1.0-M10",
		"1.0-rc1", "1.0-CR2", "1.0-RC10", "1.0-SNAPSHOT", "1", "1.0", "1.0.0", "1.0-sp1", "1.0-sp2", "1.0-android", "1.0-jre",
		"1.0-1", "1.0.1", "1.0.2-rc.1", "1.0.2", "1.1", "1.2", "1.2-20231010", "1.10",
		"2.0.0-M1", "2.0.0-M3", "2.0.0.RC1", "2.0.0.RELEASE", "2.0.1.RELEASE", "10",
	}
	r := rand.New(rand.NewPCG(55, 1)) //nolint:gosec // a fixed shuffle of a test, not a secret
	for range 20 {
		got := slices.Clone(want)
		r.Shuffle(len(got), func(i, j int) { got[i], got[j] = got[j], got[i] })
		sortVersions(got)
		if !slices.Equal(got, want) {
			t.Fatalf("sortVersions:\n got %q\nwant %q", got, want)
		}
	}
}

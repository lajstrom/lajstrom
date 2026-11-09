package profile_test

import (
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestFuzzCorpus writes testdata/fuzz/FuzzEscape and FuzzUnescape when
// LAJSTROM_FUZZ_CORPUS=write: versions, names, variants and paths of the testutil fixtures, as
// they are for FuzzEscape, and escaped, as path components and as tags for FuzzUnescape. It is
// an external test: testutil imports profile.
func TestFuzzCorpus(t *testing.T) {
	_, deb := testutil.Deb(testutil.DebOptions{Name: "libstdc++6", Version: "1:14.2.0-4ubuntu2~24.04"})
	_, whl := testutil.Wheel(testutil.WheelOptions{Name: "Zope.Interface", Version: "7.1.1.post1+local", PythonTag: "cp311.cp312",
		AbiTag: "abi3", PlatformTag: "manylinux_2_17_x86_64.manylinux2014_x86_64"})
	_, pom := testutil.POM(testutil.POMOptions{GroupID: "org.apache.commons", ArtifactID: "commons-lang3", Version: "3.17.0"})
	plain, escaped := map[string]any{}, map[string]any{}
	for name, s := range map[string]string{
		"deb-version": deb.Version, "deb-filename": deb.Filename, "wheel-version": whl.Version, "wheel-variant": whl.Variant,
		"maven-path": pom.Path, "raw-path": "Docs/Release Notes/v2.0 (final).PDF", "unicode": ".Пакет/über-ß",
	} {
		plain[name] = s
		escaped[name] = profile.Escape(s)
		escaped[name+"-component"] = profile.EscapeComponent(s)
	}
	escaped["tag"] = profile.Tag(whl.Version, whl.Variant)
	escaped["tag-hashed"] = profile.Tag(deb.Version, strings.Repeat(whl.Variant, 3))
	testutil.WriteFuzzCorpus(t, "FuzzEscape", plain)
	testutil.WriteFuzzCorpus(t, "FuzzUnescape", escaped)
}

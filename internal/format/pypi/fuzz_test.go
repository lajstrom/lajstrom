package pypi

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// ociTag is the tag grammar of the OCI distribution spec.
var ociTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// FuzzWheelFilename feeds ParseWheelFilename a file name; testdata/fuzz/FuzzWheelFilename has
// the file names of testutil.Wheel, which TestFuzzCorpus writes. Every error must be
// format.ErrInvalid, and what it allocates must grow with the name. A name it accepts
// round-trips: the parts make the file name again; with the project name normalised it is
// still a wheel, of the same project; the tags are not empty, and the version and the variant
// make an OCI tag. A name it refuses stays refused: describe, which reads the file name of an
// upload, refuses it rather than take it for another kind of distribution, and so it is below
// a directory.
func FuzzWheelFilename(f *testing.F) {
	for _, s := range []string{"requests-2.32.3-py3-none-any.whl", "tool-1.0-2-py3-none-any.whl",
		"numpy-2.1.3-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl", "x.whl", "-----.whl",
		"a-1-x2-b-c-d.whl", "_a-1-b-c-d.whl", "a/b-1-c-d-e.whl", "A.B__c-1_0-py3-none-any.whl", "a-1-py3-none-any.whl.whl"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		var (
			w   Wheel
			err error
		)
		if n := testutil.Allocated(func() { w, err = ParseWheelFilename(s) }); n > testutil.AllocSlack+64*len(s) {
			t.Fatalf("ParseWheelFilename of %d bytes allocated %d bytes", len(s), n)
		}
		if err != nil {
			if !errors.Is(err, format.ErrInvalid) {
				t.Fatalf("ParseWheelFilename(%q) = %v, want ErrInvalid", s, err)
			}
			if _, err := describe(s, ""); err == nil && strings.HasSuffix(s, ".whl") {
				t.Fatalf("describe accepted %q, which ParseWheelFilename refuses", s)
			}
			if _, err := ParseWheelFilename("dir/" + s); err == nil {
				t.Fatalf("ParseWheelFilename accepted dir/%s", s)
			}
			return
		}
		checkWheel(t, s, w)
	})
}

// checkWheel checks the wheel w that ParseWheelFilename read from the file name s.
func checkWheel(t *testing.T, s string, w Wheel) {
	t.Helper()
	rest := []string{strings.ReplaceAll(w.Version, "-", "_")}
	if w.Build != "" {
		rest = append(rest, w.Build)
	}
	rest = append(rest, w.Python, w.ABI, w.Platform)
	if again := w.Name + "-" + strings.Join(rest, "-") + ".whl"; again != s {
		t.Fatalf("%q parsed into %+v, which makes %q", s, w, again)
	}
	normalized := strings.ReplaceAll(Normalize(w.Name), "-", "_") + "-" + strings.Join(rest, "-") + ".whl"
	if n, err := ParseWheelFilename(normalized); err != nil || Normalize(n.Name) != Normalize(w.Name) ||
		n.Version != w.Version || n.Variant() != w.Variant() {
		t.Fatalf("%q, normalised %q, parsed into %+v, %v", s, normalized, n, err)
	}
	if w.Python == "" || w.ABI == "" || w.Platform == "" || strings.Contains(w.Version, "_") {
		t.Fatalf("%q parsed into %+v", s, w)
	}
	if tag := profile.Tag(w.Version, w.Variant()); !ociTag.MatchString(tag) {
		t.Fatalf("%q: the tag %q is not an OCI tag", s, tag)
	}
}

// TestFuzzCorpus writes testdata/fuzz/FuzzWheelFilename when LAJSTROM_FUZZ_CORPUS=write: the
// file names of testutil.Wheel, one with a build tag, and the file name of an sdist.
func TestFuzzCorpus(t *testing.T) {
	seeds := map[string]any{}
	for name, o := range map[string]testutil.WheelOptions{
		"default":         {},
		"compressed-tags": {Name: "numpy", Version: "2.1.3", PythonTag: "cp311.cp312", AbiTag: "abi3", PlatformTag: "manylinux_2_17_x86_64.manylinux2014_x86_64"},
		"local-version":   {Name: "torch", Version: "2.5.1+cu124", PythonTag: "cp312", AbiTag: "cp312", PlatformTag: "win_amd64"},
		"dotted-name":     {Name: "Zope.Interface", Version: "7.1.1.post1", PythonTag: "py2.py3"},
		"pre-release":     {Name: "my_pkg", Version: "1.0.0rc1.dev3"},
	} {
		_, info := testutil.Wheel(o)
		seeds[name] = info.Filename
	}
	_, info := testutil.Wheel(testutil.WheelOptions{})
	seeds["build-tag"] = strings.Replace(info.Filename, "-py3-", "-1b-py3-", 1)
	_, sdist := testutil.Sdist(testutil.SdistOptions{})
	seeds["sdist"] = sdist.Filename
	testutil.WriteFuzzCorpus(t, "FuzzWheelFilename", seeds)
}

package pypi

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
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
		testutil.CheckAllocated(t, fmt.Sprintf("ParseWheelFilename of %d bytes", len(s)), testutil.AllocSlack+64*len(s),
			func() { w, err = ParseWheelFilename(s) })
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

// FuzzCoreMetadata feeds ParseCoreMetadata a METADATA or PKG-INFO file;
// testdata/fuzz/FuzzCoreMetadata has those of testutil and real-looking ones, which
// TestFuzzCorpus writes. Every error must be format.ErrInvalid, and what it allocates must grow
// with the file. Line endings do not matter: a file with LF line endings gives with CRLF the
// same metadata, or is refused too. (Not when a line ends with "\r" already: the scanner takes
// off one "\r" and ParseCoreMetadata another, so "\r\r\n" and "\r\r\r\n" end lines that differ by
// one "\r"; testdata/fuzz/FuzzCoreMetadata/cr-before-lf is such a file.) A file it accepts has
// its name and version, lists for the multiple-use fields and
// the keywords, and written back by metadataOf, it parses to the same metadata. A file with a
// field named "" (a line " :x" before the first field) cannot be written back and is left out.
func FuzzCoreMetadata(f *testing.F) {
	for _, s := range []string{
		"Metadata-Version: 2.1\nName: p\nVersion: 1\n",
		"Metadata-Version: 1.0\r\nName: p\r\nVersion: 1\r\nDescription: first\r\n        |second\r\n        |\r\n",
		"Metadata-Version: 2.4\nName: p\nVersion: 1\nClassifier: a\nClassifier: b\n\tc\nKeywords: x, y  z\n\n# p\n\nbody\n\n",
		"Metadata-Version: 2.1\nName: p\nVersion: 1\nSummary: x\n y\n \n",
		" :x\nMetadata-Version: 2.1\nName: p\nVersion: 1",
		"Metadata-Version: 2.1\nName: p\nVersion: 1\nLicense: MIT\r\r\n with\r\n\n\r\n",
		"Name: p\nVersion: 1\n", "not a field\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var (
			m   CoreMetadata
			err error
		)
		testutil.CheckAllocated(t, fmt.Sprintf("ParseCoreMetadata of %d bytes", len(b)), coreMetadataAllocBound(len(b)),
			func() { m, err = ParseCoreMetadata(b) })
		if !bytes.Contains(b, []byte("\r\n")) {
			crlf, crlfErr := ParseCoreMetadata(bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n")))
			if (err == nil) != (crlfErr == nil) || err == nil && !reflect.DeepEqual(m, crlf) {
				t.Fatalf("with CRLF, ParseCoreMetadata = %v, %+v; with LF %v, %+v", crlfErr, crlf, err, m)
			}
		}
		if err != nil {
			if !errors.Is(err, format.ErrInvalid) {
				t.Fatalf("ParseCoreMetadata = %v, want ErrInvalid", err)
			}
			return
		}
		checkCoreMetadata(t, m)
		if _, ok := m.Fields[""]; ok {
			return
		}
		back := metadataOf(m)
		again, err := ParseCoreMetadata(back)
		if err != nil || !reflect.DeepEqual(m, again) {
			t.Fatalf("written back as %q, the metadata parses to %v, %+v; first %+v", back, err, again, m)
		}
	})
}

// checkCoreMetadata checks the types of the fields of metadata ParseCoreMetadata accepted.
func checkCoreMetadata(t *testing.T, m CoreMetadata) {
	t.Helper()
	if m.Name == "" || m.Name != m.Fields["name"] || m.Version == "" || m.Version != m.Fields["version"] {
		t.Fatalf("name %q, version %q of %v", m.Name, m.Version, m.Fields)
	}
	for k, v := range m.Fields {
		_, isList := v.([]string)
		_, isString := v.(string)
		if want := slices.Contains(multipleUse, k) || k == "keywords"; isList != want || !isList && !isString {
			t.Fatalf("field %q is a %T", k, v)
		}
	}
}

// metadataOf writes m back as a core metadata file: a header per value, each continuation line
// after a space, the keywords joined with spaces, and the description as the body when it reads
// back the same from there (not empty, no "\n" at the end), as a header otherwise. Every line
// ends with "\r\r\n": the scanner takes off one "\r" and ParseCoreMetadata another, so that a
// line that ends with "\r" itself comes back as it is.
func metadataOf(m CoreMetadata) []byte {
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\r\r\n") }
	header := func(key, v string) {
		lines := strings.Split(v, "\n")
		line(key + ": " + lines[0])
		for _, l := range lines[1:] {
			line(" " + l)
		}
	}
	d, hasDescription := m.Fields["description"].(string)
	inBody := hasDescription && d != "" && !strings.HasSuffix(d, "\n")
	for _, k := range slices.Sorted(maps.Keys(m.Fields)) {
		switch v := m.Fields[k].(type) {
		case string:
			if k != "description" || !inBody {
				header(k, v)
			}
		case []string:
			if k == "keywords" {
				header(k, strings.Join(v, " "))
				continue
			}
			for _, e := range v {
				header(k, e)
			}
		}
	}
	if inBody {
		line("")
		for _, l := range strings.Split(d, "\n") {
			line(l)
		}
	}
	return []byte(b.String())
}

// TestFuzzCorpus writes testdata/fuzz/FuzzWheelFilename and FuzzCoreMetadata when
// LAJSTROM_FUZZ_CORPUS=write: the file names of testutil.Wheel, one with a build tag, and the
// file name of an sdist; the METADATA and PKG-INFO of testutil.Wheel and testutil.Sdist, a
// PKG-INFO of Metadata-Version 1.1 with its description in a header, one of 2.4 with a body, and
// a file of many continuation lines, which cost the square of its size before #261.
func TestFuzzCorpus(t *testing.T) {
	seeds, metadata := map[string]any{}, map[string]any{}
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
	metadata["wheel-default"] = info.Metadata
	_, deps := testutil.Wheel(testutil.WheelOptions{Name: "Flask-Login", Version: "0.6.3", RequiresPython: ">=3.7",
		RequiresDist: []string{"Flask>=1.0.4", "Werkzeug>=1.0.1", `pytest; extra == "test"`}})
	metadata["wheel-requires-dist"] = deps.Metadata
	_, sdist := testutil.Sdist(testutil.SdistOptions{})
	seeds["sdist"] = sdist.Filename
	metadata["sdist-pkg-info"] = sdist.PKGInfo
	metadata["pkg-info-1.1"] = []byte("Metadata-Version: 1.1\r\nName: zope.interface\r\nVersion: 3.6.1\r\n" +
		"Summary: Interfaces for Python\r\nHome-page: http://pypi.python.org/pypi/zope.interface\r\n" +
		"Author: Zope Foundation and Contributors\r\nLicense: ZPL 2.1\r\nDescription: *****************\r\n" +
		"        Interfaces for Python\r\n        *****************\r\n        \r\n        This package provides...\r\n" +
		"Keywords: interface,components,plugins\r\nPlatform: UNKNOWN\r\nClassifier: Development Status :: 5 - Production/Stable\r\n" +
		"Classifier: License :: OSI Approved :: Zope Public License\r\n")
	metadata["metadata-2.4"] = []byte("Metadata-Version: 2.4\nName: example-pkg\nVersion: 2.0.0rc1\n" +
		"Summary: An example\nAuthor-email: Jane Doe <jane@example.org>\nLicense-Expression: MIT OR Apache-2.0\n" +
		"License-File: LICENSE\nLicense-File: NOTICE\nProject-URL: Source, https://example.org/src\n" +
		"Requires-Python: >=3.10\nRequires-Dist: httpx>=0.27\nRequires-Dist: rich; extra == \"cli\"\nProvides-Extra: cli\n" +
		"Dynamic: license-file\nDescription-Content-Type: text/markdown\n\n# example-pkg\n\nRead me.\n\n    code\n")
	metadata["continuation-lines"] = []byte("Metadata-Version: 2.1\nName: p\nVersion: 1\nSummary: x" + strings.Repeat("\n y", 2048) + "\n")
	testutil.WriteFuzzCorpus(t, "FuzzWheelFilename", seeds)
	testutil.WriteFuzzCorpus(t, "FuzzCoreMetadata", metadata)
}

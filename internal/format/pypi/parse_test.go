package pypi

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"Requests": "requests", "zope.interface": "zope-interface", "Foo__Bar--baz..Qux": "foo-bar-baz-qux",
		"my_pkg": "my-pkg", "A": "a", "typing-extensions": "typing-extensions",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	for name, valid := range map[string]bool{"requests": true, "zope.interface": true, "a": true, "A1": true,
		"-x": false, "x-": false, "": false, "naïve": false, "a b": false, "x/y": false} {
		if got := ValidName(name); got != valid {
			t.Errorf("ValidName(%q) = %v", name, got)
		}
	}
}

func TestIsPEP440(t *testing.T) {
	for v, want := range map[string]bool{
		"1.0": true, "2024.11.9": true, "1.0.0a1": true, "1.0b2": true, "1.0rc1": true, "1.0.post1": true,
		"1.0-1": true, "1.0.dev3": true, "1!2.0": true, "1.0+local.7": true, "v1.0": true, "1.0.0-alpha": true,
		"1.0a1.post2.dev3": true, "0": true,
		"1.0-SNAPSHOT": false, "latest": false, "1.0.0.x": false, "": false, "1..0": false, "1.0+": false,
	} {
		if got := IsPEP440(v); got != want {
			t.Errorf("IsPEP440(%q) = %v, want %v", v, got, want)
		}
	}
}

// TestParseWheelFilename reads real-world wheel file names: pure, abi3, manylinux and musllinux
// with compressed tag sets, macOS universal2, Windows, a build tag, an epoch and a local version.
func TestParseWheelFilename(t *testing.T) {
	for _, tt := range []struct {
		filename string
		want     Wheel
		variant  string
	}{
		{"requests-2.32.3-py3-none-any.whl", Wheel{Name: "requests", Version: "2.32.3", Python: "py3", ABI: "none", Platform: "any"}, "py3-none-any"},
		{"six-1.16.0-py2.py3-none-any.whl", Wheel{Name: "six", Version: "1.16.0", Python: "py2.py3", ABI: "none", Platform: "any"}, "py2.py3-none-any"},
		{"cryptography-43.0.3-cp39-abi3-manylinux_2_28_x86_64.whl",
			Wheel{Name: "cryptography", Version: "43.0.3", Python: "cp39", ABI: "abi3", Platform: "manylinux_2_28_x86_64"}, "cp39-abi3-manylinux_2_28_x86_64"},
		{"numpy-2.1.3-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl",
			Wheel{Name: "numpy", Version: "2.1.3", Python: "cp312", ABI: "cp312", Platform: "manylinux_2_17_x86_64.manylinux2014_x86_64"},
			"cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64"},
		{"orjson-3.10.11-cp313-cp313-musllinux_1_2_aarch64.whl",
			Wheel{Name: "orjson", Version: "3.10.11", Python: "cp313", ABI: "cp313", Platform: "musllinux_1_2_aarch64"}, "cp313-cp313-musllinux_1_2_aarch64"},
		{"pydantic_core-2.23.4-cp311-cp311-macosx_10_12_universal2.whl",
			Wheel{Name: "pydantic_core", Version: "2.23.4", Python: "cp311", ABI: "cp311", Platform: "macosx_10_12_universal2"}, "cp311-cp311-macosx_10_12_universal2"},
		{"pywin32-308-cp312-cp312-win_amd64.whl", Wheel{Name: "pywin32", Version: "308", Python: "cp312", ABI: "cp312", Platform: "win_amd64"}, "cp312-cp312-win_amd64"},
		{"tool-1.0-2-py3-none-any.whl", Wheel{Name: "tool", Version: "1.0", Build: "2", Python: "py3", ABI: "none", Platform: "any"}, "py3-none-any"},
		{"tool-1.0-1b-py3-none-any.whl", Wheel{Name: "tool", Version: "1.0", Build: "1b", Python: "py3", ABI: "none", Platform: "any"}, "py3-none-any"},
		{"epoch-1!2.0-py3-none-any.whl", Wheel{Name: "epoch", Version: "1!2.0", Python: "py3", ABI: "none", Platform: "any"}, "py3-none-any"},
		{"torch-2.5.1+cpu-cp311-cp311-linux_x86_64.whl",
			Wheel{Name: "torch", Version: "2.5.1+cpu", Python: "cp311", ABI: "cp311", Platform: "linux_x86_64"}, "cp311-cp311-linux_x86_64"},
		{"legacy-1.0_1-py3-none-any.whl", Wheel{Name: "legacy", Version: "1.0-1", Python: "py3", ABI: "none", Platform: "any"}, "py3-none-any"},
	} {
		got, err := ParseWheelFilename(tt.filename)
		if err != nil {
			t.Errorf("%s: %v", tt.filename, err)
			continue
		}
		if d := cmp.Diff(tt.want, got); d != "" {
			t.Errorf("%s: (-want +got)\n%s", tt.filename, d)
		}
		if got.Variant() != tt.variant {
			t.Errorf("%s: variant %q, want %q", tt.filename, got.Variant(), tt.variant)
		}
	}
	for _, bad := range []string{
		"requests-2.32.3.tar.gz", "requests-py3-none-any.whl", "a-b-c-d-e-f-g.whl", "tool-1.0-x-py3-none-any.whl",
		"../evil-1.0-py3-none-any.whl", "sp ace-1.0-py3-none-any.whl", "-1.0-py3-none-any.whl", "tool-1.0--none-any.whl",
	} {
		if _, err := ParseWheelFilename(bad); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("ParseWheelFilename(%q): %v, want ErrInvalid", bad, err)
		}
	}
}

func TestParseSdistFilename(t *testing.T) {
	for _, tt := range []struct {
		filename string
		want     Sdist
	}{
		{"requests-2.32.3.tar.gz", Sdist{Name: "requests", Version: "2.32.3", Kind: SdistTarGz}},
		{"my_pkg-1.0.0.tar.gz", Sdist{Name: "my_pkg", Version: "1.0.0", Kind: SdistTarGz}},
		{"zope.interface-7.1.1.tar.gz", Sdist{Name: "zope.interface", Version: "7.1.1", Kind: SdistTarGz}},
		{"Old-Style-Name-0.9.zip", Sdist{Name: "Old-Style-Name", Version: "0.9", Kind: SdistZip}},
	} {
		got, err := ParseSdistFilename(tt.filename)
		if err != nil {
			t.Errorf("%s: %v", tt.filename, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s: %+v, want %+v", tt.filename, got, tt.want)
		}
	}
	for _, bad := range []string{"requests-2.32.3.tar.bz2", "requests.tar.gz", "requests-.tar.gz", "-1.0.tar.gz", "a/b-1.0.tar.gz"} {
		if _, err := ParseSdistFilename(bad); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("ParseSdistFilename(%q): %v, want ErrInvalid", bad, err)
		}
	}
}

func TestParseCoreMetadata(t *testing.T) {
	m, err := ParseCoreMetadata([]byte("Metadata-Version: 2.1\r\nName: My-Pkg\nVersion: 1.0.0\nSummary: A thing\n" +
		"Requires-Python: >=3.9\nRequires-Dist: requests>=2\nRequires-Dist: idna; extra == \"x\"\n" +
		"Classifier: License :: OSI Approved\nKeywords: one two  three\nLicense: MIT\n  with a continuation\n" +
		"Project-URL: Home, https://example.com\n\n# My-Pkg\n\nThe description.\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"metadata_version": "2.1", "name": "My-Pkg", "version": "1.0.0", "summary": "A thing", "requires_python": ">=3.9",
		"requires_dist": []string{"requests>=2", `idna; extra == "x"`}, "classifier": []string{"License :: OSI Approved"},
		"keywords": []string{"one", "two", "three"}, "license": "MIT\nwith a continuation",
		"project_url": []string{"Home, https://example.com"}, "description": "# My-Pkg\n\nThe description.",
	}
	if d := cmp.Diff(want, m.Fields); d != "" {
		t.Errorf("fields: (-want +got)\n%s", d)
	}
	if m.Name != "My-Pkg" || m.Version != "1.0.0" {
		t.Errorf("name %q, version %q", m.Name, m.Version)
	}
	for _, bad := range []string{"Name: x\nVersion: 1\n", "Metadata-Version: 2.1\nVersion: 1\n", "Metadata-Version: 2.1\nName: x\n", "not a field\n"} {
		if _, err := ParseCoreMetadata([]byte(bad)); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("ParseCoreMetadata(%q): %v, want ErrInvalid", bad, err)
		}
	}
}

// coreMetadataAllocBound is the most ParseCoreMetadata may allocate for a file of n bytes: the
// buffer of the scanner, a copy of each line, the keys, the map and the values.
func coreMetadataAllocBound(n int) int { return testutil.AllocSlack + 128*n }

// TestParseCoreMetadataContinuationLines checks that a field of many continuation lines costs
// what the file has, a single field and the last value of a multiple-use one: appending each
// line to the value copied it, and before #261 the 96 KiB here allocated 1.16 GB, quadrupling
// with each doubling of the lines.
func TestParseCoreMetadataContinuationLines(t *testing.T) {
	long := "x" + strings.Repeat("\ny", 1<<15)
	b := []byte("Metadata-Version: 2.1\nName: p\nVersion: 1\nSummary: x" + strings.Repeat("\n y", 1<<15) +
		"\nClassifier: a\nClassifier: b\n\tc\n d\nLicense: MIT\n\nThe description.\n")
	var (
		m   CoreMetadata
		err error
	)
	testutil.CheckAllocated(t, "ParseCoreMetadata", coreMetadataAllocBound(len(b)), func() { m, err = ParseCoreMetadata(b) })
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := m.Fields["summary"].(string); s != long {
		t.Errorf("summary has %d bytes, want %d", len(s), len(long))
	}
	want := map[string]any{"classifier": []string{"a", "b\nc\nd"}, "license": "MIT", "description": "The description."}
	for k, v := range want {
		if d := cmp.Diff(v, m.Fields[k]); d != "" {
			t.Errorf("%s (-want +got)\n%s", k, d)
		}
	}
}

func TestReadWheelMetadata(t *testing.T) {
	whl, info := testutil.Wheel(testutil.WheelOptions{Name: "My-Pkg", Version: "2.0.1", RequiresDist: []string{"requests"}})
	got, err := ReadWheelMetadata(bytes.NewReader(whl), int64(len(whl)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, info.Metadata) {
		t.Errorf("METADATA:\n%s\nwant\n%s", got, info.Metadata)
	}
	for name, members := range map[string][]string{
		"no METADATA":             {"my_pkg/__init__.py", "my_pkg-1.0.dist-info/WHEEL"},
		"two dist-info":           {"a-1.0.dist-info/METADATA", "b-1.0.dist-info/METADATA"},
		"METADATA not at the top": {"vendor/x-1.0.dist-info/METADATA"},
	} {
		z := zipOf(t, members...)
		if _, err := ReadWheelMetadata(bytes.NewReader(z), int64(len(z))); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := ReadWheelMetadata(strings.NewReader("not a zip"), 9); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("not a zip: %v", err)
	}
}

func TestReadSdistPKGInfo(t *testing.T) {
	tgz, info := testutil.Sdist(testutil.SdistOptions{Name: "My-Pkg", Version: "3.1"})
	s, err := ParseSdistFilename(info.Filename)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadSdistPKGInfo(bytes.NewReader(tgz), int64(len(tgz)), s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, info.PKGInfo) {
		t.Errorf("PKG-INFO:\n%s\nwant\n%s", got, info.PKGInfo)
	}
	other := s
	other.Version = "9.9"
	if _, err := ReadSdistPKGInfo(bytes.NewReader(tgz), int64(len(tgz)), other); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("another root: %v, want ErrInvalid", err)
	}
	z := zipOf(t, "Old-1.0/setup.py", "Old-1.0/PKG-INFO")
	if got, err := ReadSdistPKGInfo(bytes.NewReader(z), int64(len(z)), Sdist{Name: "Old", Version: "1.0", Kind: SdistZip}); err != nil ||
		string(got) != "Old-1.0/PKG-INFO" {
		t.Errorf("zip sdist: %q, %v", got, err)
	}
	if _, err := ReadSdistPKGInfo(bytes.NewReader(z), int64(len(z)), Sdist{Name: "Old", Version: "2.0", Kind: SdistZip}); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("zip sdist without PKG-INFO: %v", err)
	}
}

// zipOf returns a zip archive whose members hold their own names.
func zipOf(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(n))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

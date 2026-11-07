// Checked once with pip in a container on 2026-11-07, not in CI (#22). From the repository
// root, with the fixtures of TestWriteFixtures (the sdist build fetches setuptools from PyPI):
//
//	export LAJSTROM_FIXTURES_OUT=$(mktemp -d)
//	go test -count=1 -run TestWriteFixtures ./internal/testutil
//	docker run --rm --name pip-22 -v "$LAJSTROM_FIXTURES_OUT:/fixtures:ro" python:3.13-slim sh -ec '
//	  cd /root
//	  pip install --no-index --root-user-action=ignore --disable-pip-version-check /fixtures/pypi/my_pkg-1.0.0-py3-none-any.whl
//	  python -c "import my_pkg; print(\"wheel:\", my_pkg.__name__, my_pkg.__version__)"
//	  pip uninstall -y --root-user-action=ignore My-Pkg
//	  pip install --root-user-action=ignore --disable-pip-version-check /fixtures/pypi/my_pkg-1.0.0.tar.gz
//	  python -c "import my_pkg; print(\"sdist:\", my_pkg.__name__, my_pkg.__version__)"
//	  pip show My-Pkg'
//
// Output of docker run, shortened:
//
//	Processing /fixtures/pypi/my_pkg-1.0.0-py3-none-any.whl
//	Installing collected packages: my-pkg
//	Successfully installed my-pkg-1.0.0
//	wheel: my_pkg 1.0.0
//	Found existing installation: My-Pkg 1.0.0
//	  Successfully uninstalled My-Pkg-1.0.0
//	Processing /fixtures/pypi/my_pkg-1.0.0.tar.gz
//	  Installing build dependencies: finished with status 'done'
//	  Preparing metadata (pyproject.toml): finished with status 'done'
//	  Building wheel for My-Pkg (pyproject.toml): finished with status 'done'
//	  Created wheel for My-Pkg: filename=my_pkg-1.0.0-py3-none-any.whl size=1156 sha256=…
//	Successfully built My-Pkg
//	Successfully installed My-Pkg-1.0.0
//	sdist: my_pkg 1.0.0
//	Name: My-Pkg
//	Version: 1.0.0
//	Summary: A package built in process by internal/testutil.

package testutil

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"io"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestWheel(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opts       WheelOptions
		filename   string
		normalized string
		tags       []string
		purelib    string
	}{
		{
			name:       "defaults: a mixed-case name with a dash",
			filename:   "my_pkg-1.0.0-py3-none-any.whl",
			normalized: "my-pkg",
			tags:       []string{"py3-none-any"},
			purelib:    "true",
		},
		{
			name: "dots and underscores, a compressed tag set, requirements",
			opts: WheelOptions{
				Name:           "Foo.Bar__Baz",
				Version:        "2.0.post1",
				PythonTag:      "cp311",
				AbiTag:         "cp311",
				PlatformTag:    "manylinux_2_17_x86_64.manylinux2014_x86_64",
				RequiresPython: ">=3.11",
				RequiresDist:   []string{"requests>=2.31", `tomli; python_version < "3.11"`},
			},
			filename:   "foo_bar_baz-2.0.post1-cp311-cp311-manylinux_2_17_x86_64.manylinux2014_x86_64.whl",
			normalized: "foo-bar-baz",
			tags:       []string{"cp311-cp311-manylinux_2_17_x86_64", "cp311-cp311-manylinux2014_x86_64"},
			purelib:    "false",
		},
		{
			name:       "two python tags, a dash in the version",
			opts:       WheelOptions{Name: "a_b", Version: "1.0-1", PythonTag: "py2.py3"},
			filename:   "a_b-1.0_1-py2.py3-none-any.whl",
			normalized: "a-b",
			tags:       []string{"py2-none-any", "py3-none-any"},
			purelib:    "true",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, want := Wheel(tc.opts)
			if want.Filename != tc.filename || want.Normalized != tc.normalized {
				t.Errorf("Filename, Normalized = %q, %q, want %q, %q", want.Filename, want.Normalized, tc.filename, tc.normalized)
			}
			if !slices.Equal(want.Tags, tc.tags) {
				t.Errorf("Tags = %q, want %q", want.Tags, tc.tags)
			}
			if variant := want.PythonTag + "-" + want.AbiTag + "-" + want.PlatformTag; want.Variant != variant ||
				!strings.HasSuffix(want.Filename, "-"+variant+".whl") {
				t.Errorf("Variant = %q, file name %q, tags %s", want.Variant, want.Filename, variant)
			}
			files := readZip(t, b)
			if got, wantNames := zipNames(files), []string{
				want.Module + "/__init__.py",
				want.DistInfo + "/METADATA",
				want.DistInfo + "/WHEEL",
				want.DistInfo + "/RECORD",
			}; !slices.Equal(got, wantNames) {
				t.Fatalf("members %q, want %q", got, wantNames)
			}
			if name, _, _ := strings.Cut(want.Filename, "-"+want.Variant); want.DistInfo != name+".dist-info" {
				t.Errorf("DistInfo = %q", want.DistInfo)
			}
			checkMetadata(t, files[1].body, want)
			wheel := parseHeaders(t, files[2].body)
			if got := wheel["Tag"]; !slices.Equal(got, tc.tags) {
				t.Errorf("WHEEL Tag lines %q, want %q", got, tc.tags)
			}
			for name, value := range map[string]string{"Wheel-Version": "1.0", "Root-Is-Purelib": tc.purelib} {
				if got := wheel.Get(name); got != value {
					t.Errorf("WHEEL %s = %q, want %q", name, got, value)
				}
			}
			checkRecord(t, files)
			checkWhole(t, b, want.Size, want.Checksums)
		})
	}
}

// checkMetadata checks METADATA against want.
func checkMetadata(t *testing.T, b []byte, want WheelInfo) {
	t.Helper()
	if !bytes.Equal(b, want.Metadata) {
		t.Errorf("METADATA = %q, want Metadata %q", b, want.Metadata)
	}
	h := parseHeaders(t, b)
	for name, value := range map[string]string{
		"Metadata-Version": "2.1", "Name": want.Name, "Version": want.Version,
		"Summary": want.Summary, "Requires-Python": want.RequiresPython,
	} {
		if got := h.Get(name); got != value || got == "" {
			t.Errorf("METADATA %s = %q, want %q", name, got, value)
		}
	}
	if got := h["Requires-Dist"]; !slices.Equal(got, want.RequiresDist) {
		t.Errorf("METADATA Requires-Dist %q, want %q", got, want.RequiresDist)
	}
}

// checkRecord checks that RECORD, the last member, lists every member in order with the
// sha256 and the size of its bytes, and then itself with neither.
func checkRecord(t *testing.T, files []zipFile) {
	t.Helper()
	last := files[len(files)-1]
	rows, err := csv.NewReader(bytes.NewReader(last.body)).ReadAll()
	if err != nil {
		t.Fatalf("RECORD: %v", err)
	}
	if len(rows) != len(files) {
		t.Fatalf("RECORD has %d rows, the wheel %d members", len(rows), len(files))
	}
	for i, f := range files[:len(files)-1] {
		sum := sha256.Sum256(f.body)
		wantRow := []string{f.name, "sha256=" + base64.RawURLEncoding.EncodeToString(sum[:]), strconv.Itoa(len(f.body))}
		if !slices.Equal(rows[i], wantRow) {
			t.Errorf("RECORD row %d = %q, want %q", i, rows[i], wantRow)
		}
	}
	if got := rows[len(rows)-1]; !slices.Equal(got, []string{last.name, "", ""}) {
		t.Errorf("RECORD last row %q, want %q", got, []string{last.name, "", ""})
	}
}

func TestNormalizePyPI(t *testing.T) {
	// The equivalent names of PEP 503.
	for _, name := range []string{
		"friendly-bard", "Friendly-Bard", "FRIENDLY-BARD", "friendly.bard", "friendly_bard",
		"friendly--bard", "FrIeNdLy-._.-bArD",
	} {
		if got := normalizePyPI(name); got != "friendly-bard" {
			t.Errorf("normalizePyPI(%q) = %q, want friendly-bard", name, got)
		}
	}
}

func TestSdist(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     SdistOptions
		filename string
	}{
		{name: "defaults", filename: "my_pkg-1.0.0.tar.gz"},
		{name: "options", opts: SdistOptions{Name: "Zope.Interface", Version: "7.1", Summary: `say "hi" \ bye`}, filename: "zope_interface-7.1.tar.gz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, want := Sdist(tc.opts)
			if want.Filename != tc.filename || want.Filename != want.Root+".tar.gz" {
				t.Errorf("Filename, Root = %q, %q, want %q", want.Filename, want.Root, tc.filename)
			}
			files := readTarGz(t, b)
			r := want.Root + "/"
			if got, wantNames := tarNames(files), []string{
				r, r + "PKG-INFO", r + "pyproject.toml", r + want.Module + "/", r + want.Module + "/__init__.py",
			}; !slices.Equal(got, wantNames) {
				t.Fatalf("members %q, want %q", got, wantNames)
			}
			if !bytes.Equal(files[1].body, want.PKGInfo) {
				t.Errorf("PKG-INFO = %q, want PKGInfo %q", files[1].body, want.PKGInfo)
			}
			h := parseHeaders(t, files[1].body)
			for name, value := range map[string]string{
				"Metadata-Version": "2.1", "Name": want.Name, "Version": want.Version, "Summary": want.Summary,
			} {
				if got := h.Get(name); got != value || got == "" {
					t.Errorf("PKG-INFO %s = %q, want %q", name, got, value)
				}
			}
			for _, line := range []string{
				`build-backend = "setuptools.build_meta"`,
				"name = " + strconv.Quote(want.Name),
				"version = " + strconv.Quote(want.Version),
				"description = " + strconv.Quote(want.Summary),
			} {
				if !strings.Contains(string(files[2].body), line+"\n") {
					t.Errorf("pyproject.toml has no line %s:\n%s", line, files[2].body)
				}
			}
			checkWhole(t, b, want.Size, want.Checksums)
		})
	}
}

func TestPythonDeterministic(t *testing.T) {
	w1, _ := Wheel(WheelOptions{RequiresDist: []string{"a", "b"}})
	w2, _ := Wheel(WheelOptions{RequiresDist: []string{"a", "b"}})
	s1, _ := Sdist(SdistOptions{})
	s2, _ := Sdist(SdistOptions{})
	if !bytes.Equal(w1, w2) || !bytes.Equal(s1, s2) {
		t.Fatal("two calls with the same options give different bytes")
	}
	w3, _ := Wheel(WheelOptions{})
	if w4, _ := Wheel(WheelOptions{Version: "1.0.1"}); bytes.Equal(w3, w4) {
		t.Error("another version gives the same wheel")
	}
}

// zipFile is a member of a zip archive.
type zipFile struct {
	name string
	body []byte
}

// readZip reads a zip archive and checks that every member is dated fixtureTime.
func readZip(t *testing.T, b []byte) []zipFile {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	files := make([]zipFile, 0, len(zr.File))
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("zip: %s: %v", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("zip: %s: %v", f.Name, err)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("zip: %s: %v", f.Name, err)
		}
		if !f.Modified.Equal(fixtureTime) {
			t.Errorf("%s: modified %v, want %v", f.Name, f.Modified, fixtureTime)
		}
		files = append(files, zipFile{name: f.Name, body: body})
	}
	return files
}

// zipNames returns the names of the members.
func zipNames(files []zipFile) []string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.name
	}
	return names
}

// parseHeaders reads METADATA, PKG-INFO or WHEEL, which are RFC 822 headers, with net/mail.
func parseHeaders(t *testing.T, b []byte) mail.Header {
	t.Helper()
	m, err := mail.ReadMessage(io.MultiReader(bytes.NewReader(b), strings.NewReader("\n")))
	if err != nil {
		t.Fatalf("headers %q: %v", b, err)
	}
	return m.Header
}

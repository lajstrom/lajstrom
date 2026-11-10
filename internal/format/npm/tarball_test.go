package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// entry is a member of a tarball built by tgz: a name that ends with / is a directory, a link
// target makes a symbolic link.
type entry struct {
	name, body, link string
}

// tgz returns the entries as a gzipped tar archive.
func tgz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		switch {
		case strings.HasSuffix(e.name, "/"):
			h.Typeflag, h.Size = tar.TypeDir, 0
		case e.link != "":
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.link, 0
		}
		must(t, tw.WriteHeader(h))
		_, err := tw.Write([]byte(e.body))
		must(t, err)
	}
	must(t, tw.Close())
	must(t, zw.Close())
	return buf.Bytes()
}

const pkgJSON = `{"name": "widgets", "version": "1.0.0"}`

// TestReadTarball reads what testutil.NpmTarball writes: the top directory, package.json, and
// the shrinkwrap and binding.gyp that hasShrinkwrap and hasInstallScript come from.
func TestReadTarball(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		opts                  testutil.NpmOptions
		shrinkwrap, installer bool
	}{
		{"plain", testutil.NpmOptions{}, false, false},
		{"scoped", testutil.NpmOptions{Name: "@acme/widgets", Version: "2.0.0-rc.1"}, false, false},
		{"another top directory", testutil.NpmOptions{Root: "widgets"}, false, false},
		{"a shrinkwrap", testutil.NpmOptions{Shrinkwrap: true}, true, false},
		{"binding.gyp", testutil.NpmOptions{BindingGyp: true}, false, true},
		{"a postinstall script", testutil.NpmOptions{Scripts: map[string]string{"postinstall": "node x.js", "test": "x"}}, false, true},
		{"a test script only", testutil.NpmOptions{Scripts: map[string]string{"test": "x"}}, false, false},
	} {
		b, info := testutil.NpmTarball(tc.opts)
		tb, err := ReadTarball(bytes.NewReader(b))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if tb.Root != info.Root || tb.PackageJSON.Name != info.Name || tb.PackageJSON.Version != info.Version ||
			tb.HasShrinkwrap != tc.shrinkwrap || tb.HasInstallScript() != tc.installer {
			t.Errorf("%s: %+v, package.json %+v", tc.name, tb, tb.PackageJSON)
		}
	}
	tb, err := ReadTarball(bytes.NewReader(tgz(t, entry{name: "./package/"}, entry{name: "./package/package.json", body: pkgJSON},
		entry{name: "package/lib/package.json", body: "{}"}, entry{name: "package/link", link: "../../etc/passwd"})))
	if err != nil || tb.Root != "package" || tb.PackageJSON.Name != "widgets" {
		t.Errorf("./package/: %+v, %v", tb, err)
	}
}

// TestReadTarballRefuses is the malformed tarballs: each is format.ErrInvalid.
func TestReadTarballRefuses(t *testing.T) {
	pkg := entry{name: "package/package.json", body: pkgJSON}
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"not gzip", []byte("not gzip")},
		{"gzip of no tar", func() []byte {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			_, _ = zw.Write([]byte(strings.Repeat("x", 1024)))
			_ = zw.Close()
			return buf.Bytes()
		}()},
		{"empty", tgz(t)},
		{"no package.json", tgz(t, entry{name: "package/index.js", body: "x"})},
		{"package.json at the top", tgz(t, entry{name: "package.json", body: pkgJSON})},
		{"two top directories", tgz(t, pkg, entry{name: "other/index.js", body: "x"})},
		{"an absolute path", tgz(t, pkg, entry{name: "/etc/passwd", body: "x"})},
		{"a path out of the top", tgz(t, entry{name: "../package/package.json", body: pkgJSON})},
		{"two package.json", tgz(t, pkg, pkg)},
		{"PACKAGE.JSON besides", tgz(t, pkg, entry{name: "package/PACKAGE.JSON", body: pkgJSON})},
		{"Package.json in place", tgz(t, entry{name: "package/Package.json", body: pkgJSON})},
		{"a symbolic link", tgz(t, entry{name: "package/package.json", link: "../x"})},
		{"package.json not an object", tgz(t, entry{name: "package/package.json", body: `["name"]`})},
		{"package.json null", tgz(t, entry{name: "package/package.json", body: `null`})},
		{"package.json twice", tgz(t, entry{name: "package/package.json", body: pkgJSON + pkgJSON})},
		{"no name", tgz(t, entry{name: "package/package.json", body: `{"version": "1.0.0"}`})},
		{"a name that is no string", tgz(t, entry{name: "package/package.json", body: `{"name": 1, "version": "1.0.0"}`})},
		{"no version", tgz(t, entry{name: "package/package.json", body: `{"name": "x"}`})},
		{"package.json too large", tgz(t, entry{name: "package/package.json", body: strings.Repeat(" ", maxPackageJSON+1) + pkgJSON})},
	} {
		if _, err := ReadTarball(bytes.NewReader(tc.b)); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", tc.name, err)
		}
	}
}

// TestTarballBombs reads the bombs of #243 as npm tarballs: maxEntries+1 entries, and a GiB of
// zeros, a MiB of gzip, each refused at its bound; maxEntries-4 entries and a MiB of zeros are
// read.
func TestTarballBombs(t *testing.T) {
	if testing.Short() {
		t.Skip("reads a GiB")
	}
	for _, tc := range []struct {
		name    string
		entries int
		zeros   int64
		want    string
	}{
		{"entries", maxEntries + 1, 0, "more than 262144 entries"},
		{"zeros", 0, maxUnpacked, "unpacks to more than"},
		{"below the bounds", maxEntries - 4, 1 << 20, ""},
	} {
		b, _ := testutil.NpmBomb(testutil.NpmOptions{}, tc.entries, tc.zeros)
		_, err := ReadTarball(bytes.NewReader(b))
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (!errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

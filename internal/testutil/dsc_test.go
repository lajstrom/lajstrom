package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

// TestDsc checks the source packages Dsc builds: the name of the .dsc, its format, the files it
// names and their checksums in each of its three lists, the trees in the tarballs, the signed
// form, and that the same options give the same bytes.
func TestDsc(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     DscOptions
		filename string
		format   string
		files    []string
		trees    [][]string
	}{
		{"quilt", DscOptions{}, "hello_1.0.0-1.dsc", "3.0 (quilt)",
			[]string{"hello_1.0.0.orig.tar.gz", "hello_1.0.0-1.debian.tar.xz"},
			[][]string{{"hello-1.0.0/", "hello-1.0.0/README", "hello-1.0.0/src/hello.c"},
				{"debian/", "debian/changelog", "debian/control", "debian/source/", "debian/source/format"}}},
		{"epoch", DscOptions{Source: "libfoo", Version: "2:1.2-3ubuntu1"}, "libfoo_1.2-3ubuntu1.dsc", "3.0 (quilt)",
			[]string{"libfoo_1.2.orig.tar.gz", "libfoo_1.2-3ubuntu1.debian.tar.xz"}, nil},
		{"native", DscOptions{Version: "2.0", Files: map[string]string{"a": "a\n"}}, "hello_2.0.dsc", "3.0 (native)",
			[]string{"hello_2.0.tar.gz"},
			[][]string{{"hello-2.0/", "hello-2.0/a", "hello-2.0/debian/", "hello-2.0/debian/changelog", "hello-2.0/debian/control",
				"hello-2.0/debian/source/", "hello-2.0/debian/source/format"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Dsc(tc.opts)
			if d.Filename != tc.filename || d.Format != tc.format || d.Control["Format"] != tc.format {
				t.Errorf("Filename %q, Format %q, want %q and %q", d.Filename, d.Format, tc.filename, tc.format)
			}
			if names := checkListed(t, d); !slices.Equal(names, tc.files) {
				t.Errorf("files %v, want %v", names, tc.files)
			}
			for i, want := range tc.trees {
				if got := treeOf(t, d.Files[i]); !slices.Equal(got, want) {
					t.Errorf("the tree of %s: %v, want %v", d.Files[i].Name, got, want)
				}
			}
			if !bytes.Equal(d.Dsc, d.Paragraph) || d.Size != int64(len(d.Dsc)) || d.Checksums != checksumsOf(d.Dsc) {
				t.Error("an unsigned .dsc is its paragraph, with its size and checksums")
			}
			if again := Dsc(tc.opts); !bytes.Equal(again.Dsc, d.Dsc) || !bytes.Equal(again.Files[0].Content, d.Files[0].Content) {
				t.Error("the same options gave other bytes")
			}
		})
	}
}

// checkListed checks that each file of d has the size and checksums of its content and is
// listed with them in the three lists of the .dsc, and returns their names.
func checkListed(t *testing.T, d DscInfo) []string {
	t.Helper()
	var names []string
	for _, f := range d.Files {
		names = append(names, f.Name)
		if f.Size != int64(len(f.Content)) || f.Checksums != checksumsOf(f.Content) {
			t.Errorf("%s: size and checksums are not those of its content", f.Name)
		}
		for field, sum := range map[string]string{"Files": f.MD5, "Checksums-Sha1": f.SHA1, "Checksums-Sha256": f.SHA256} {
			if line := fmt.Sprintf("\n %s %d %s", sum, f.Size, f.Name); !strings.Contains(d.Control[field], line) {
				t.Errorf("%s does not list %q", field, line)
			}
		}
	}
	return names
}

// TestDscSigned: a signed .dsc is the paragraph in a clearsigned message, armor and signature
// block around it, its size and checksums those of the signed bytes.
func TestDscSigned(t *testing.T) {
	d := Dsc(DscOptions{Signed: true})
	s := string(d.Dsc)
	if !strings.HasPrefix(s, "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n\n"+string(d.Paragraph)+"-----BEGIN PGP SIGNATURE-----\n") ||
		!strings.HasSuffix(s, "-----END PGP SIGNATURE-----\n") {
		t.Errorf("the signed .dsc:\n%s", s)
	}
	if d.Checksums != checksumsOf(d.Dsc) || d.Size != int64(len(d.Dsc)) {
		t.Error("the size and checksums are not those of the signed bytes")
	}
	if got := clearsigned([]byte("A: b\n-x\n")); !bytes.Contains(got, []byte("\nA: b\n- -x\n")) {
		t.Errorf("a line that starts with - is not dash-escaped:\n%s", got)
	}
}

// treeOf returns the names in the tar archive of f, gzip or xz as its name says.
func treeOf(t *testing.T, f SourceFile) []string {
	t.Helper()
	var r io.Reader
	var err error
	switch {
	case strings.HasSuffix(f.Name, ".gz"):
		r, err = gzip.NewReader(bytes.NewReader(f.Content))
	case strings.HasSuffix(f.Name, ".xz"):
		r, err = xz.NewReader(bytes.NewReader(f.Content))
	default:
		t.Fatalf("%s: no compression", f.Name)
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

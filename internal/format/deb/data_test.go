package deb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestFileList: Parse reads the file list of a package from its data member in every compression
// testutil writes, the paths of its files and symbolic links without "./" in the order of the
// archive, its directories left out; the metadata layer has it as files, and Validate takes it.
func TestFileList(t *testing.T) {
	want := []string{"usr/bin/hello", "usr/bin/hi", "usr/share/doc/hello/README", "usr/share/hello/greeting"}
	for _, compression := range []string{"gz", "xz", "zst", "lzma", "none"} {
		t.Run(compression, func(t *testing.T) {
			deb, _ := testutil.Deb(testutil.DebOptions{DataCompression: compression,
				Files: map[string]string{"usr/bin/hello": "#!/bin/sh\n", "usr/share/doc/hello/README": "r\n", "usr/share/hello/greeting": "g\n"},
				Links: map[string]string{"usr/bin/hi": "hello"}})
			pkg, err := Parse(bytes.NewReader(deb))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(pkg.Files, want) {
				t.Errorf("Files = %q, want %q", pkg.Files, want)
			}
			md, err := pkg.Metadata()
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Files []string `json:"files"`
			}
			must(t, json.Unmarshal(md, &got))
			if !slices.Equal(got.Files, want) {
				t.Errorf("the metadata files = %q", got.Files)
			}
			b, err := Plugin{}.Build(t.Context(), ubuntu, upload([]format.UploadedFile{debFile(deb)}))
			must(t, err)
			if _, err := (Plugin{}).Validate(t.Context(), ubuntu, b.Manifest, fetcherOf(blobsOf(t, b), nil)); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

// dataDeb returns a .deb whose data member is name with the bytes data.
func dataDeb(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	deb, _ := testutil.Deb(testutil.DebOptions{})
	head := append([]byte{}, deb[:controlEnd(t, deb)]...)
	if len(head)%2 == 1 { // the padding byte of an odd control member
		head = append(head, '\n')
	}
	return append(head, arOf(arMember{name: name, data: data})[len(arMagic):]...)
}

// TestFileListGivesUp: a data member the lister cannot read within its bounds leaves the package
// without a file list and is no error: one that does not decompress, one past dataBase and
// dataRatio times its size, one whose paths are more than maxFileList bytes. Paths the list does
// not hold are left out of it.
func TestFileListGivesUp(t *testing.T) {
	var zeros bytes.Buffer
	zw := gzip.NewWriter(&zeros)
	tw := tar.NewWriter(zw)
	size := int64(dataBase + 16<<20) // compressed to about 80 KiB: past dataBase + dataRatio times that
	must(t, tw.WriteHeader(&tar.Header{Name: "./zeros", Mode: 0o644, Size: size, Typeflag: tar.TypeReg}))
	chunk := make([]byte, 1<<20)
	for n := int64(0); n < size; n += int64(len(chunk)) {
		_, err := tw.Write(chunk)
		must(t, err)
	}
	must(t, tw.Close())
	must(t, zw.Close())

	var long bytes.Buffer
	lw := tar.NewWriter(&long)
	name := "./" + strings.Repeat("d/", 1000) + "f"
	for i := range maxFileList/len(name) + 2 {
		must(t, lw.WriteHeader(&tar.Header{Name: name + string(rune('a'+i%26)) + strings.Repeat("x", i/26%8), Mode: 0o644, Typeflag: tar.TypeReg, Format: tar.FormatPAX}))
	}
	must(t, lw.Close())

	for _, tc := range []struct {
		name, member string
		data         []byte
	}{
		{"not gzip", "data.tar.gz", []byte("not gzip at all")},
		{"a bomb", "data.tar.gz", zeros.Bytes()},
		{"long paths", "data.tar", long.Bytes()},
		{"an lzma dictionary", "data.tar.lzma", append([]byte{0x5d, 0, 0, 0, 0x10}, make([]byte, 20)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkg, err := Parse(bytes.NewReader(dataDeb(t, tc.member, tc.data)))
			if err != nil {
				t.Fatal(err)
			}
			if pkg.Files != nil {
				t.Errorf("Files = %d paths, want none", len(pkg.Files))
			}
		})
	}
	t.Run("paths left out", func(t *testing.T) {
		var b bytes.Buffer
		w := tar.NewWriter(&b)
		for _, n := range []string{"./", "./ok", "/abs", "./a/../b", "./x\ny", "./bad\xff", "./dir/", "./c/./d"} {
			typ := byte(tar.TypeReg)
			if strings.HasSuffix(n, "/") {
				typ = tar.TypeDir
			}
			must(t, w.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Typeflag: typ, Format: tar.FormatPAX}))
		}
		must(t, w.Close())
		pkg, err := Parse(bytes.NewReader(dataDeb(t, "data.tar", b.Bytes())))
		must(t, err)
		if want := []string{"ok", "abs"}; !slices.Equal(pkg.Files, want) {
			t.Errorf("Files = %q, want %q", pkg.Files, want)
		}
	})
}

// TestValidateFiles: Validate refuses a file list that is not one of paths a list holds, and
// takes a list that disagrees with the package, which it does not read (the uploader's claim).
func TestValidateFiles(t *testing.T) {
	good := newFixture(t)
	for _, tc := range []struct {
		name  string
		files any
		want  string
	}{
		{"absolute", []string{"/usr/bin/x"}, `lists "/usr/bin/x" at 0`},
		{"dot dot", []string{"a", "a/../b"}, `lists "a/../b" at 1`},
		{"empty component", []string{"a//b"}, `lists "a//b"`},
		{"newline", []string{"a\nb"}, `lists "a\nb"`},
		{"not a list", "usr/bin/x", "not the deb metadata object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := good.withMetadata(t, func(md map[string]any) { md["files"] = tc.files })
			_, err := Plugin{}.Validate(t.Context(), ubuntu, f.m, fetcherOf(f.blobs, nil))
			if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate = %v, want ErrInvalid with %q", err, tc.want)
			}
		})
	}
	f := good.withMetadata(t, func(md map[string]any) { md["files"] = []string{"usr/bin/not-in-the-package"} })
	if _, err := (Plugin{}).Validate(t.Context(), ubuntu, f.m, fetcherOf(f.blobs, nil)); err != nil {
		t.Errorf("Validate of a list the package does not have: %v", err)
	}
}

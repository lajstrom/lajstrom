package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"io"
	"slices"
	"testing"
)

// TestNpmTarball checks what NpmTarball says of the tarball against its bytes: the same
// options give the same bytes, the members are below package/ with package.json first after the
// directory, the file name drops the scope, and Integrity is npm's sha512 of the whole file.
func TestNpmTarball(t *testing.T) {
	opts := NpmOptions{Name: "@acme/widgets", Version: "1.2.0", Shrinkwrap: true, BindingGyp: true, Bin: map[string]string{"widgets": "cli.js"}}
	b, info := NpmTarball(opts)
	if again, _ := NpmTarball(opts); !bytes.Equal(b, again) {
		t.Error("the same options gave other bytes")
	}
	sum := sha512.Sum512(b)
	if info.Filename != "widgets-1.2.0.tgz" || info.Root != "package" || info.Size != int64(len(b)) ||
		info.Integrity != "sha512-"+base64.StdEncoding.EncodeToString(sum[:]) || len(info.Shasum) != 40 {
		t.Errorf("info = %+v", info)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Name == "package/package.json" {
			if body, _ := io.ReadAll(tr); !bytes.Equal(body, info.PackageJSON) {
				t.Errorf("package.json = %s, info has %s", body, info.PackageJSON)
			}
		}
	}
	want := []string{"package/", "package/package.json", "package/index.js", "package/npm-shrinkwrap.json", "package/binding.gyp", "package/cli.js"}
	if !slices.Equal(names, want) {
		t.Errorf("members %v, want %v", names, want)
	}
}

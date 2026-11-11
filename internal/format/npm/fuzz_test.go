package npm

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// FuzzPackageJSON feeds the package.json reader a tar archive, which it gzips (stored blocks,
// which cost no time) and reads with ReadTarball, and the same bytes as a package.json, read with
// ParsePackageJSON; testdata/fuzz/FuzzPackageJSON has the archives of testutil.NpmTarball, which
// TestFuzzCorpus writes. Every error must be format.ErrInvalid, and what a read allocates must grow with the
// input. A tarball it accepts has a top directory and a package.json with a name and a version,
// and reads the same again; so does a package.json.
func FuzzPackageJSON(f *testing.F) {
	f.Add([]byte(pkgJSON))
	f.Add([]byte(`{"name": "@acme/widgets", "version": "1.0.0", "scripts": {"install": "node-gyp rebuild"}}`))
	for _, o := range []testutil.NpmOptions{{}, {Name: "@acme/widgets", Shrinkwrap: true, BindingGyp: true}} {
		b, _ := testutil.NpmTarball(o)
		f.Add(gunzip(f, b))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.NoCompression)
		_, _ = zw.Write(b)
		_ = zw.Close()
		bound := testutil.AllocSlack + 64<<10 + 16*len(b)
		checkReadTarball(t, buf.Bytes(), bound)
		var (
			p   *PackageJSON
			err error
		)
		testutil.CheckAllocated(t, fmt.Sprintf("ParsePackageJSON of %d bytes", len(b)), bound, func() { p, err = ParsePackageJSON(b) })
		switch {
		case err != nil && !errors.Is(err, format.ErrInvalid):
			t.Fatalf("ParsePackageJSON = %v, want ErrInvalid", err)
		case err == nil && (p.Name == "" || p.Version == "" || p.Fields["name"] == nil || p.Fields["version"] == nil):
			t.Fatalf("ParsePackageJSON = %+v", p)
		}
	})
}

// FuzzTarballInspector writes a tarball to the inspector of a publish, as ingest does while it
// streams (ADR 0014), in pieces of 1 to 256 bytes, and compares what it finds with readFacts of
// the whole, as Validate reads it from the registry: the same package.json, shrinkwrap,
// binding.gyp and paths, size, integrity and shasum, or an error both times, format.ErrInvalid,
// which is also that of a write the inspector refused. The input is a tar archive, which it
// gzips (stored blocks), as FuzzPackageJSON does; the seeds are the archives of
// testutil.NpmTarball, whole and cut short.
func FuzzTarballInspector(f *testing.F) {
	for _, o := range []testutil.NpmOptions{{}, {Name: "@acme/widgets", Shrinkwrap: true, BindingGyp: true,
		Scripts: map[string]string{"postinstall": "node x.js"}, Bin: map[string]string{"w": "bin/w.js"}}} {
		b, _ := testutil.NpmTarball(o)
		tb := gunzip(f, b)
		f.Add(uint8(99), tb)
		f.Add(uint8(6), tb[:len(tb)/2])
	}
	f.Fuzz(func(t *testing.T, piece uint8, b []byte) {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.NoCompression)
		_, _ = zw.Write(b)
		_ = zw.Close()
		gz := buf.Bytes()
		ti := newTarballInspector()
		var werr error
		for rest, n := gz, int(piece)+1; len(rest) > 0 && werr == nil; rest = rest[min(n, len(rest)):] {
			_, werr = ti.Write(rest[:min(n, len(rest))])
		}
		err := ti.Close()
		var got *facts
		if err == nil {
			got, err = ti.facts(digest.FromBytes(gz))
		}
		want, wantErr := readFacts(bytes.NewReader(gz))
		switch {
		case (err == nil) != (wantErr == nil) || err == nil && !reflect.DeepEqual(got, want):
			t.Fatalf("the inspector found %+v, %v; readFacts %+v, %v", got, err, want, wantErr)
		case err != nil && !errors.Is(err, format.ErrInvalid):
			t.Fatalf("the inspector: %v, want ErrInvalid", err)
		case werr != nil && !errors.Is(err, werr):
			t.Fatalf("a write failed with %v, Close with %v", werr, err)
		}
	})
}

// checkReadTarball reads the tarball b, allocating at most bound bytes: an error is ErrInvalid, a
// tarball read has a top directory and a package.json with a name and a version, and reads the
// same again.
func checkReadTarball(t *testing.T, b []byte, bound int) {
	t.Helper()
	var (
		tb  *Tarball
		err error
	)
	testutil.CheckAllocated(t, fmt.Sprintf("ReadTarball of %d bytes", len(b)), bound, func() { tb, err = ReadTarball(bytes.NewReader(b)) })
	switch {
	case err != nil && !errors.Is(err, format.ErrInvalid):
		t.Fatalf("ReadTarball = %v, want ErrInvalid", err)
	case err != nil:
		return
	case tb.Root == "" || tb.PackageJSON.Name == "" || tb.PackageJSON.Version == "":
		t.Fatalf("ReadTarball = %+v, package.json %+v", tb, tb.PackageJSON)
	}
	if again, err := ReadTarball(bytes.NewReader(b)); err != nil || !reflect.DeepEqual(tb, again) {
		t.Fatalf("ReadTarball again = %+v, %v; first %+v", again, err, tb)
	}
}

// gunzip returns the bytes gzipped in b.
func gunzip(tb testing.TB, b []byte) []byte {
	tb.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		tb.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		tb.Fatal(err)
	}
	return out
}

// TestFuzzCorpus writes testdata/fuzz/FuzzPackageJSON when LAJSTROM_FUZZ_CORPUS=write: the tar
// archives of testutil.NpmTarball, unscoped and scoped, with a shrinkwrap, binding.gyp, install
// scripts and a top directory of another name, and a package.json alone.
func TestFuzzCorpus(t *testing.T) {
	seeds := map[string]any{"package-json": []byte(pkgJSON)}
	for name, o := range map[string]testutil.NpmOptions{
		"default":   {},
		"scoped":    {Name: "@acme/widgets", Version: "2.0.0-rc.1", Dependencies: map[string]string{"lodash": "^4.17.21"}},
		"installer": {Shrinkwrap: true, BindingGyp: true, Scripts: map[string]string{"postinstall": "node setup.js"}},
		"bin":       {Root: "widgets", Bin: map[string]string{"widgets": "cli.js"}},
	} {
		b, _ := testutil.NpmTarball(o)
		seeds[name] = gunzip(t, b)
	}
	testutil.WriteFuzzCorpus(t, "FuzzPackageJSON", seeds)
}

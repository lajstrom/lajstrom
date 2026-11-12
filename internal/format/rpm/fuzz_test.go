package rpm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// fuzzSeeds are the packages the fuzz targets start from: testutil.RPM in several shapes, cut
// and corrupted.
func fuzzSeeds() map[string][]byte {
	seeds := map[string][]byte{}
	for name, opts := range map[string]testutil.RPMOptions{
		"default": {}, "source": {Source: true}, "epoch": {Epoch: "3"}, "plain": {NoDigests: true},
		"ghosts": {Ghosts: []string{"/var/log/x"}, Recommends: []testutil.RPMDep{{Name: "y", Flags: testutil.RPMSenseGreater, Version: "1:2-3"}}},
	} {
		b, info := testutil.RPM(opts)
		seeds[name] = b
		seeds[name+"-head"] = b[:info.HeaderEnd]
		seeds[name+"-cut"] = b[:info.HeaderStart+introLen+3]
		seeds[name+"-flipped"] = edit(b, func(c []byte) { c[info.HeaderStart+introLen+5] ^= 0x41 })
	}
	return seeds
}

// FuzzParse feeds Parse the bytes of an rpm: the packages of testutil.RPM, cut and corrupted.
// Run it locally with go test -fuzz=FuzzParse -fuzztime=30s ./internal/format/rpm/. A reader of
// bytes never fails, so every error must be format.ErrInvalid; a package Parse accepts must have
// the size and the sha256 of the bytes, names, versions and an arch of their grammars, a header
// range inside the file, and what ParseHead finds in the same bytes.
func FuzzParse(f *testing.F) {
	for _, b := range fuzzSeeds() {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		pkg, err := Parse(bytes.NewReader(b))
		if err != nil {
			if !errors.Is(err, format.ErrInvalid) {
				t.Fatalf("Parse = %v, want ErrInvalid", err)
			}
			return
		}
		checkParsed(t, b, pkg)
		head, err := ParseHead(bytes.NewReader(b), MaxHeader)
		if err != nil {
			t.Fatalf("ParseHead = %v of a package Parse accepts", err)
		}
		head.Size, head.SHA256 = pkg.Size, pkg.SHA256
		if diff := cmp.Diff(pkg, head, cmp.AllowUnexported(Package{})); diff != "" {
			t.Fatalf("ParseHead differs from Parse (-Parse +ParseHead):\n%s", diff)
		}
	})
}

// checkParsed checks what FuzzParse asks of a package that Parse accepted from b.
func checkParsed(t *testing.T, b []byte, pkg *Package) {
	t.Helper()
	sum := sha256.Sum256(b)
	switch {
	case pkg.Size != int64(len(b)) || pkg.SHA256 != hex.EncodeToString(sum[:]):
		t.Fatalf("Size %d, SHA256 %s of %d bytes", pkg.Size, pkg.SHA256, len(b))
	case !nameGrammar.MatchString(pkg.Name) || !verGrammar.MatchString(pkg.Version) || !verGrammar.MatchString(pkg.Release):
		t.Fatalf("name %q, version %q, release %q", pkg.Name, pkg.Version, pkg.Release)
	case !archGrammar.MatchString(pkg.Arch), pkg.Source != (pkg.Arch == "src" || pkg.Arch == "nosrc"):
		t.Fatalf("arch %q of a package whose Source is %t", pkg.Arch, pkg.Source)
	case pkg.HeaderStart < leadLen || pkg.HeaderEnd <= pkg.HeaderStart || pkg.HeaderEnd > pkg.Size:
		t.Fatalf("header range %d to %d in %d bytes", pkg.HeaderStart, pkg.HeaderEnd, pkg.Size)
	case pkg.Filename != Filename(pkg.Name, pkg.Version, pkg.Release, pkg.Arch):
		t.Fatalf("Filename %q", pkg.Filename)
	}
}

// FuzzInspector writes an rpm to the inspector of a streamed upload in pieces of step bytes, 1
// to 512, as a body arrives over the network, and checks that it says what Parse says of the
// whole, which writes it in pieces of 32 KiB: the same package, or the same error. Run it
// locally with go test -fuzz=FuzzInspector -fuzztime=30s ./internal/format/rpm/.
func FuzzInspector(f *testing.F) {
	for _, b := range fuzzSeeds() {
		for _, step := range []uint16{0, 15, 95, 111, 511} {
			f.Add(b, step)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte, step uint16) {
		want, wantErr := Parse(bytes.NewReader(b))
		in := &inspector{}
		var err error
		for off, k := 0, int(step%512)+1; off < len(b) && err == nil; off += k {
			_, err = in.Write(b[off:min(off+k, len(b))])
		}
		if err == nil {
			err = in.Close()
		}
		switch {
		case (err == nil) != (wantErr == nil), err != nil && err.Error() != wantErr.Error():
			t.Fatalf("in pieces of %d: %v; Parse: %v", int(step%512)+1, err, wantErr)
		case err == nil:
			got := *in.pkg
			got.Size, got.SHA256 = want.Size, want.SHA256
			if diff := cmp.Diff(want, &got, cmp.AllowUnexported(Package{})); diff != "" {
				t.Fatalf("in pieces of %d (-Parse +inspector):\n%s", int(step%512)+1, diff)
			}
		}
	})
}

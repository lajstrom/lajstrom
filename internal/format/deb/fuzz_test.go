package deb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// FuzzParse feeds Parse the bytes of a .deb: the packages of testutil.Deb for each control
// compression, a control file written by hand, and corrupted variants of them. Run it locally
// with go test -fuzz=FuzzParse -fuzztime=30s ./internal/format/deb/. A reader of bytes never
// fails, so every error must be format.ErrInvalid; a package Parse accepts must have the size
// and the sha256 of the bytes, names and versions that pass their grammars, the same control
// file through ParseControl, and a metadata layer whose JSON gives back the control fields and
// md5sums as they are (no byte lost to UTF-8 replacement).
func FuzzParse(f *testing.F) {
	for _, c := range []string{"gz", "xz", "zst", "none"} {
		b, _ := testutil.Deb(testutil.DebOptions{ControlCompression: c})
		f.Add(b)
		f.Add(b[:len(b)/2])
		flipped := append([]byte{}, b...)
		flipped[controlEnd(f, b)-40] ^= 0x55
		f.Add(flipped)
	}
	f.Add(debOf(f, handControl, ptr("d41d8cd98f00b204e9800998ecf8427e  usr/share/x\n"), ""))
	f.Add(debOf(f, minimal+"Source: hello-src (1:2.0-1)\n", nil, "gz"))
	f.Add([]byte(arMagic))
	f.Fuzz(func(t *testing.T, b []byte) {
		pkg, err := Parse(bytes.NewReader(b))
		if err != nil {
			if !errors.Is(err, format.ErrInvalid) {
				t.Fatalf("Parse = %v, want ErrInvalid", err)
			}
			return
		}
		checkParsed(t, b, pkg)
	})
}

// checkParsed checks what FuzzParse asks of a package that Parse accepted from b.
func checkParsed(t *testing.T, b []byte, pkg *Package) {
	t.Helper()
	sum := sha256.Sum256(b)
	if pkg.Size != int64(len(b)) || pkg.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("Size %d, SHA256 %s of %d bytes", pkg.Size, pkg.SHA256, len(b))
	}
	for _, err := range []error{
		checkName(FieldPackage, pkg.Name), checkName(FieldSource, pkg.Source),
		checkVersion(FieldVersion, pkg.Version), checkArchitecture(FieldArchitecture, pkg.Architecture),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	head, err := ParseControl(bytes.NewReader(b), MaxControl)
	if err != nil || !maps.Equal(head.Control.Map(), pkg.Control.Map()) || !maps.Equal(head.MD5Sums, pkg.MD5Sums) {
		t.Fatalf("ParseControl = %v; it disagrees with Parse", err)
	}
	md, err := pkg.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	var back metadata
	if err := json.Unmarshal(md, &back); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(back.Control, pkg.Control.Map()) || (len(pkg.MD5Sums) > 0 && !maps.Equal(back.MD5Sums, pkg.MD5Sums)) {
		t.Fatal("the metadata JSON does not give back the control fields and md5sums")
	}
}

// FuzzControl feeds parseControl a control file directly, which FuzzParse reaches only through
// a compressed archive. A control file it accepts, written back as "Name: value" lines, must
// parse to the same fields: the values keep everything but the whitespace the syntax ignores.
func FuzzControl(f *testing.F) {
	for _, s := range []string{minimal, handControl, "A: b\n c\n .\n\td\n", "\n\nX:\n y\n\n", "a:b\n#c: d\n", "A: 1\nB: 2\n\nC: 3\n"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := parseControl(b)
		if err != nil {
			if !errors.Is(err, format.ErrInvalid) {
				t.Fatalf("parseControl = %v, want ErrInvalid", err)
			}
			return
		}
		var out strings.Builder
		for _, f := range c.Fields {
			out.WriteString(f.Name + ": " + f.Value + "\n")
		}
		again, err := parseControl([]byte(out.String()))
		if err != nil {
			t.Fatalf("parseControl of %q written back: %v", out.String(), err)
		}
		if len(again.Fields) != len(c.Fields) {
			t.Fatalf("%d fields, written back %d", len(c.Fields), len(again.Fields))
		}
		for i := range c.Fields {
			if again.Fields[i] != c.Fields[i] {
				t.Fatalf("field %d: %q, written back %q", i, c.Fields[i], again.Fields[i])
			}
		}
	})
}

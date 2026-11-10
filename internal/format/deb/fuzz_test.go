package deb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
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
	blocks, _ := testutil.Deb(testutil.DebOptions{ControlCompression: "xz", XZEmptyBlocks: 2})
	f.Add(blocks)
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

// FuzzParseControl feeds parseControl a control file directly, which FuzzParse reaches only
// through a compressed archive; testdata/fuzz/FuzzParseControl has the control files of
// testutil.Deb, which TestFuzzCorpus writes. Every error must be format.ErrInvalid, and what
// parseControl allocates must grow with the file. Blank lines around the paragraph change
// nothing: a file refused stays refused, one accepted gives the same fields. A file it accepts
// has every name once, found by Get in any case, and values without the white space the syntax
// ignores; written back as "Name: value" lines, it parses to the same fields. In a .deb,
// ParseControl gives what packageOf gives for the file: the same package or the same error.
func FuzzParseControl(f *testing.F) {
	for _, s := range []string{minimal, handControl, "A: b\n c\n .\n\td\n", "\n\nX:\n y\n\n", "a:b\n#c: d\n",
		"A: 1\nB: 2\n\nC: 3\n", " x\n", "A: 1\na: 2\n", "A\x01: b\n", "\xff: b\n", "-A: b\n", "A b\n"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var (
			c   Control
			err error
		)
		testutil.CheckAllocated(t, fmt.Sprintf("parseControl of %d bytes", len(b)), controlAllocBound(len(b)),
			func() { c, err = parseControl(b) })
		checkControlInDeb(t, b)
		padded, perr := parseControl(slices.Concat([]byte("\n \t\n"), b, []byte("\n\n")))
		if err != nil {
			if !errors.Is(err, format.ErrInvalid) {
				t.Fatalf("parseControl = %v, want ErrInvalid", err)
			}
			if perr == nil {
				t.Fatalf("parseControl refused the file (%v) and accepted it with blank lines around", err)
			}
			return
		}
		if perr != nil || !slices.Equal(padded.Fields, c.Fields) {
			t.Fatalf("with blank lines around, parseControl = %v, %q; want %q", perr, padded.Fields, c.Fields)
		}
		checkControl(t, c)
		var out strings.Builder
		for _, f := range c.Fields {
			out.WriteString(f.Name + ": " + f.Value + "\n")
		}
		again, err := parseControl([]byte(out.String()))
		if err != nil || !slices.Equal(again.Fields, c.Fields) {
			t.Fatalf("parseControl of %q written back = %v, %q", out.String(), err, again.Fields)
		}
	})
}

// checkControl checks a control file parseControl accepted: Get and Map find every field, by its
// name in any case, and the values have none of the white space the syntax ignores.
func checkControl(t *testing.T, c Control) {
	t.Helper()
	if len(c.Map()) != len(c.Fields) {
		t.Fatalf("%d fields, %d names", len(c.Fields), len(c.Map()))
	}
	for _, f := range c.Fields {
		for _, name := range []string{f.Name, strings.ToLower(f.Name), strings.ToUpper(f.Name)} {
			if v, ok := c.Get(name); !ok || v != f.Value {
				t.Fatalf("Get(%q) = %q, %v; want %q", name, v, ok, f.Value)
			}
		}
		for i, line := range strings.Split(f.Value, "\n") {
			if strings.TrimRight(line, " \t") != line || i == 0 && strings.TrimLeft(line, " \t") != line ||
				i > 0 && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
				t.Fatalf("field %s: line %d of the value is %q", f.Name, i+1, line)
			}
		}
	}
}

// checkControlInDeb checks that ParseControl reads the control file b in a .deb as packageOf
// reads it alone: the same package, or the same error.
func checkControlInDeb(t *testing.T, b []byte) {
	t.Helper()
	deb := arOf(arMember{"debian-binary", []byte("2.0\n")},
		arMember{"control.tar", tarOf(t, tarEntry{name: "./control", body: string(b)})})
	got, err := ParseControl(bytes.NewReader(deb), MaxControl)
	want, wantErr := packageOf(b)
	if err != nil || wantErr != nil {
		if err == nil || wantErr == nil || err.Error() != wantErr.Error() {
			t.Fatalf("in a .deb, ParseControl = %v; packageOf = %v", err, wantErr)
		}
		return
	}
	want.ControlMember = "control.tar"
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("in a .deb, ParseControl = %+v; packageOf = %+v", got, want)
	}
}

// arEntry is a member of an ar archive as readAr reads it.
type arEntry struct {
	name string
	size int64
	data []byte
}

func (e arEntry) equal(o arEntry) bool {
	return e.name == o.name && e.size == o.size && bytes.Equal(e.data, o.data)
}

func (e arEntry) String() string {
	return fmt.Sprintf("%q (%d bytes, %d read)", e.name, e.size, len(e.data))
}

// readAr reads the members of the ar archive b with arReader and returns those b holds whole:
// a member cut short ends the archive with an error after the members before it.
func readAr(b []byte) ([]arEntry, error) {
	ar, err := newArReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	var members []arEntry
	for {
		h, body, err := ar.next()
		if errors.Is(err, io.EOF) {
			return members, nil
		}
		if err != nil {
			return members, err
		}
		data, _ := io.ReadAll(body) // a bytes.Reader does not fail
		if int64(len(data)) == h.size {
			members = append(members, arEntry{h.name, h.size, data})
		}
	}
}

// arAllocBound is the most readAr may allocate for an archive of n bytes, whatever sizes its
// headers claim: the data as read, and a reader, a buffer and an entry per member.
func arAllocBound(n int) int { return testutil.AllocSlack + 32*n }

// FuzzParseAr feeds arReader an ar archive; testdata/fuzz/FuzzParseAr has the packages of
// testutil.Deb, which TestFuzzCorpus writes. Every error must be format.ErrInvalid, and what a
// read allocates must grow with the bytes, not with the sizes the headers claim. An archive
// read to its end is the global header and the members, nothing else, and written again it
// reads to the same members. The reader streams: a prefix reads to a prefix of the members,
// all of them when it ends where a member ends, and a prefix refused for anything but where it
// ends stays refused, with the same error, whatever follows.
func FuzzParseAr(f *testing.F) {
	f.Add([]byte(arMagic))
	f.Add([]byte("!<arch>\r\n"))
	f.Add(arOf(arMember{"debian-binary", []byte("2.0\n")}, arMember{"odd/", []byte("x")}, arMember{"", nil},
		arMember{"név  /", []byte("é")}))
	huge := fmt.Appendf([]byte(arMagic), "%-16s%-12d%-6d%-6d%-8o%-10d`\n", "data.tar", 0, 0, 0, 0o644, 9999999999)
	f.Add(append(huge, "but a few bytes"...))
	f.Add(append([]byte(arMagic), "debian-binary   0           0     0     100644  4         `\r2.0\n"...))
	f.Add(append([]byte(arMagic), "x               0           0     0     100644  -1        `\n"...))
	f.Fuzz(func(t *testing.T, b []byte) {
		var (
			members []arEntry
			err     error
		)
		testutil.CheckAllocated(t, fmt.Sprintf("reading %d bytes", len(b)), arAllocBound(len(b)),
			func() { members, err = readAr(b) })
		if err != nil && !errors.Is(err, format.ErrInvalid) {
			t.Fatalf("readAr = %v, want ErrInvalid", err)
		}
		// cuts are prefixes to read: in the global header, in each header, at the end of each
		// member and a byte before it; ends maps the end of the global header and of each
		// member to the members up to it.
		cuts, ends := []int{len(arMagic) - 1, len(b) / 2, len(b) - 1}, map[int]int{}
		if bytes.HasPrefix(b, []byte(arMagic)) {
			ends[len(arMagic)] = 0
		}
		off := len(arMagic)
		for i, m := range members {
			cuts = append(cuts, off+arHeaderLen/2)
			off += arHeaderLen + int(m.size) + int(m.size%2)
			cuts, ends[off] = append(cuts, off-1, off), i+1
		}
		if err == nil {
			if off != len(b) {
				t.Fatalf("%d bytes, the members take %d", len(b), off)
			}
			again, err := readAr(arOf(rewrite(members)...))
			if err != nil || !slices.EqualFunc(again, members, arEntry.equal) {
				t.Fatalf("written again, the members read as %v, %v; first %v", err, again, members)
			}
		}
		for _, cut := range cuts {
			if cut >= 0 && cut < len(b) {
				checkArPrefix(t, b, cut, members, err, ends)
			}
		}
	})
}

// checkArPrefix checks what readAr makes of b[:cut], given the members and the error of b.
func checkArPrefix(t *testing.T, b []byte, cut int, members []arEntry, err error, ends map[int]int) {
	t.Helper()
	p, perr := readAr(b[:cut])
	if len(p) > len(members) || !slices.EqualFunc(p, members[:len(p)], arEntry.equal) {
		t.Fatalf("the first %d bytes read as %v, not the first members of %v", cut, p, members)
	}
	if k, ok := ends[cut]; ok && (perr != nil || len(p) != k) {
		t.Fatalf("the first %d bytes, where member %d ends, read as %d members, %v", cut, k, len(p), perr)
	}
	truncated := perr != nil && (cut < len(arMagic) || strings.Contains(perr.Error(), "truncated"))
	if perr != nil && !truncated && (err == nil || err.Error() != perr.Error()) {
		t.Fatalf("the first %d bytes are refused (%v), all of them read as %v", cut, perr, err)
	}
}

// rewrite returns the members to write again with arOf: a name that ends with "/" or a space
// had the "/" of GNU ar after it, which the reader takes off, so it gets one again.
func rewrite(members []arEntry) []arMember {
	out := make([]arMember, 0, len(members))
	for _, m := range members {
		name := m.name
		if strings.HasSuffix(name, "/") || strings.HasSuffix(name, " ") {
			name += "/"
		}
		out = append(out, arMember{name, m.data})
	}
	return out
}

// TestFuzzCorpus writes testdata/fuzz/FuzzParseControl and FuzzParseAr when
// LAJSTROM_FUZZ_CORPUS=write: the control files and the packages of testutil.Deb, a package with
// what deb(5) allows and dpkg-deb does not write, and a control file of many continuation lines,
// which cost the square of its size before #51.
func TestFuzzCorpus(t *testing.T) {
	controls := map[string]any{"hand-written": []byte(handControl),
		"continuation-lines": []byte(minimal + "X-Long: x" + strings.Repeat("\n y", 2048) + "\n")}
	debs := map[string]any{}
	for name, o := range map[string]testutil.DebOptions{
		"default": {},
		"epoch-all": {Name: "hello-doc", Version: "1:2.0~rc1-0ubuntu1", Arch: "all", Depends: "hello (= 1:2.0~rc1-0ubuntu1)",
			Files: map[string]string{"usr/share/doc/hello-doc/README": "read me\n", "usr/share/doc/hello-doc/a b.txt": "spaces\n"}},
		"long-description": {Name: "libhello1", Arch: "arm64", Description: "a library\nthat says hello\n\nin two paragraphs\n  and a verbatim line"},
	} {
		for _, c := range []string{"gz", "xz", "zst", "none"} {
			o.ControlCompression = c
			b, info := testutil.Deb(o)
			controls[name] = info.ControlFile
			if name == "default" || c == "xz" {
				debs[name+"-"+c] = b
			}
		}
	}
	b, _ := testutil.Deb(testutil.DebOptions{})
	members, err := readAr(b)
	if err != nil {
		t.Fatal(err)
	}
	gnu := append(rewrite(members), arMember{"_gpgorigin", []byte("a signature deb(5) says to ignore")})
	for i := range gnu[:len(members)] {
		gnu[i].name += "/"
	}
	debs["gnu-names-signature"] = arOf(gnu...)
	testutil.WriteFuzzCorpus(t, "FuzzParseControl", controls)
	testutil.WriteFuzzCorpus(t, "FuzzParseAr", debs)
}

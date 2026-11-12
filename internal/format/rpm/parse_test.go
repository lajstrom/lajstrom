package rpm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestParse parses the packages of testutil.RPM and compares what it finds with what the builder
// wrote: the names, the EVR and the arch of the repodata, the strings, the sizes, the place of the
// header, the relations with their flags and parts, the files with their types, the changelog,
// and the size and sha256 of the file.
func TestParse(t *testing.T) {
	weak := []testutil.RPMDep{{Name: "bash-completion"}}
	for name, opts := range map[string]testutil.RPMOptions{
		"default":   {},
		"epoch":     {Epoch: "2", Version: "3.1", Release: "4.fc41"},
		"epoch 0":   {Epoch: "0"},
		"source":    {Source: true},
		"noarch":    {Arch: "noarch", Name: "hello-data"},
		"no digest": {NoDigests: true},
		"lead v4":   {Major: 4},
		"ghosts":    {Ghosts: []string{"/var/log/hello.log"}, Dirs: []string{"/var/lib/hello", "/etc/hello"}},
		"weak":      {Recommends: weak, Suggests: weak, Supplements: weak, Enhances: weak},
		"others": {
			Conflicts: []testutil.RPMDep{{Name: "hello-old", Flags: testutil.RPMSenseLess, Version: "1:0.9"}},
			Obsoletes: []testutil.RPMDep{{Name: "hello-legacy", Flags: testutil.RPMSenseLess | testutil.RPMSenseEqual, Version: "0.8-2"}},
			Vendor:    "Lajstrom", Packager: "Tests <tests@lajstrom.dev>",
		},
		"no files": {Files: map[string]string{}, Dirs: []string{}},
		"changelog": {Changelogs: []testutil.RPMChangelog{
			{Time: 1767225600, Author: "B <b@x> - 1.0.0-2", Text: "- Second\n- More"},
			{Time: 1764547200, Author: "A <a@x> - 1.0.0-1", Text: "- First"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			b, info := testutil.RPM(opts)
			pkg, err := Parse(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(want(info), pkg, cmp.AllowUnexported(Package{})); diff != "" {
				t.Errorf("Parse (-want +got):\n%s", diff)
			}
			head, err := ParseHead(bytes.NewReader(b), MaxHeader)
			if err != nil {
				t.Fatal(err)
			}
			head.Size, head.SHA256 = pkg.Size, pkg.SHA256
			if diff := cmp.Diff(pkg, head, cmp.AllowUnexported(Package{})); diff != "" {
				t.Errorf("ParseHead differs from Parse (-Parse +ParseHead):\n%s", diff)
			}
		})
	}
}

// want returns the Package of the rpm testutil.RPM described with info, its unexported fields
// those Parse sets.
func want(info testutil.RPMInfo) *Package {
	pkg := &Package{
		Name: info.Name, Version: info.Version, Release: info.Release, Epoch: info.Epoch, Arch: info.Arch,
		Source: info.SourceRPM == "", Summary: info.Summary, Description: info.Description, Packager: info.Packager,
		URL: info.URL, License: info.License, Vendor: info.Vendor, Group: info.Group, BuildHost: info.BuildHost,
		SourceRPM: info.SourceRPM, BuildTime: info.BuildTime, InstalledSize: info.InstalledSize,
		ArchiveSize: info.ArchiveSize, HeaderStart: info.HeaderStart, HeaderEnd: info.HeaderEnd,
		Filename: info.Filename, Size: info.Size, SHA256: info.SHA256, Files: []File{}, Changelogs: []Changelog{},
	}
	for i, deps := range [][]testutil.RPMDep{info.Provides, info.Requires, info.Conflicts, info.Obsoletes,
		info.Recommends, info.Suggests, info.Supplements, info.Enhances} {
		pkg.Relations[i] = []Relation{}
		for _, d := range deps {
			e, v, r := SplitEVR(d.Version)
			pkg.Relations[i] = append(pkg.Relations[i], Relation{Name: d.Name, Flags: int64(d.Flags), Epoch: e, Version: v,
				Release: r, Pre: i == 1 && d.Flags&SensePre != 0})
		}
	}
	for _, f := range info.Files {
		pkg.Files = append(pkg.Files, File{Path: f.Path, Type: f.Type})
	}
	for _, c := range info.Changelogs {
		pkg.Changelogs = append(pkg.Changelogs, Changelog{Author: c.Author, Date: c.Time, Text: c.Text})
	}
	return withSigned(pkg, info)
}

// withSigned sets the unexported fields of pkg as Parse finds them in the rpm of info: the size
// of header and payload, and the payload digest, both absent from a package without digests.
func withSigned(pkg *Package, info testutil.RPMInfo) *Package {
	if info.PayloadSHA256 != "" {
		pkg.signed = info.HeaderEnd - info.HeaderStart + int64(len(info.Payload))
		pkg.payloadDigest = info.PayloadSHA256
	}
	return pkg
}

// TestParseText reads a string that is not UTF-8 as Latin-1, as old packages wrote it.
func TestParseText(t *testing.T) {
	b, _ := testutil.RPM(testutil.RPMOptions{Summary: "caf\xe9 au lait", Packager: "J\xfcrgen <j@x>"})
	pkg, err := Parse(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Summary != "café au lait" || pkg.Packager != "Jürgen <j@x>" {
		t.Errorf("Summary %q, Packager %q", pkg.Summary, pkg.Packager)
	}
}

// TestParseRefuses corrupts the packages of testutil.RPM, each in one way, and checks that
// Parse refuses each with an error that wraps format.ErrInvalid and says what is wrong, and
// that ParseHead does too for what lies in the head.
func TestParseRefuses(t *testing.T) {
	plain, info := testutil.RPM(testutil.RPMOptions{NoDigests: true})
	signed, sinfo := testutil.RPM(testutil.RPMOptions{})
	for _, c := range []struct {
		name string
		b    []byte
		want string
		head bool // ParseHead refuses it as well
	}{
		{"empty", nil, "fewer than the lead", true},
		{"lead magic", edit(plain, func(b []byte) { b[0] = 0 }), "lead magic", true},
		{"lead version", edit(plain, func(b []byte) { b[4] = 2 }), "rpm version 2", true},
		{"signature type", edit(plain, func(b []byte) { b[79] = 1 }), "signature type 1", true},
		{"signature magic", edit(plain, func(b []byte) { b[leadLen] = 0 }), "signature header does not start", true},
		{"header magic", edit(plain, func(b []byte) { b[info.HeaderStart+1] = 0 }), "the header does not start", true},
		{"header version", edit(plain, func(b []byte) { b[info.HeaderStart+3] = 3 }), "of version 3", true},
		{"no entries", edit(plain, func(b []byte) { binary.BigEndian.PutUint32(b[info.HeaderStart+8:], 0) }), "0 index entries", true},
		{"many entries", edit(plain, func(b []byte) { binary.BigEndian.PutUint32(b[leadLen+8:], maxEntries+1) }), "65536 index entries", true},
		{"cut in the signature", plain[:leadLen+40], "truncated: the signature header", true},
		{"cut in the header", plain[:info.HeaderStart+30], "truncated: the header", true},
		{"cut in the intro", plain[:info.HeaderStart+5], "of its intro", true},
		{"no NAME", retag(plain, info.HeaderStart, tagName, 1999), "no NAME", true},
		{"two VERSIONs", retag(plain, info.HeaderStart, tagRelease, tagVersion), "tag 1001 twice", true},
		{"type", setEntry(plain, info.HeaderStart, tagName, 4, 10), "type 10", true},
		{"count of a string", setEntry(plain, info.HeaderStart, tagName, 12, 2), "a count of 2", true},
		{"offset", setEntry(plain, info.HeaderStart, tagName, 8, 1<<30), "past the data store", true},
		{"no NUL", setEntry(plain, info.HeaderStart, tagName, 8, dataLen(plain, info.HeaderStart)-1), "has no end", true},
		{"numbers past the end", setEntry(plain, info.HeaderStart, tagBuildTime, 12, 1<<20), "past the end of the data store", true},
		{"strings past the end", setEntry(plain, info.HeaderStart, tagChangelogName, 12, 1<<20), "strings in", true},
		{"relation counts", setEntry(plain, info.HeaderStart, 1048, 12, 1), "3 requires names, 1 flags", true},
		{"NAME as a number", setEntry(plain, info.HeaderStart, tagName, 4, typeInt32), "not a string", true},
		{"header SHA256", edit(signed, func(b []byte) { b[sinfo.HeaderEnd-30] ^= 1 }), "the signature header says", true},
		{"short payload", signed[:len(signed)-1], "truncated: header and payload", false},
		{"long payload", append(append([]byte{}, signed...), 0), "the signature header says", false},
		{"payload digest", edit(signed, func(b []byte) { b[len(b)-3] ^= 1 }), "the payload has the sha256", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(bytes.NewReader(c.b))
			if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Parse = %v, want ErrInvalid with %q", err, c.want)
			}
			_, err = ParseHead(bytes.NewReader(c.b), MaxHeader)
			if c.head && (!errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), c.want)) {
				t.Errorf("ParseHead = %v, want ErrInvalid with %q", err, c.want)
			}
		})
	}
}

// TestParseGrammar refuses a name, a version, a release and an arch outside their grammars.
func TestParseGrammar(t *testing.T) {
	for _, c := range []struct {
		opts testutil.RPMOptions
		want string
	}{
		{testutil.RPMOptions{Name: "-hello"}, "NAME"},
		{testutil.RPMOptions{Name: "hello world"}, "NAME"},
		{testutil.RPMOptions{Version: "1.0-1"}, "VERSION"},
		{testutil.RPMOptions{Release: "1%{?dist}"}, "RELEASE"},
		{testutil.RPMOptions{Arch: "x86-64"}, "ARCH"},
	} {
		b, _ := testutil.RPM(c.opts)
		if _, err := Parse(bytes.NewReader(b)); !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: Parse = %v, want ErrInvalid naming %s", c.opts, err, c.want)
		}
	}
}

// TestParseHeadBound refuses a head over the limit before reading it, and reads no more than the
// head of a package within it.
func TestParseHeadBound(t *testing.T) {
	b, info := testutil.RPM(testutil.RPMOptions{Filler: 4096})
	if _, err := ParseHead(bytes.NewReader(b), info.HeaderEnd-1); !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "past the bound") {
		t.Errorf("ParseHead with a limit of %d = %v, want the bound", info.HeaderEnd-1, err)
	}
	r := &countingReader{r: bytes.NewReader(b)}
	if _, err := ParseHead(r, MaxHeader); err != nil {
		t.Fatal(err)
	}
	if r.n != info.HeaderEnd {
		t.Errorf("ParseHead read %d bytes, the head has %d", r.n, info.HeaderEnd)
	}
}

// TestParseDecodeBound refuses a header whose file names share a long directory so that the
// joined paths would take more than maxDecoded, before it joins them.
func TestParseDecodeBound(t *testing.T) {
	dir := "/" + strings.Repeat("d", 1<<16)
	files := map[string]string{}
	for i := range 1100 {
		files[dir+"/"+strings.Repeat("f", 1+i%7)+string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676))] = ""
	}
	b, _ := testutil.RPM(testutil.RPMOptions{Files: files, Dirs: []string{}})
	n := testutil.Allocated(func() {
		if _, err := Parse(bytes.NewReader(b)); !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "decodes to more than") {
			t.Errorf("Parse = %v, want the decode bound", err)
		}
	})
	if limit := 8*len(b) + testutil.AllocSlack; n > limit {
		t.Errorf("Parse of %d bytes allocated %d, at most %d", len(b), n, limit)
	}
}

// TestParseReadError returns the failure of the reader, not a malformed package.
func TestParseReadError(t *testing.T) {
	b, info := testutil.RPM(testutil.RPMOptions{})
	boom := errors.New("boom")
	for _, at := range []int64{10, info.HeaderStart + 20, info.HeaderEnd + 5} {
		r := io.MultiReader(bytes.NewReader(b[:at]), &failingReader{err: boom})
		if _, err := Parse(r); !errors.Is(err, boom) || errors.Is(err, format.ErrInvalid) {
			t.Errorf("at %d: Parse = %v, want boom", at, err)
		}
	}
}

// TestInspectorPieces writes a package to the inspector of a streamed upload in pieces of many
// sizes and checks that it finds what Parse finds.
func TestInspectorPieces(t *testing.T) {
	b, _ := testutil.RPM(testutil.RPMOptions{})
	whole, err := Parse(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []int{1, 7, 96, 111, 112, 113, 4096, len(b)} {
		in := &inspector{}
		for off := 0; off < len(b); off += step {
			if _, err := in.Write(b[off:min(off+step, len(b))]); err != nil {
				t.Fatalf("pieces of %d: %v", step, err)
			}
		}
		if err := in.Close(); err != nil || !in.done {
			t.Fatalf("pieces of %d: Close = %v", step, err)
		}
		got := *in.pkg
		got.Size, got.SHA256 = whole.Size, whole.SHA256
		if diff := cmp.Diff(whole, &got, cmp.AllowUnexported(Package{})); diff != "" {
			t.Errorf("pieces of %d (-Parse +inspector):\n%s", step, diff)
		}
	}
}

// TestEVR writes and splits versions as lajstrom.version and the relations do.
func TestEVR(t *testing.T) {
	for _, c := range []struct {
		epoch            int64
		version, release string
		evr              string
	}{
		{0, "1.0", "1", "1.0-1"},
		{1, "1.0", "1.el9", "1:1.0-1.el9"},
		{12, "2.0~rc1", "0.1", "12:2.0~rc1-0.1"},
	} {
		if got := EVR(c.epoch, c.version, c.release); got != c.evr {
			t.Errorf("EVR(%d, %q, %q) = %q, want %q", c.epoch, c.version, c.release, got, c.evr)
		}
	}
	for s, want := range map[string][3]string{
		"":             {"", "", ""},
		"3.0.4-1":      {"", "3.0.4", "1"},
		"1:2.0":        {"1", "2.0", ""},
		"1:2.0-3.el9":  {"1", "2.0", "3.el9"},
		"a:2.0-3":      {"", "a:2.0", "3"},
		":2.0":         {"", ":2.0", ""},
		"2:1.0-1-beta": {"2", "1.0-1", "beta"},
	} {
		e, v, r := SplitEVR(s)
		if got := [3]string{e, v, r}; got != want {
			t.Errorf("SplitEVR(%q) = %q, want %q", s, got, want)
		}
	}
	if got := Filename("hello", "2.12", "1.el9", "x86_64"); got != "hello-2.12-1.el9.x86_64.rpm" {
		t.Errorf("Filename = %q", got)
	}
}

// edit returns a copy of b changed by f.
func edit(b []byte, f func([]byte)) []byte {
	c := append([]byte{}, b...)
	f(c)
	return c
}

// entryOffset returns the offset in b of the index entry of tag in the header at start.
func entryOffset(b []byte, start int64, tag int32) int64 {
	il := int64(binary.BigEndian.Uint32(b[start+8:]))
	for i := range il {
		at := start + introLen + i*entryLen
		if int32(binary.BigEndian.Uint32(b[at:])) == tag { //nolint:gosec // a tag
			return at
		}
	}
	panic("no such tag")
}

// dataLen returns the size of the data store of the header at start.
func dataLen(b []byte, start int64) uint32 { return binary.BigEndian.Uint32(b[start+12:]) }

// retag returns a copy of b whose index entry of tag in the header at start has the tag to.
func retag(b []byte, start int64, tag, to int32) []byte {
	return setEntry(b, start, tag, 0, uint32(to)) //nolint:gosec // a tag
}

// setEntry returns a copy of b whose index entry of tag in the header at start has v at field,
// the byte offset in the entry: 0 the tag, 4 the type, 8 the offset, 12 the count.
func setEntry(b []byte, start int64, tag int32, field int64, v uint32) []byte {
	return edit(b, func(c []byte) { binary.BigEndian.PutUint32(c[entryOffset(c, start, tag)+field:], v) })
}

// countingReader counts what is read from r.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// failingReader fails every read with err.
type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }

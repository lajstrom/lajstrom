package deb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// fieldNames returns the names of fields in order.
func fieldNames(fs []Field) []string {
	names := make([]string, len(fs))
	for i, f := range fs {
		names[i] = f.Name
	}
	return names
}

// TestParse reads the packages of testutil.Deb, one per control compression and the cases of
// the issue (an epoch, arch all, a multi-line Description with " .", a lib* pool path), and
// compares every field of the result with what the builder says it wrote.
func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     testutil.DebOptions
		member   string
		synopsis string
		poolPath string
	}{
		{name: "gz", member: "control.tar.gz", synopsis: "greets the tests of the deb format",
			poolPath: "h/hello/hello_1.0.0-1_amd64.deb"},
		{name: "xz", opts: testutil.DebOptions{ControlCompression: "xz"}, member: "control.tar.xz",
			synopsis: "greets the tests of the deb format", poolPath: "h/hello/hello_1.0.0-1_amd64.deb"},
		{name: "zst", opts: testutil.DebOptions{ControlCompression: "zst"}, member: "control.tar.zst",
			synopsis: "greets the tests of the deb format", poolPath: "h/hello/hello_1.0.0-1_amd64.deb"},
		{name: "uncompressed", opts: testutil.DebOptions{ControlCompression: "none"}, member: "control.tar",
			synopsis: "greets the tests of the deb format", poolPath: "h/hello/hello_1.0.0-1_amd64.deb"},
		{
			name:     "epoch",
			opts:     testutil.DebOptions{Name: "nginx", Version: "1:1.18.0-6ubuntu14.4", ControlCompression: "xz"},
			member:   "control.tar.xz",
			synopsis: "greets the tests of the deb format",
			poolPath: "n/nginx/nginx_1.18.0-6ubuntu14.4_amd64.deb",
		},
		{
			name:     "all, lib, multi-line description",
			opts:     testutil.DebOptions{Name: "libfoo2", Version: "2.1~rc1-3", Arch: "all", Description: "the foo library\none\n\n\nfour"},
			member:   "control.tar.gz",
			synopsis: "the foo library",
			poolPath: "libf/libfoo2/libfoo2_2.1~rc1-3_all.deb",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, want := testutil.Deb(tc.opts)
			pkg, err := Parse(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{
				"Name": pkg.Name, "Version": pkg.Version, "Architecture": pkg.Architecture, "Maintainer": pkg.Maintainer,
				"Description": pkg.Description, "Source": pkg.Source, "SourceVersion": pkg.SourceVersion,
				"ControlMember": pkg.ControlMember, "Filename": pkg.Filename, "PoolPath": pkg.PoolPath,
				"MD5": pkg.MD5, "SHA1": pkg.SHA1, "SHA256": pkg.SHA256,
			}
			wantFields := map[string]string{
				"Name": want.Package, "Version": want.Version, "Architecture": want.Architecture, "Maintainer": want.Maintainer,
				"Description": tc.synopsis, "Source": want.Package, "SourceVersion": "",
				"ControlMember": tc.member, "Filename": want.Filename, "PoolPath": tc.poolPath,
				"MD5": want.MD5, "SHA1": want.SHA1, "SHA256": want.SHA256,
			}
			if d := cmp.Diff(wantFields, got); d != "" {
				t.Errorf("Parse (-want +got):\n%s", d)
			}
			if pkg.Size != want.Size {
				t.Errorf("Size = %d, want %d", pkg.Size, want.Size)
			}
			if d := cmp.Diff(want.Control, pkg.Control.Map()); d != "" {
				t.Errorf("Control (-want +got):\n%s", d)
			}
			order := []string{"Package", "Version", "Architecture", "Maintainer", "Installed-Size", "Depends", "Section", "Priority", "Description"}
			if got := fieldNames(pkg.Control.Fields); !slices.Equal(got, order) {
				t.Errorf("field order %q, want %q", got, order)
			}
			if !maps.Equal(pkg.MD5Sums, want.MD5Sums) {
				t.Errorf("MD5Sums = %v, want %v", pkg.MD5Sums, want.MD5Sums)
			}
			if d, _ := pkg.Control.Get("description"); d != want.Description {
				t.Errorf("Get(description) = %q, want %q", d, want.Description)
			}
		})
	}
}

// handControl is a control file written by hand: an unusual field order, a Description with a
// tab continuation line and a " .", lowercase and custom field names, a Source with a version,
// trailing whitespace, and blank lines around the paragraph.
const handControl = "\n" +
	"Description: a control file written by hand  \n" +
	"\tthe tab starts this continuation line\n" +
	" .\n" +
	" and a space this one\n" +
	"architecture: arm64\n" +
	"Source: hello-src (2.0-1)\n" +
	"Version:2.0-1+b1\n" +
	"Maintainer: Lajstrom Tests <tests@lajstrom.dev>\n" +
	"X-Custom: kept: as written\n" +
	"Package: hello-bin\n" +
	"\n"

// TestParseHandWritten checks the control syntax of Debian Policy §5.1 on a control file
// written by hand, a Source with a version, and md5sums in md5sum's binary mode and in
// uppercase.
func TestParseHandWritten(t *testing.T) {
	pkg, err := Parse(bytes.NewReader(debOf(t, handControl, ptr("0123456789ABCDEF0123456789abcdef *usr/share/x\n\n"), "xz")))
	if err != nil {
		t.Fatal(err)
	}
	wantFields := []Field{
		{"Description", "a control file written by hand\n\tthe tab starts this continuation line\n .\n and a space this one"},
		{"architecture", "arm64"},
		{"Source", "hello-src (2.0-1)"},
		{"Version", "2.0-1+b1"},
		{"Maintainer", "Lajstrom Tests <tests@lajstrom.dev>"},
		{"X-Custom", "kept: as written"},
		{"Package", "hello-bin"},
	}
	if d := cmp.Diff(wantFields, pkg.Control.Fields); d != "" {
		t.Errorf("Fields (-want +got):\n%s", d)
	}
	for _, c := range [][2]string{
		{pkg.Name, "hello-bin"}, {pkg.Version, "2.0-1+b1"}, {pkg.Architecture, "arm64"},
		{pkg.Description, "a control file written by hand"}, {pkg.Source, "hello-src"}, {pkg.SourceVersion, "2.0-1"},
		{pkg.Filename, "hello-bin_2.0-1+b1_arm64.deb"}, {pkg.PoolPath, "h/hello-src/hello-bin_2.0-1+b1_arm64.deb"},
	} {
		if c[0] != c[1] {
			t.Errorf("got %q, want %q", c[0], c[1])
		}
	}
	if want := map[string]string{"usr/share/x": "0123456789abcdef0123456789abcdef"}; !maps.Equal(pkg.MD5Sums, want) {
		t.Errorf("MD5Sums = %v, want %v", pkg.MD5Sums, want)
	}
}

// TestParseSourceNoMD5Sums checks a Source without a version, which names the pool directory,
// and a package without md5sums.
func TestParseSourceNoMD5Sums(t *testing.T) {
	control := "Package: libc6\nSource: glibc\nVersion: 2.39-0ubuntu8\nArchitecture: amd64\nMaintainer: m\nDescription: d\n"
	pkg, err := Parse(bytes.NewReader(debOf(t, control, nil, "zst")))
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Source != "glibc" || pkg.SourceVersion != "" || pkg.PoolPath != "g/glibc/libc6_2.39-0ubuntu8_amd64.deb" || pkg.MD5Sums != nil {
		t.Errorf("Source %q (%q), PoolPath %q, MD5Sums %v", pkg.Source, pkg.SourceVersion, pkg.PoolPath, pkg.MD5Sums)
	}
}

// TestParseLenient checks what deb(5) allows and dpkg-deb does not write: ar names with the "/"
// of GNU ar, more lines in debian-binary, and a member after data.tar, which is ignored.
func TestParseLenient(t *testing.T) {
	b, want := testutil.Deb(testutil.DebOptions{})
	var members []arMember
	ar, err := newArReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	for {
		h, body, err := ar.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(body)
		if h.name == "debian-binary" {
			data = []byte("2.0\na later minor version may add lines\n")
		}
		members = append(members, arMember{h.name + "/", data})
	}
	members = append(members, arMember{"_gpgorigin", []byte("a signature deb(5) says to ignore")})
	odd := arOf(members...)
	pkg, err := Parse(bytes.NewReader(odd))
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Name != want.Package || pkg.Size != int64(len(odd)) || pkg.ControlMember != "control.tar.gz" {
		t.Errorf("Parse = %q, %d bytes, %s", pkg.Name, pkg.Size, pkg.ControlMember)
	}
}

// TestParseCorrupted checks that a broken archive, a member out of order and a control member
// that is not one are format.ErrInvalid with a message that says what is wrong.
func TestParseCorrupted(t *testing.T) {
	good, _ := testutil.Deb(testutil.DebOptions{})
	end := controlEnd(t, good)
	member := func(name string, data []byte) []byte {
		return arOf(arMember{"debian-binary", []byte("2.0\n")}, arMember{name, data})
	}
	controlTar := func(entries ...tarEntry) []byte {
		return arOf(arMember{"debian-binary", []byte("2.0\n")}, arMember{"control.tar", tarOf(t, entries...)},
			arMember{"data.tar", nil})
	}
	const control = "Package: a1\nVersion: 1\nArchitecture: all\nMaintainer: m\nDescription: d\n"
	badHeader := append([]byte{}, good...)
	copy(badHeader[len(arMagic)+58:], "x\n")
	badSize := append([]byte{}, good...)
	copy(badSize[len(arMagic)+48:], "4a        ")
	// A header that announces one byte more than MaxControl; the data is not even there.
	overLimit := fmt.Appendf(arOf(arMember{"debian-binary", []byte("2.0\n")}), "%-16s%-12d%-6d%-6d%-8o%-10d`\n",
		"control.tar.gz", 0, 0, 0, 0o100644, MaxControl+1)
	for _, tc := range []struct {
		name string
		b    []byte
		want string
	}{
		{"empty", nil, "not an ar archive: 0 bytes"},
		{"wrong magic", []byte("GIF89a, not an archive"), `not an ar archive: it starts with "GIF89a, "`},
		{"truncated global header", []byte("!<ar"), "not an ar archive: 4 bytes"},
		{"no members", []byte(arMagic), "the archive has no members"},
		{"truncated header", good[:len(arMagic)+30], "truncated archive: the header of member 1 has 30 of 60 bytes"},
		{"header end", badHeader, "member 1: the header ends with"},
		{"header size", badSize, `size "4a" is not a decimal number`},
		{"debian-binary not first", arOf(arMember{"control.tar.gz", nil}, arMember{"debian-binary", []byte("2.0\n")}),
			`the first member is "control.tar.gz", not debian-binary`},
		{"debian-binary 3.0", arOf(arMember{"debian-binary", []byte("3.0\n")}), `debian-binary says "3.0", not 2.0`},
		{"debian-binary 2.1", arOf(arMember{"debian-binary", []byte("2.1\n")}), `debian-binary says "2.1", not 2.0`},
		{"debian-binary too long", arOf(arMember{"debian-binary", bytes.Repeat([]byte("2"), 2000)}), "debian-binary has 2000 bytes"},
		{"truncated debian-binary", good[:len(arMagic)+62], "truncated archive: debian-binary has 2 of 4 bytes"},
		{"no control member", arOf(arMember{"debian-binary", []byte("2.0\n")}), "ends after debian-binary, before the control member"},
		{"data before control", member("data.tar.xz", nil), `the second member is "data.tar.xz", not control.tar`},
		{"unknown control compression", member("control.tar.bz2", []byte("BZh9")), `the second member is "control.tar.bz2"`},
		{"control member over the limit", overLimit, "the control member control.tar.gz has 4194305 bytes, at most 4194304"},
		{"truncated control member", good[:end-10], "truncated archive: member 2 lacks 10 bytes"},
		{"gzip garbage", member("control.tar.gz", []byte("not gzip at all")), "control.tar.gz: gzip: invalid header"},
		{"xz garbage", member("control.tar.xz", []byte("not xz at all, not xz")), "control.tar.xz: xz: not an xz stream"},
		{"zst garbage", member("control.tar.zst", []byte("not zstd at all")), "control.tar.zst"},
		{"tar garbage", member("control.tar", bytes.Repeat([]byte("garbage!"), 64)), "control.tar: archive/tar"},
		{"no control file", controlTar(tarEntry{name: "./md5sums", body: ""}), "control.tar has no control file"},
		{"control twice", controlTar(tarEntry{name: "./control", body: control}, tarEntry{name: "control", body: control}),
			"control.tar holds control twice"},
		{"control a symlink", controlTar(tarEntry{name: "./control", body: "/etc/passwd", typ: '2'}), "./control is not a regular file"},
		{"no data member", good[:end], "the archive ends after control.tar.gz, before the data member"},
		{"truncated data header", good[:end+30], "truncated archive: the header of member 3 has 30 of 60 bytes"},
		{"no padding", arOf(arMember{"debian-binary", []byte("2.0\n\n")})[:len(arMagic)+65], "truncated archive: member 1 lacks its padding byte"},
		{"truncated control file", member("control.tar", tarOf(t, tarEntry{name: "./control", body: control})[:520]),
			"control.tar: ./control: unexpected EOF"},
		{"third member not data", append(good[:end:end], arOf(arMember{"foo.tar", nil})[len(arMagic):]...),
			`the third member is "foo.tar"`},
		{"truncated data member", good[:len(good)-5], "truncated archive: member 3 lacks"},
		{"duplicated field", debOf(t, control+"package: a2\n", nil, "gz"), "field package is given twice, first as Package"},
		{"bad package", debOf(t, strings.Replace(control, "a1", "A1", 1), nil, "gz"), `Package "A1" is not a package name`},
		{"bad version", debOf(t, strings.Replace(control, "Version: 1", "Version: 1.0-", 1), nil, "gz"), `Version "1.0-" ends with "-"`},
		{"bad architecture", debOf(t, strings.Replace(control, "all", "x86_64", 1), nil, "gz"), `Architecture "x86_64" is not an architecture name`},
		{"bad md5sums", debOf(t, control, ptr("d41d8cd98f00b204e9800998ecf8427e usr/x\n"), "gz"), `md5sums line 1 is not "<md5>  <path>"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkg, err := Parse(bytes.NewReader(tc.b))
			if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse = %+v, %v; want ErrInvalid containing %q", pkg, err, tc.want)
			}
		})
	}
}

// TestParseControlStops checks that ParseControl reads nothing after the control member: on a
// reader that fails right after it, ParseControl succeeds, and Parse reports the failure as a
// failure to read, not as a broken package.
func TestParseControlStops(t *testing.T) {
	for _, c := range []string{"gz", "xz", "zst", "none"} {
		b, want := testutil.Deb(testutil.DebOptions{ControlCompression: c})
		end := controlEnd(t, b)
		pkg, err := ParseControl(failAfter(b[:end]), MaxControl)
		if err != nil {
			t.Fatalf("%s: ParseControl = %v", c, err)
		}
		if !maps.Equal(pkg.Control.Map(), want.Control) || !maps.Equal(pkg.MD5Sums, want.MD5Sums) || pkg.Size != 0 || pkg.SHA256 != "" {
			t.Errorf("%s: ParseControl = %+v", c, pkg)
		}
		if _, err := Parse(failAfter(b[:end])); !errors.Is(err, errBoom) || errors.Is(err, format.ErrInvalid) {
			t.Errorf("%s: Parse of a failing reader = %v, want errBoom and not ErrInvalid", c, err)
		}
	}
	// Without md5sums the control archive is read to its end, still not further.
	b := debOf(t, "Package: a1\nVersion: 1\nArchitecture: all\nMaintainer: m\nDescription: d\n", nil, "gz")
	if _, err := ParseControl(failAfter(b[:controlEnd(t, b)]), MaxControl); err != nil {
		t.Errorf("ParseControl without md5sums = %v", err)
	}
	// A failure before the end of the control member is a failure to read.
	if _, err := ParseControl(failAfter(b[:100]), MaxControl); !errors.Is(err, errBoom) || errors.Is(err, format.ErrInvalid) {
		t.Errorf("ParseControl of a reader failing in debian-binary = %v, want errBoom", err)
	}
	if _, err := ParseControl(failAfter(b[:4]), MaxControl); !errors.Is(err, errBoom) {
		t.Errorf("ParseControl of a reader failing in the global header = %v, want errBoom", err)
	}
}

// TestParseControlLimit checks both bounds of limit: the control member as stored, and the
// control archive uncompressed.
func TestParseControlLimit(t *testing.T) {
	b, _ := testutil.Deb(testutil.DebOptions{})
	if _, err := ParseControl(bytes.NewReader(b), 100); !errors.Is(err, format.ErrInvalid) ||
		!strings.Contains(err.Error(), "the control member control.tar.gz has") {
		t.Errorf("ParseControl with a 100-byte limit = %v", err)
	}
	// 64 KiB of zeros in a maintainer script compress to almost nothing.
	bomb := arOf(arMember{"debian-binary", []byte("2.0\n")}, arMember{"control.tar.gz", compress(t, "gz", tarOf(t,
		tarEntry{name: "./postinst", body: strings.Repeat("\x00", 64<<10)},
		tarEntry{name: "./control", body: "Package: a1\n"}))})
	if _, err := ParseControl(bytes.NewReader(bomb), 32<<10); !errors.Is(err, format.ErrInvalid) ||
		!strings.Contains(err.Error(), "control.tar.gz has more than 32768 bytes uncompressed") {
		t.Errorf("ParseControl of 64 KiB uncompressed with a 32 KiB limit = %v", err)
	}
}

// TestParseReadError checks that a failure of the reader is returned wrapped and is not
// format.ErrInvalid.
func TestParseReadError(t *testing.T) {
	b, _ := testutil.Deb(testutil.DebOptions{})
	for _, n := range []int{0, 7, 70, 200, len(b) - 3} {
		if _, err := Parse(failAfter(b[:n])); !errors.Is(err, errBoom) || errors.Is(err, format.ErrInvalid) {
			t.Errorf("Parse failing after %d bytes = %v, want errBoom and not ErrInvalid", n, err)
		}
	}
}

func TestFilenameAndPoolPath(t *testing.T) {
	for _, tc := range []struct{ name, version, arch, source, file, pool string }{
		{"nginx", "1:1.18.0-6ubuntu14.4", "amd64", "nginx", "nginx_1.18.0-6ubuntu14.4_amd64.deb", "n/nginx/"},
		{"libc6", "2.39-0ubuntu8", "arm64", "glibc", "libc6_2.39-0ubuntu8_arm64.deb", "g/glibc/"},
		{"libssl3t64", "3.0.13-0ubuntu3", "amd64", "openssl", "libssl3t64_3.0.13-0ubuntu3_amd64.deb", "o/openssl/"},
		{"libfoo2", "10:2.1~rc1", "all", "libfoo", "libfoo2_2.1~rc1_all.deb", "libf/libfoo/"},
		{"lib", "1", "all", "lib", "lib_1_all.deb", "lib/lib/"},
		{"li", "1", "all", "li", "li_1_all.deb", "l/li/"},
		{"0ad", "0.0.26-3", "amd64", "0ad", "0ad_0.0.26-3_amd64.deb", "0/0ad/"},
	} {
		file := Filename(tc.name, tc.version, tc.arch)
		if file != tc.file {
			t.Errorf("Filename(%q, %q, %q) = %q, want %q", tc.name, tc.version, tc.arch, file, tc.file)
		}
		if got := PoolPath(tc.source, file); got != tc.pool+tc.file {
			t.Errorf("PoolPath(%q) = %q, want %q", tc.source, got, tc.pool+tc.file)
		}
	}
}

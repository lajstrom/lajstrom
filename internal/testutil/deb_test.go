// Checked once with dpkg in a container on 2026-11-07, not in CI (#22). From the repository
// root, with the fixtures of TestWriteFixtures:
//
//	export LAJSTROM_FIXTURES_OUT=$(mktemp -d)
//	go test -count=1 -run TestWriteFixtures ./internal/testutil
//	docker run --rm --name deb-22 -v "$LAJSTROM_FIXTURES_OUT:/fixtures:ro" ubuntu:24.04 sh -ec '
//	  d=/fixtures/deb/hello_1.0.0-1_amd64.deb
//	  dpkg-deb --info $d
//	  dpkg-deb --contents $d
//	  mkdir /x && dpkg-deb -e $d /x/DEBIAN && dpkg-deb -x $d /x
//	  (cd /x && md5sum -c DEBIAN/md5sums)
//	  rm /etc/dpkg/dpkg.cfg.d/excludes  # the image leaves out /usr/share/doc, which --verify reports
//	  dpkg -i /fixtures/deb/hello_1.0.0-1_all.deb >/dev/null
//	  dpkg --verify hello && echo "dpkg --verify hello: ok"'
//
// Output of docker run:
//
//	 new Debian package, version 2.0.
//	 size 950 bytes: control archive=470 bytes.
//	     319 bytes,    12 lines      control
//	     120 bytes,     2 lines      md5sums
//	 Package: hello
//	 Version: 1.0.0-1
//	 Architecture: amd64
//	 Maintainer: Lajstrom Tests <tests@lajstrom.dev>
//	 Installed-Size: 2
//	 Depends: libc6 (>= 2.34)
//	 Section: misc
//	 Priority: optional
//	 Description: greets the tests of the deb format
//	  A package built in process by internal/testutil.
//	  .
//	  Nothing in it comes from a distribution.
//	drwxr-xr-x root/root         0 2026-01-01 00:00 ./
//	drwxr-xr-x root/root         0 2026-01-01 00:00 ./usr/
//	drwxr-xr-x root/root         0 2026-01-01 00:00 ./usr/share/
//	drwxr-xr-x root/root         0 2026-01-01 00:00 ./usr/share/doc/
//	drwxr-xr-x root/root         0 2026-01-01 00:00 ./usr/share/doc/hello/
//	-rw-r--r-- root/root        42 2026-01-01 00:00 ./usr/share/doc/hello/README
//	drwxr-xr-x root/root         0 2026-01-01 00:00 ./usr/share/hello/
//	-rw-r--r-- root/root        13 2026-01-01 00:00 ./usr/share/hello/greeting
//	usr/share/doc/hello/README: OK
//	usr/share/hello/greeting: OK
//	dpkg --verify hello: ok
//
// ControlCompression "xz" and "zst" were checked the same way on 2026-11-07 (#39), dpkg-deb
// 1.22.6, the container deb-39 removed after:
//
//	docker run --rm --name deb-39 -v "$LAJSTROM_FIXTURES_OUT:/fixtures:ro" ubuntu:24.04 sh -ec '
//	  for c in xz zst; do
//	    d=/fixtures/deb/$c/hello_1.0.0-1_amd64.deb
//	    echo "== $c"
//	    dpkg-deb --info $d | head -4
//	    dpkg-deb --ctrl-tarfile $d | tar -t
//	    mkdir /x-$c && dpkg-deb -e $d /x-$c/DEBIAN && dpkg-deb -x $d /x-$c
//	    (cd /x-$c && md5sum -c DEBIAN/md5sums)
//	  done'
//
// Output of docker run (dpkg-deb --info prints the control file after these lines, as above):
//
//	== xz
//	 new Debian package, version 2.0.
//	 size 1020 bytes: control archive=540 bytes.
//	     319 bytes,    12 lines      control
//	     120 bytes,     2 lines      md5sums
//	./
//	./control
//	./md5sums
//	usr/share/doc/hello/README: OK
//	usr/share/hello/greeting: OK
//	== zst
//	 new Debian package, version 2.0.
//	 size 942 bytes: control archive=462 bytes.
//	     319 bytes,    12 lines      control
//	     120 bytes,     2 lines      md5sums
//	./
//	./control
//	./md5sums
//	usr/share/doc/hello/README: OK
//	usr/share/hello/greeting: OK

package testutil

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// defaultDescription is the Description of DebOptions{} as control has it.
const defaultDescription = "greets the tests of the deb format\n" +
	" A package built in process by internal/testutil.\n .\n Nothing in it comes from a distribution."

func TestDeb(t *testing.T) {
	for _, tc := range []struct {
		name         string
		opts         DebOptions
		filename     string
		description  string
		installedKiB string
	}{
		{
			name:         "defaults",
			filename:     "hello_1.0.0-1_amd64.deb",
			description:  defaultDescription,
			installedKiB: "2",
		},
		{
			name: "epoch, all, own files",
			opts: DebOptions{
				Name:        "libfoo2",
				Version:     "1:2.0~rc1-3",
				Arch:        "all",
				Depends:     "libc6 (>= 2.34), libbar1 | libbaz1",
				Description: "the foo library\nline one\n\n\nline four\n",
				Files: map[string]string{
					"./usr/lib/libfoo.so.2":             strings.Repeat("x", 2049),
					"/usr/share/doc/libfoo2/changelog":  "",
					"usr/share/doc/libfoo2/copyright":   "MIT\n",
					"usr/share/libfoo2/data/nested/one": "1",
				},
			},
			filename:     "libfoo2_2.0~rc1-3_all.deb",
			description:  "the foo library\n line one\n .\n .\n line four",
			installedKiB: "5",
		},
		{
			name:         "xz control",
			opts:         DebOptions{ControlCompression: "xz"},
			filename:     "hello_1.0.0-1_amd64.deb",
			description:  defaultDescription,
			installedKiB: "2",
		},
		{
			name:         "zst control",
			opts:         DebOptions{ControlCompression: "zst"},
			filename:     "hello_1.0.0-1_amd64.deb",
			description:  defaultDescription,
			installedKiB: "2",
		},
		{
			name:         "uncompressed control",
			opts:         DebOptions{ControlCompression: "none"},
			filename:     "hello_1.0.0-1_amd64.deb",
			description:  defaultDescription,
			installedKiB: "2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, want := Deb(tc.opts)
			members := readAr(t, b, want.ControlMember)
			if got := string(members["debian-binary"]); got != "2.0\n" {
				t.Errorf("debian-binary = %q, want %q", got, "2.0\n")
			}
			if want.Filename != tc.filename {
				t.Errorf("Filename = %q, want %q", want.Filename, tc.filename)
			}
			if want.Description != tc.description {
				t.Errorf("Description = %q, want %q", want.Description, tc.description)
			}
			if want.InstalledSize != tc.installedKiB {
				t.Errorf("InstalledSize = %q, want %q", want.InstalledSize, tc.installedKiB)
			}
			checkDebControl(t, members[want.ControlMember], want)
			checkDebData(t, members["data.tar.gz"], want)
			checkWhole(t, b, want.Size, want.Checksums)
		})
	}
}

// checkDebControl checks the control member: its members, control against the fields of want,
// and md5sums against want.Files and want.MD5Sums.
func checkDebControl(t *testing.T, b []byte, want DebInfo) {
	t.Helper()
	files := readTar(t, decompress(t, want.ControlMember, b))
	if got := tarNames(files); !slices.Equal(got, []string{"./", "./control", "./md5sums"}) {
		t.Errorf("%s members %q, want ./, ./control, ./md5sums", want.ControlMember, got)
	}
	control := files[1].body
	if !bytes.Equal(control, want.ControlFile) {
		t.Errorf("control = %q, want ControlFile %q", control, want.ControlFile)
	}
	fields := parseControl(t, control)
	if !maps.Equal(fields, want.Control) {
		t.Errorf("control fields %q, want Control %q", fields, want.Control)
	}
	for name, value := range map[string]string{
		"Package": want.Package, "Version": want.Version, "Architecture": want.Architecture,
		"Maintainer": want.Maintainer, "Installed-Size": want.InstalledSize, "Depends": want.Depends,
		"Section": want.Section, "Priority": want.Priority, "Description": want.Description,
	} {
		if fields[name] != value || value == "" {
			t.Errorf("%s: control has %q, DebInfo %q", name, fields[name], value)
		}
	}
	var listed []DebFile
	sums := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(files[2].body))
	for sc.Scan() {
		sum, p, ok := strings.Cut(sc.Text(), "  ")
		if !ok {
			t.Fatalf("md5sums line %q is not <md5>  <path>", sc.Text())
		}
		sums[p] = sum
		listed = append(listed, DebFile{Path: p, MD5: sum})
	}
	if !maps.Equal(sums, want.MD5Sums) {
		t.Errorf("md5sums %q, want MD5Sums %q", sums, want.MD5Sums)
	}
	if len(listed) != len(want.Files) {
		t.Fatalf("md5sums lists %d files, want %d", len(listed), len(want.Files))
	}
	for i, f := range listed {
		if f.Path != want.Files[i].Path || f.MD5 != want.Files[i].MD5 {
			t.Errorf("md5sums line %d: %s %s, want %s %s", i, f.MD5, f.Path, want.Files[i].MD5, want.Files[i].Path)
		}
	}
}

// checkDebData checks data.tar.gz: every directory comes before what it holds, and the files
// are want.Files with their sizes and the md5 of md5sums.
func checkDebData(t *testing.T, b []byte, want DebInfo) {
	t.Helper()
	seen := map[string]bool{}
	var files []DebFile
	for _, f := range readTarGz(t, b) {
		dir := f.name[:strings.LastIndex(strings.TrimSuffix(f.name, "/"), "/")+1]
		if f.name != "./" && !seen[dir] {
			t.Errorf("%s comes before its directory %s", f.name, dir)
		}
		if f.typ == tar.TypeDir {
			seen[f.name] = true
			continue
		}
		p, ok := strings.CutPrefix(f.name, "./")
		if !ok {
			t.Errorf("data file %q does not start with ./", f.name)
		}
		files = append(files, DebFile{Path: p, Size: int64(len(f.body)), MD5: checksumsOf(f.body).MD5})
	}
	if !slices.Equal(files, want.Files) {
		t.Errorf("data files %+v, want %+v", files, want.Files)
	}
}

func TestDebDeterministic(t *testing.T) {
	for _, c := range []string{"gz", "xz", "zst", "none"} {
		opts := DebOptions{Files: map[string]string{"usr/bin/a": "a", "usr/bin/b": "b", "usr/lib/c": "c"}, ControlCompression: c}
		b1, want1 := Deb(opts)
		b2, want2 := Deb(opts)
		if !bytes.Equal(b1, b2) || want1.SHA256 != want2.SHA256 {
			t.Fatalf("%s: two calls with the same options give different bytes", c)
		}
	}
	b3, _ := Deb(DebOptions{})
	if b4, _ := Deb(DebOptions{Version: "1.0.0-2"}); bytes.Equal(b3, b4) {
		t.Error("another version gives the same bytes")
	}
}

func TestDebPanicsOnAnUnknownCompression(t *testing.T) {
	defer func() {
		if r, _ := recover().(string); !strings.HasPrefix(r, `testutil: unknown control compression "bz2"`) {
			t.Errorf("recovered %q, want the unknown compression panic", r)
		}
	}()
	Deb(DebOptions{ControlCompression: "bz2"})
	t.Error("Deb did not panic")
}

func TestDebPanicsOnAPathUstarCannotHold(t *testing.T) {
	defer func() {
		if r, _ := recover().(string); !strings.HasPrefix(r, "testutil: ") {
			t.Errorf("recovered %q, want a testutil: panic", r)
		}
	}()
	Deb(DebOptions{Files: map[string]string{strings.Repeat("x", 300): ""}})
	t.Error("Deb did not panic")
}

// readAr reads an ar archive and returns its members by name. It fails the test unless the
// archive starts with the global header, the members are debian-binary, the control member
// control and data.tar.gz in that order, every header is well formed, and every member starts
// on an even offset.
func readAr(t *testing.T, b []byte, control string) map[string][]byte {
	t.Helper()
	rest, ok := bytes.CutPrefix(b, []byte("!<arch>\n"))
	if !ok {
		t.Fatalf("no ar global header: %q", b[:min(8, len(b))])
	}
	members := map[string][]byte{}
	var names []string
	for len(rest) > 0 {
		if len(rest) < 60 || string(rest[58:60]) != "`\n" {
			t.Fatalf("member %d: bad header %q", len(names), rest[:min(60, len(rest))])
		}
		h := rest[:60]
		name := strings.TrimSpace(string(h[:16]))
		size, err := strconv.Atoi(strings.TrimSpace(string(h[48:58])))
		if err != nil || size > len(rest)-60 {
			t.Fatalf("member %s: size %q of %d bytes left", name, h[48:58], len(rest)-60)
		}
		if mode := strings.TrimSpace(string(h[40:48])); mode != "100644" {
			t.Errorf("member %s: mode %s, want 100644", name, mode)
		}
		if mtime := strings.TrimSpace(string(h[16:28])); mtime != strconv.FormatInt(fixtureTime.Unix(), 10) {
			t.Errorf("member %s: mtime %s, want %d", name, mtime, fixtureTime.Unix())
		}
		members[name] = rest[60 : 60+size]
		names = append(names, name)
		rest = rest[60+size:]
		if size%2 == 1 {
			if len(rest) == 0 || rest[0] != '\n' {
				t.Fatalf("member %s: odd size %d without the padding newline", name, size)
			}
			rest = rest[1:]
		}
	}
	if !slices.Equal(names, []string{"debian-binary", control, "data.tar.gz"}) {
		t.Fatalf("members %q, want debian-binary, %s, data.tar.gz", names, control)
	}
	return members
}

// tarFile is a member of a tar archive.
type tarFile struct {
	name string
	typ  byte
	body []byte
}

// readTarGz reads a gzip-compressed tar archive (readTar).
func readTarGz(t *testing.T, b []byte) []tarFile {
	t.Helper()
	return readTar(t, decompress(t, "x.tar.gz", b))
}

// decompress returns the bytes of the archive member name, decompressed as its extension
// says: .gz, .xz, .zst, or none for .tar.
func decompress(t *testing.T, name string, b []byte) []byte {
	t.Helper()
	var (
		r   io.Reader
		err error
	)
	switch {
	case strings.HasSuffix(name, ".gz"):
		r, err = gzip.NewReader(bytes.NewReader(b))
	case strings.HasSuffix(name, ".xz"):
		r, err = xz.NewReader(bytes.NewReader(b))
	case strings.HasSuffix(name, ".zst"):
		var d *zstd.Decoder
		if d, err = zstd.NewReader(bytes.NewReader(b)); err == nil {
			defer d.Close()
			r = d
		}
	case strings.HasSuffix(name, ".tar"):
		return b
	default:
		t.Fatalf("%s: unknown compression", name)
	}
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

// readTar reads a tar archive and checks that every member is owned by root and dated
// fixtureTime.
func readTar(t *testing.T, b []byte) []tarFile {
	t.Helper()
	var files []tarFile
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar: %s: %v", h.Name, err)
		}
		if !h.ModTime.Equal(fixtureTime) || h.Uid != 0 || h.Gid != 0 || h.Uname != "root" {
			t.Errorf("%s: %v %d:%d %s, want %v 0:0 root", h.Name, h.ModTime, h.Uid, h.Gid, h.Uname, fixtureTime)
		}
		files = append(files, tarFile{name: h.Name, typ: h.Typeflag, body: body})
	}
	return files
}

// tarNames returns the names of the members.
func tarNames(files []tarFile) []string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.name
	}
	return names
}

// parseControl reads a control file: "Field: value" lines, and continuation lines starting
// with a space appended to the value of the field before them with a "\n".
func parseControl(t *testing.T, b []byte) map[string]string {
	t.Helper()
	fields := map[string]string{}
	var last string
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(line, " ") && last != "" {
			fields[last] += "\n" + line
			continue
		}
		name, value, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("control line %q is not a field", line)
		}
		fields[name], last = value, name
	}
	return fields
}

// checkWhole checks the size and the checksums of a whole file.
func checkWhole(t *testing.T, b []byte, size int64, sums Checksums) {
	t.Helper()
	if int64(len(b)) != size {
		t.Errorf("Size = %d, the file has %d bytes", size, len(b))
	}
	if got := checksumsOf(b); got != sums {
		t.Errorf("Checksums = %+v, the file has %+v", sums, got)
	}
}

func TestChecksumsOf(t *testing.T) {
	// The "abc" test vectors of RFC 1321, FIPS 180-4.
	want := Checksums{
		MD5:    "900150983cd24fb0d6963f7d28e17f72",
		SHA1:   "a9993e364706816aba3e25717850c26c9cd0d89d",
		SHA256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		SHA512: "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a" +
			"2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f",
	}
	if got := checksumsOf([]byte("abc")); got != want {
		t.Errorf("checksumsOf(abc) = %+v, want %+v", got, want)
	}
}

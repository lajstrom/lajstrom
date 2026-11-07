package testutil

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// DebOptions are the options of Deb. Every empty field takes its default, so DebOptions{} is a
// valid package.
type DebOptions struct {
	// Name is the Package field; default "hello".
	Name string
	// Version is the Version field, as dpkg spells it, epoch included (1:2.0-1); default
	// "1.0.0-1".
	Version string
	// Arch is the Architecture field: "amd64" (default), "all", "arm64", …
	Arch string
	// Depends is the Depends field; default "libc6 (>= 2.34)".
	Depends string
	// Description is the synopsis and then the extended description, one line per "\n"; an
	// empty line of the extended description is written as " .". Default: a synopsis and an
	// extended description with an empty line.
	Description string
	// Files are the data files, path relative to / (usr/share/hello/greeting) to content;
	// default two files under usr/share. A leading "./" or "/" is dropped.
	Files map[string]string
	// ControlCompression is the compression of the control member, one of the four dpkg-deb
	// reads: "gz" (default, control.tar.gz), "xz" (control.tar.xz), "zst" (control.tar.zst)
	// or "none" (control.tar). Deb panics on any other value.
	ControlCompression string
}

// DebFile is a data file of a package as md5sums lists it.
type DebFile struct {
	// Path is relative to /, without "./": usr/share/hello/greeting.
	Path string
	// Size is the size of the file in bytes.
	Size int64
	// MD5 is the lowercase hex md5 of the file.
	MD5 string
}

// DebInfo is what a parser finds in the package Deb builds.
type DebInfo struct {
	// The fields of control, as written after "Field: ". Description is the synopsis and the
	// continuation lines joined with "\n", each continuation line with its leading space.
	// InstalledSize is the sum of the data file sizes in KiB, each rounded up.
	Package       string
	Version       string
	Architecture  string
	Maintainer    string
	InstalledSize string
	Depends       string
	Section       string
	Priority      string
	Description   string
	// Control holds the same fields by name (Installed-Size, …).
	Control map[string]string
	// ControlFile is the control file in the control member.
	ControlFile []byte
	// ControlMember is the name of the control member Deb wrote: control.tar.gz,
	// control.tar.xz, control.tar.zst or control.tar.
	ControlMember string
	// Files are the data files sorted by path, the order of md5sums.
	Files []DebFile
	// MD5Sums is md5sums as path to md5, the shape of the metadata layer
	// (docs/spec/manifest-profile.md §2).
	MD5Sums map[string]string
	// Filename is <Package>_<Version without its epoch>_<Architecture>.deb, the name that
	// dpkg-name gives it.
	Filename string
	// Size is the size of the whole file in bytes.
	Size int64
	// Checksums are the digests of the whole file.
	Checksums
}

// Deb returns a binary package: an ar archive with the members debian-binary ("2.0\n"), the
// control member (./control and ./md5sums; control.tar.gz unless ControlCompression says
// otherwise) and data.tar.gz (the files under ./, with their directories), and the facts a
// parser finds in it. The same options give the same bytes: every member and file is dated
// 2026-01-01 00:00 UTC and owned by root.
//
// xz and zstd are written with github.com/ulikunitz/xz and github.com/klauspost/compress/zstd,
// the libraries the deb plugin reads them with (#39); dpkg-deb read all four once (the comment
// at the top of deb_test.go). Deb panics on a file path that a ustar header cannot hold.
func Deb(opts DebOptions) ([]byte, DebInfo) {
	o := opts.withDefaults()
	files := debFiles(o.Files)
	var md5sums strings.Builder
	data := []member{{name: "./"}}
	dirs := map[string]bool{}
	sums := make(map[string]string, len(files))
	var installed int64
	for _, f := range files {
		fmt.Fprintf(&md5sums, "%s  %s\n", f.MD5, f.Path)
		sums[f.Path] = f.MD5
		data = append(data, debDirs(f.Path, dirs)...)
		data = append(data, member{name: "./" + f.Path, body: []byte(o.Files[f.Path])})
		installed += (f.Size + 1023) / 1024
	}
	info := DebInfo{
		Package:       o.Name,
		Version:       o.Version,
		Architecture:  o.Arch,
		Maintainer:    "Lajstrom Tests <tests@lajstrom.dev>",
		InstalledSize: strconv.FormatInt(installed, 10),
		Depends:       o.Depends,
		Section:       "misc",
		Priority:      "optional",
		Description:   debDescription(o.Description),
		Files:         files,
		MD5Sums:       sums,
	}
	var control strings.Builder
	info.Control = map[string]string{}
	for _, kv := range [][2]string{
		{"Package", info.Package}, {"Version", info.Version}, {"Architecture", info.Architecture},
		{"Maintainer", info.Maintainer}, {"Installed-Size", info.InstalledSize},
		{"Depends", info.Depends}, {"Section", info.Section}, {"Priority", info.Priority},
		{"Description", info.Description},
	} {
		fmt.Fprintf(&control, "%s: %s\n", kv[0], kv[1])
		info.Control[kv[0]] = kv[1]
	}
	info.ControlFile = []byte(control.String())

	var ar bytes.Buffer
	ar.WriteString("!<arch>\n")
	arMember(&ar, "debian-binary", []byte("2.0\n"))
	var compressed []byte
	info.ControlMember, compressed = controlMember(o.ControlCompression, tarOf([]member{
		{name: "./"},
		{name: "./control", body: info.ControlFile},
		{name: "./md5sums", body: []byte(md5sums.String())},
	}))
	arMember(&ar, info.ControlMember, compressed)
	arMember(&ar, "data.tar.gz", tarGz(data))

	b := ar.Bytes()
	version := o.Version
	if _, v, ok := strings.Cut(version, ":"); ok {
		version = v
	}
	info.Filename = o.Name + "_" + version + "_" + o.Arch + ".deb"
	info.Size = int64(len(b))
	info.Checksums = checksumsOf(b)
	return b, info
}

// withDefaults returns the options with every empty field set to its default.
func (o DebOptions) withDefaults() DebOptions {
	if o.Name == "" {
		o.Name = "hello"
	}
	if o.Version == "" {
		o.Version = "1.0.0-1"
	}
	if o.Arch == "" {
		o.Arch = "amd64"
	}
	if o.ControlCompression == "" {
		o.ControlCompression = "gz"
	}
	if o.Depends == "" {
		o.Depends = "libc6 (>= 2.34)"
	}
	if o.Description == "" {
		o.Description = "greets the tests of the deb format\n" +
			"A package built in process by internal/testutil.\n" +
			"\n" +
			"Nothing in it comes from a distribution."
	}
	files := map[string]string{
		"usr/share/" + o.Name + "/greeting":   "hello, world\n",
		"usr/share/doc/" + o.Name + "/README": "Built by internal/testutil for the tests.\n",
	}
	if len(o.Files) > 0 {
		files = map[string]string{}
		for p, body := range o.Files {
			files[path.Clean(strings.TrimLeft(strings.TrimPrefix(p, "./"), "/"))] = body
		}
	}
	o.Files = files
	return o
}

// controlMember returns the name and the bytes of the control member: the control tar
// compressed as compression says. The xz stream has the writer's defaults (an 8 MiB dictionary,
// CRC64), the zstd frame the encoder's default level, encoded on one goroutine so that the
// bytes do not depend on scheduling.
func controlMember(compression string, tar []byte) (string, []byte) {
	switch compression {
	case "gz":
		return "control.tar.gz", gzipOf(tar)
	case "xz":
		var buf bytes.Buffer
		xw, err := xz.NewWriter(&buf)
		must(err)
		_, err = xw.Write(tar)
		must(err)
		must(xw.Close())
		return "control.tar.xz", buf.Bytes()
	case "zst":
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		must(err)
		return "control.tar.zst", enc.EncodeAll(tar, nil)
	case "none":
		return "control.tar", tar
	}
	panic(fmt.Sprintf("testutil: unknown control compression %q, want gz, xz, zst or none", compression))
}

// debFiles returns the data files sorted by path, with size and md5.
func debFiles(files map[string]string) []DebFile {
	out := make([]DebFile, 0, len(files))
	for p, body := range files {
		out = append(out, DebFile{Path: p, Size: int64(len(body)), MD5: checksumsOf([]byte(body)).MD5})
	}
	slices.SortFunc(out, func(a, b DebFile) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// debDirs returns the directory members ("./usr/", "./usr/share/", …) that the file p needs and
// that are not in seen yet, outermost first, and adds them to seen.
func debDirs(p string, seen map[string]bool) []member {
	var dirs []member
	for i := range len(p) {
		if p[i] != '/' || seen[p[:i]] {
			continue
		}
		seen[p[:i]] = true
		dirs = append(dirs, member{name: "./" + p[:i+1]})
	}
	return dirs
}

// debDescription writes a description as control wants it: the first line is the synopsis,
// every following line gets a leading space, and an empty one is " .".
func debDescription(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i] == "" {
			lines[i] = "."
		}
		lines[i] = " " + lines[i]
	}
	return strings.Join(lines, "\n")
}

// arMember appends one member to an ar archive: the 60-byte header (name, mtime, uid, gid, mode
// in octal, size in decimal, each left-aligned and space-padded, then "`\n"), the data, and a
// "\n" when the size is odd, so that the next header starts on an even offset. The name has no
// trailing '/', as dpkg-deb writes it.
func arMember(ar *bytes.Buffer, name string, data []byte) {
	fmt.Fprintf(ar, "%-16s%-12d%-6d%-6d%-8o%-10d`\n", name, fixtureTime.Unix(), 0, 0, 0o100644, len(data))
	ar.Write(data)
	if len(data)%2 == 1 {
		ar.WriteByte('\n')
	}
}

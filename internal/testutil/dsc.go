package testutil

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ulikunitz/xz"
)

// DscOptions are the options of Dsc. Every empty field takes its default, so DscOptions{} is a
// valid source package.
type DscOptions struct {
	// Source is the Source field; default "hello".
	Source string
	// Version is the Version field, epoch included (1:2.0-1); default "1.0.0-1". A version
	// with a Debian revision (a "-") makes a 3.0 (quilt) package of an .orig.tar.gz and a
	// .debian.tar.xz; one without, a 3.0 (native) package of one .tar.gz.
	Version string
	// Binary is the Binary field, the binary packages the source builds; default Source.
	Binary string
	// BuildDepends is the Build-Depends field; default "debhelper-compat (= 13)".
	BuildDepends string
	// Files are the files of the upstream tree, path below its top directory to content;
	// default a README and src/hello.c.
	Files map[string]string
	// Signed wraps the .dsc in an OpenPGP clearsigned message, its signature block made up:
	// nothing in Lajstrom checks it.
	Signed bool
}

// SourceFile is one file a .dsc names, and its bytes.
type SourceFile struct {
	// Name is the file name, hello_1.0.0.orig.tar.gz.
	Name string
	// Content is the file.
	Content []byte
	// Size is len(Content).
	Size int64
	// Checksums are the digests of Content.
	Checksums
}

// DscInfo is a source package Dsc built: the .dsc, the files it names and what a parser finds
// in them.
type DscInfo struct {
	// Source, Version and Format are the fields of the .dsc.
	Source, Version, Format string
	// Filename is <Source>_<Version without its epoch>.dsc, the name dpkg-source gives it.
	Filename string
	// Dsc is the .dsc file, signed when DscOptions.Signed.
	Dsc []byte
	// Paragraph is the .dsc without its OpenPGP armor: the fields as a control file has them.
	Paragraph []byte
	// Control holds the fields of the .dsc by name; a field of several lines has them joined
	// with "\n", each continuation line with its leading space, the first one empty for the
	// lists (Files, Checksums-Sha256, Package-List).
	Control map[string]string
	// Files are the files the .dsc names, in the order of its lists.
	Files []SourceFile
	// Size and Checksums are those of the .dsc.
	Size int64
	Checksums
}

// Dsc returns a source package: a 3.0 (quilt) or 3.0 (native) .dsc and the tarballs it names,
// as dpkg-source -b lays them out, with Files, Checksums-Sha1 and Checksums-Sha256. The same
// options give the same bytes: every tar member is dated 2026-01-01 00:00 UTC and owned by root.
func Dsc(opts DscOptions) DscInfo {
	o := opts.withDefaults()
	revision := strings.Contains(o.Version, "-")
	version := o.Version
	if _, v, ok := strings.Cut(version, ":"); ok {
		version = v
	}
	upstream := version
	if i := strings.LastIndex(version, "-"); revision && i >= 0 {
		upstream = version[:i]
	}
	top := o.Source + "-" + upstream + "/"
	tree := []member{{name: top}}
	for _, p := range slices.Sorted(maps.Keys(o.Files)) {
		tree = append(tree, member{name: top + p, body: []byte(o.Files[p])})
	}
	debian := []member{
		{name: "debian/"},
		{name: "debian/changelog", body: []byte(fmt.Sprintf("%s (%s) unstable; urgency=medium\n\n  * Built by internal/testutil.\n\n"+
			" -- Lajstrom Tests <tests@lajstrom.dev>  Thu, 01 Jan 2026 00:00:00 +0000\n", o.Source, o.Version))},
		{name: "debian/control", body: []byte(fmt.Sprintf("Source: %s\nMaintainer: Lajstrom Tests <tests@lajstrom.dev>\n"+
			"Build-Depends: %s\n\nPackage: %s\nArchitecture: any\nDescription: built by internal/testutil\n", o.Source, o.BuildDepends, o.Binary))},
		{name: "debian/source/"},
	}
	info := DscInfo{Source: o.Source, Version: o.Version, Filename: o.Source + "_" + version + ".dsc"}
	if revision {
		info.Format = "3.0 (quilt)"
		debian = append(debian, member{name: "debian/source/format", body: []byte("3.0 (quilt)\n")})
		info.Files = []SourceFile{
			sourceFile(o.Source+"_"+upstream+".orig.tar.gz", tarGz(tree)),
			sourceFile(o.Source+"_"+version+".debian.tar.xz", xzOf(tarOf(debian))),
		}
	} else {
		info.Format = "3.0 (native)"
		debian = append(debian, member{name: "debian/source/format", body: []byte("3.0 (native)\n")})
		for i := range debian {
			debian[i].name = top + debian[i].name
		}
		info.Files = []SourceFile{sourceFile(o.Source+"_"+version+".tar.gz", tarGz(append(tree, debian...)))}
	}

	var files, sha1s, sha256s strings.Builder
	for _, f := range info.Files {
		fmt.Fprintf(&files, "\n %s %d %s", f.MD5, f.Size, f.Name)
		fmt.Fprintf(&sha1s, "\n %s %d %s", f.SHA1, f.Size, f.Name)
		fmt.Fprintf(&sha256s, "\n %s %d %s", f.SHA256, f.Size, f.Name)
	}
	fields := [][2]string{
		{"Format", info.Format},
		{"Source", o.Source},
		{"Binary", o.Binary},
		{"Architecture", "any"},
		{"Version", o.Version},
		{"Maintainer", "Lajstrom Tests <tests@lajstrom.dev>"},
		{"Standards-Version", "4.6.2"},
		{"Build-Depends", o.BuildDepends},
		{"Package-List", "\n " + o.Binary + " deb misc optional arch=any"},
		{"Checksums-Sha1", sha1s.String()},
		{"Checksums-Sha256", sha256s.String()},
		{"Files", files.String()},
	}
	var para bytes.Buffer
	info.Control = map[string]string{}
	for _, f := range fields {
		para.WriteString(f[0] + ":")
		if !strings.HasPrefix(f[1], "\n") {
			para.WriteByte(' ')
		}
		para.WriteString(f[1] + "\n")
		info.Control[f[0]] = f[1]
	}
	info.Paragraph = para.Bytes()
	info.Dsc = info.Paragraph
	if o.Signed {
		info.Dsc = clearsigned(info.Paragraph)
	}
	info.Size = int64(len(info.Dsc))
	info.Checksums = checksumsOf(info.Dsc)
	return info
}

// withDefaults returns the options with every empty field set to its default.
func (o DscOptions) withDefaults() DscOptions {
	if o.Source == "" {
		o.Source = "hello"
	}
	if o.Version == "" {
		o.Version = "1.0.0-1"
	}
	if o.Binary == "" {
		o.Binary = o.Source
	}
	if o.BuildDepends == "" {
		o.BuildDepends = "debhelper-compat (= 13)"
	}
	if len(o.Files) == 0 {
		o.Files = map[string]string{
			"README":      "Built by internal/testutil for the tests.\n",
			"src/hello.c": "int main(void) { return 0; }\n",
		}
	}
	return o
}

// clearsigned wraps text in an OpenPGP clearsigned message (RFC 9580 §7): the armor header,
// the text with every line that starts with "-" dash-escaped, and a signature block that is
// made up, of the right shape and verifying nothing.
func clearsigned(text []byte) []byte {
	var b bytes.Buffer
	b.WriteString("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n\n")
	for _, line := range strings.SplitAfter(string(text), "\n") {
		if strings.HasPrefix(line, "-") {
			b.WriteString("- ")
		}
		b.WriteString(line)
	}
	b.WriteString("-----BEGIN PGP SIGNATURE-----\n\n" +
		"iHUEARYKAB0WIQRsYWpzdHJvbSB0ZXN0cyBkbyBub3QgdmVyaWZ5AAoJEGxhanN0\n" +
		"cm9tAAAAAA==\n=AAAA\n-----END PGP SIGNATURE-----\n")
	return b.Bytes()
}

// sourceFile is the SourceFile of name with content b.
func sourceFile(name string, b []byte) SourceFile {
	return SourceFile{Name: name, Content: b, Size: int64(len(b)), Checksums: checksumsOf(b)}
}

// xzOf returns b compressed with xz, the writer's defaults.
func xzOf(b []byte) []byte {
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	must(err)
	_, err = w.Write(b)
	must(err)
	must(w.Close())
	return buf.Bytes()
}

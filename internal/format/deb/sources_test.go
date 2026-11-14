package deb

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/testutil"
)

// sourcesParagraph is the Sources paragraph of the source package d in component.
func sourcesParagraph(d testutil.DscInfo, component string) string {
	list := func(own string, sum func(testutil.SourceFile) string) string {
		s := fmt.Sprintf("\n %s %d %s", own, d.Size, d.Filename)
		for _, f := range d.Files {
			s += fmt.Sprintf("\n %s %d %s", sum(f), f.Size, f.Name)
		}
		return s
	}
	return "Package: " + d.Source + "\nBinary: " + d.Source + "\nVersion: " + d.Version + "\n" +
		"Maintainer: Lajstrom Tests <tests@lajstrom.dev>\nBuild-Depends: debhelper-compat (= 13)\nArchitecture: any\n" +
		"Standards-Version: 4.6.2\nFormat: " + d.Format + "\n" +
		"Files:" + list(d.MD5, func(f testutil.SourceFile) string { return f.MD5 }) + "\n" +
		"Checksums-Sha1:" + list(d.SHA1, func(f testutil.SourceFile) string { return f.SHA1 }) + "\n" +
		"Checksums-Sha256:" + list(d.SHA256, func(f testutil.SourceFile) string { return f.SHA256 }) + "\n" +
		"Package-List:\n " + d.Source + " deb misc optional arch=any\n" +
		"Directory: pool/" + component + "/" + poolDir(d.Source) + "\n"
}

// TestRenderSources renders a suite with binaries and source packages (the fixtures of the golden
// files, hello 1.0.0-1 and libfoo 1:1.2-3 as sources in jammy/main, signed and not): every
// component has a Sources, empty for universe, which has no source; the paragraphs are the .dsc's
// fields with Source as Package, the lists with the .dsc first and Directory, sorted by name; the
// Release lists the three variants with their hashes, and its Architectures and the Packages are
// those of the binaries alone.
func TestRenderSources(t *testing.T) {
	p := newPipeline(t)
	fixtureSet(t, p)
	before := render(t, p, fixedClock(p), "jammy")
	hello := testutil.Dsc(testutil.DscOptions{Signed: true})
	libfoo := testutil.Dsc(testutil.DscOptions{Source: "libfoo", Version: "1:1.2-3"})
	for _, d := range []testutil.DscInfo{libfoo, hello} {
		if _, err := p.pipe.Upload(t.Context(), p.repo, upload(sourceFiles(d)), alice); err != nil {
			t.Fatal(err)
		}
	}
	docs := render(t, p, fixedClock(p), "jammy")
	if got, want := string(docs["main/source/Sources"]), sourcesParagraph(hello, "main")+"\n"+sourcesParagraph(libfoo, "main")+"\n"; got != want {
		t.Errorf("main/source/Sources:\n%s\nwant\n%s", got, want)
	}
	if got := docs["universe/source/Sources"]; len(got) != 0 {
		t.Errorf("universe/source/Sources: %q, want empty", got)
	}
	if !bytes.Equal(gunzip(t, docs["main/source/Sources.gz"]), docs["main/source/Sources"]) ||
		!bytes.Equal(unxz(t, docs["main/source/Sources.xz"]), docs["main/source/Sources"]) {
		t.Error("Sources.gz or Sources.xz is not Sources")
	}
	checkRelease(t, docs[kindRelease], docs)
	for kind, b := range before {
		if kind != kindRelease && !bytes.Equal(docs[kind], b) {
			t.Errorf("%s changed with the sources", kind)
		}
	}
	if got, want := field(string(docs[kindRelease]), "Architectures"), field(string(before[kindRelease]), "Architectures"); got != want {
		t.Errorf("Architectures %q, want %q", got, want)
	}
}

// TestRenderSourcesOnly: a suite with source packages alone has their Sources and a Release
// without architectures; one with binaries alone has no Sources (the golden files).
func TestRenderSourcesOnly(t *testing.T) {
	p := newPipeline(t)
	d := testutil.Dsc(testutil.DscOptions{})
	files := upload(sourceFiles(d), ParamSuite, "sid", ParamComponent, "contrib")
	if _, err := p.pipe.Upload(t.Context(), p.repo, files, alice); err != nil {
		t.Fatal(err)
	}
	docs := render(t, p, fixedClock(p), "sid")
	kinds := slices.Sorted(maps.Keys(docs))
	if want := []string{"Release", "contrib/source/Sources", "contrib/source/Sources.gz", "contrib/source/Sources.xz"}; !slices.Equal(kinds, want) {
		t.Errorf("sid renders %q, want %q", kinds, want)
	}
	release := string(docs[kindRelease])
	if !strings.Contains(release, "\nArchitectures:\nComponents: contrib\n") {
		t.Errorf("the Release:\n%s", release)
	}
	if got := string(docs["contrib/source/Sources"]); got != sourcesParagraph(d, "contrib")+"\n" {
		t.Errorf("Sources:\n%s", got)
	}
}

// TestPoolContentType: the pool serves each file with the Content-Type of its name.
func TestPoolContentType(t *testing.T) {
	for name, want := range map[string]string{
		"hello_1.0_amd64.deb": packageContentType, "hello_1.0-1.dsc": "text/plain; charset=utf-8",
		"hello_1.0.orig.tar.gz": "application/gzip", "hello_1.0-1.debian.tar.xz": "application/x-xz",
		"hello_1.0.orig.tar.bz2": "application/x-bzip2", "hello_1.0.orig.tar.lzma": "application/x-lzma",
		"hello_1.0.orig.tar.zst": "application/zstd", "hello_1.0.orig.tar.gz.asc": "application/pgp-signature",
		"hello_1.0.orig.tar": "application/octet-stream",
	} {
		if got := poolContentType(name); got != want {
			t.Errorf("poolContentType(%s) = %q, want %q", name, got, want)
		}
	}
}

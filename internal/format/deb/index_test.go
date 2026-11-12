package deb

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"  //nolint:gosec // the MD5Sum section of a Release
	"crypto/sha1" //nolint:gosec // the SHA1 section of a Release
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/ulikunitz/xz"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// renderedAt is the clock of the renders whose Release a test compares: its Date is
// "Sun, 08 Nov 2026 09:30:00 UTC".
var renderedAt = time.Date(2026, 11, 8, 9, 30, 0, 0, time.UTC)

// fixedClock is a Plugin that renders at renderedAt with the providers of p.
func fixedClock(p *pipeline) Plugin {
	return Plugin{svc: &services{providers: provider.Map{"local": p.prov}, now: func() time.Time { return renderedAt }}}
}

// libfooControl is the control file of libfoo2, written by hand: a Source with a version, a
// relation folded onto continuation lines, a field the order does not name, a Multi-Arch before
// the relations, and a Description with an empty line and a line that starts with a tab.
const libfooControl = "Package: libfoo2\n" +
	"Source: libfoo (2.1-1)\n" +
	"Version: 2.1-3\n" +
	"Architecture: amd64\n" +
	"Multi-Arch: same\n" +
	"Maintainer: Lajstrom Tests <tests@lajstrom.dev>\n" +
	"Installed-Size: 1\n" +
	"Depends:\n libc6 (>= 2.34),\n zlib1g (>= 1:1.2.0)\n" +
	"Section: libs\n" +
	"Priority: optional\n" +
	"X-Lajstrom-Note: kept as written\n" +
	"Description: the foo library\n Shared objects of foo, built in process.\n .\n\tA line that starts with a tab.\n"

// fixtureSet uploads the packages of the golden files through the real pipeline:
//
//	jammy/main          hello 1.0.0-1 amd64 and arm64, hello-data 1:2.0-1 all, bonjour 0.1-1 arm64
//	jammy/universe      libfoo2 2.1-3 amd64, Source libfoo (control written by hand)
//	jammy-updates/main  hello 1.0.0-1 amd64 (the same .deb as in jammy) and 1.1.0-1 amd64
//
// bonjour is uploaded last and sorts first.
func fixtureSet(t *testing.T, p *pipeline) {
	t.Helper()
	hello, _ := testutil.Deb(testutil.DebOptions{})
	helloArm, _ := testutil.Deb(testutil.DebOptions{Arch: "arm64"})
	hello11, _ := testutil.Deb(testutil.DebOptions{Version: "1.1.0-1"})
	data, _ := testutil.Deb(testutil.DebOptions{Name: "hello-data", Version: "1:2.0-1", Arch: "all", Depends: "hello (>= 1.0.0)",
		Description: "the greetings of hello\nEvery architecture shares them."})
	bonjour, _ := testutil.Deb(testutil.DebOptions{Name: "bonjour", Version: "0.1-1", Arch: "arm64", ControlCompression: "zst"})
	libfoo := debOf(t, libfooControl, ptr("b026324c6904b2a9cb4b88d6d61c81d1  usr/lib/libfoo.so.2\n"), "xz")
	for _, u := range []struct {
		deb              []byte
		suite, component string
	}{
		{hello, "jammy", "main"}, {hello, "jammy-updates", "main"}, {helloArm, "jammy", "main"}, {hello11, "jammy-updates", "main"},
		{data, "jammy", "main"}, {libfoo, "jammy", "universe"}, {bonjour, "jammy", "main"},
	} {
		p.uploadTo(t, u.deb, u.suite, u.component)
	}
}

// TestRenderGolden renders the suites of fixtureSet and compares the Release and every
// Packages with testdata/apt/<suite>/<path>; UPDATE_GOLDEN=1 rewrites the files. The clock is
// fixed, so the Date is compared as it is. The compressed indexes are not compared byte for
// byte, since their bytes are the compressor's: each must decompress to its Packages, the hash
// and size lines of all three variants in the three sections of the Release must be those of
// the documents, and the golden Release has placeholders for the hashes and sizes of .gz and
// .xz.
func TestRenderGolden(t *testing.T) {
	p := newPipeline(t)
	fixtureSet(t, p)
	compared := map[string]bool{}
	for _, suite := range []string{"jammy", "jammy-updates"} {
		docs, err := fixedClock(p).RenderIndex(t.Context(), p.repo, format.IndexScope(suite), p.st)
		must(t, err)
		byKind := documents(t, suite, docs)
		for kind, content := range byKind {
			switch {
			case kind == kindRelease:
				golden(t, suite+"/Release", checkRelease(t, content, byKind))
			case strings.HasSuffix(kind, ".gz"):
				if got := gunzip(t, content); !bytes.Equal(got, byKind[strings.TrimSuffix(kind, ".gz")]) {
					t.Errorf("%s/%s does not decompress to its Packages", suite, kind)
				}
				continue
			case strings.HasSuffix(kind, ".xz"):
				if got := unxz(t, content); !bytes.Equal(got, byKind[strings.TrimSuffix(kind, ".xz")]) {
					t.Errorf("%s/%s does not decompress to its Packages", suite, kind)
				}
				continue
			default:
				golden(t, suite+"/"+kind, content)
			}
			compared[filepath.FromSlash(suite+"/"+kind)] = true
		}
	}
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		return
	}
	must(t, filepath.WalkDir(filepath.Join("testdata", "apt"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(filepath.Join("testdata", "apt"), path)
			if !compared[rel] {
				t.Errorf("%s is not a document of the render; remove it", path)
			}
		}
		return err
	}))
}

// documents checks the Kind, Path and ContentType of the documents of a suite, that the
// Release comes first and that no kind is there twice, and returns their contents by kind.
func documents(t *testing.T, suite string, docs []format.Document) map[string][]byte {
	t.Helper()
	byKind := map[string][]byte{}
	for i, d := range docs {
		want := packagesContentType
		switch {
		case d.Kind == kindRelease:
			want = "text/plain"
		case strings.HasSuffix(d.Kind, ".gz"):
			want = "application/gzip"
		case strings.HasSuffix(d.Kind, ".xz"):
			want = "application/x-xz"
		}
		if d.Path != "dists/"+suite+"/"+d.Kind || d.ContentType != want || (i == 0) != (d.Kind == kindRelease) {
			t.Errorf("document %d of %s: kind %q, path %q, content type %q", i, suite, d.Kind, d.Path, d.ContentType)
		}
		if _, dup := byKind[d.Kind]; dup {
			t.Errorf("%s has the document %s twice", suite, d.Kind)
		}
		byKind[d.Kind] = d.Content
	}
	return byKind
}

// releaseSums are the hashes of the three sections of a Release.
var releaseSums = map[string]func([]byte) string{
	"MD5Sum": func(b []byte) string { s := md5.Sum(b); return hex.EncodeToString(s[:]) },  //nolint:gosec // what apt checks
	"SHA1":   func(b []byte) string { s := sha1.Sum(b); return hex.EncodeToString(s[:]) }, //nolint:gosec // what apt checks
	"SHA256": func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) },
}

// checkRelease checks that each of the MD5Sum, SHA1 and SHA256 sections of release lists every
// document of byKind but the Release, once, with its hash and size, and returns release with
// the hash and size of every .gz and .xz replaced by placeholders.
func checkRelease(t *testing.T, release []byte, byKind map[string][]byte) []byte {
	t.Helper()
	var out strings.Builder
	section := ""
	seen := map[string]int{}
	for line := range strings.Lines(string(release)) {
		if !strings.HasPrefix(line, " ") {
			if name, ok := strings.CutSuffix(line, ":\n"); ok {
				section = name
			}
			out.WriteString(line)
			continue
		}
		f := strings.Fields(line)
		content, ok := byKind[f[2]]
		switch {
		case len(f) != 3 || !ok || f[2] == kindRelease:
			t.Errorf("Release line %q names no index of the render", line)
		case f[0] != releaseSums[section](content) || f[1] != fmt.Sprint(len(content)):
			t.Errorf("Release %s line %q: the document has %s %d", section, line, releaseSums[section](content), len(content))
		}
		seen[section+" "+f[2]]++
		if strings.HasSuffix(f[2], ".gz") || strings.HasSuffix(f[2], ".xz") {
			line = " <" + section + "> <size> " + f[2] + "\n"
		}
		out.WriteString(line)
	}
	for kind := range byKind {
		for section := range releaseSums {
			if n := seen[section+" "+kind]; n != 1 && kind != kindRelease {
				t.Errorf("the %s section lists %s %d times", section, kind, n)
			}
		}
	}
	return []byte(out.String())
}

// golden compares got with testdata/apt/<name>, or writes it there when UPDATE_GOLDEN=1.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "apt", filepath.FromSlash(name))
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		must(t, os.MkdirAll(filepath.Dir(path), 0o750))
		must(t, os.WriteFile(path, got, 0o600)) // git keeps no mode but the executable bit
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // a file below testdata/apt that the test names

	must(t, err)
	if !bytes.Equal(got, want) {
		t.Errorf("%s is not %s (UPDATE_GOLDEN=1 rewrites it):\n--- got\n%s--- want\n%s", name, path, got, want)
	}
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	must(t, err)
	out, err := io.ReadAll(zr)
	must(t, err)
	return out
}

func unxz(t *testing.T, b []byte) []byte {
	t.Helper()
	xr, err := xz.NewReader(bytes.NewReader(b))
	must(t, err)
	out, err := io.ReadAll(xr)
	must(t, err)
	return out
}

// render renders suite with the fixed clock and returns the contents by kind.
func render(t *testing.T, p *pipeline, pl Plugin, suite string) map[string][]byte {
	t.Helper()
	docs, err := pl.RenderIndex(t.Context(), p.repo, format.IndexScope(suite), p.st)
	must(t, err)
	return documents(t, suite, docs)
}

// paragraphsOf splits a Packages index into its paragraphs.
func paragraphsOf(b []byte) []string {
	return slices.DeleteFunc(strings.Split(string(b), "\n\n"), func(s string) bool { return s == "" })
}

// field returns the value of the field name in a paragraph, its first line.
func field(paragraph, name string) string {
	for line := range strings.Lines(paragraph) {
		if v, ok := strings.CutPrefix(line, name+": "); ok {
			return strings.TrimSuffix(v, "\n")
		}
	}
	return ""
}

// TestRenderPackages checks the rules of the Packages indexes on fixtureSet one by one: the all
// package in the index of every architecture of its component; the empty index of a component
// without a package of an architecture; the sort by name, then version; the package in two
// suites with one Filename; the pool path of a lib* package of a source; the control fields of
// the hand-written control file, a folded relation and the tab of a Description unchanged; the
// fields of the file after them.
func TestRenderPackages(t *testing.T) {
	p := newPipeline(t)
	fixtureSet(t, p)
	pl := fixedClock(p)
	jammy, updates := render(t, p, pl, "jammy"), render(t, p, pl, "jammy-updates")

	names := func(b []byte) []string {
		var out []string
		for _, para := range paragraphsOf(b) {
			out = append(out, field(para, "Package")+" "+field(para, "Version")+" "+field(para, "Architecture"))
		}
		return out
	}
	for _, c := range []struct {
		docs map[string][]byte
		kind string
		want []string
	}{
		{jammy, "main/binary-amd64/Packages", []string{"hello 1.0.0-1 amd64", "hello-data 1:2.0-1 all"}},
		{jammy, "main/binary-arm64/Packages", []string{"bonjour 0.1-1 arm64", "hello 1.0.0-1 arm64", "hello-data 1:2.0-1 all"}},
		{jammy, "main/binary-all/Packages", []string{"hello-data 1:2.0-1 all"}},
		{jammy, "universe/binary-all/Packages", nil},
		{jammy, "universe/binary-amd64/Packages", []string{"libfoo2 2.1-3 amd64"}},
		{jammy, "universe/binary-arm64/Packages", nil},
		{updates, "main/binary-amd64/Packages", []string{"hello 1.0.0-1 amd64", "hello 1.1.0-1 amd64"}},
	} {
		if got := names(c.docs[c.kind]); !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.kind, got, c.want)
		}
	}
	if got := slices.Sorted(maps.Keys(updates)); len(got) != 4 || got[0] != kindRelease {
		t.Errorf("jammy-updates renders %q, want the Release and main/binary-amd64 alone", got)
	}
	if len(jammy["universe/binary-arm64/Packages"]) != 0 {
		t.Errorf("universe/binary-arm64/Packages = %q, want empty", jammy["universe/binary-arm64/Packages"])
	}

	inJammy, inUpdates := paragraphsOf(jammy["main/binary-amd64/Packages"])[0], paragraphsOf(updates["main/binary-amd64/Packages"])[0]
	if field(inJammy, "Filename") != "pool/main/h/hello/hello_1.0.0-1_amd64.deb" || field(inJammy, "Filename") != field(inUpdates, "Filename") ||
		inJammy != inUpdates {
		t.Errorf("hello 1.0.0-1 in jammy:\n%s\nin jammy-updates:\n%s\nwant one paragraph with one Filename", inJammy, inUpdates)
	}
	if got := field(paragraphsOf(jammy["main/binary-amd64/Packages"])[1], "Filename"); got != "pool/main/h/hello-data/hello-data_2.0-1_all.deb" {
		t.Errorf("Filename of hello-data = %q", got)
	}

	libfoo := paragraphsOf(jammy["universe/binary-amd64/Packages"])[0]
	wantControl := "Package: libfoo2\n" +
		"Source: libfoo (2.1-1)\n" +
		"Version: 2.1-3\n" +
		"Architecture: amd64\n" +
		"Maintainer: Lajstrom Tests <tests@lajstrom.dev>\n" +
		"Installed-Size: 1\n" +
		"Depends:\n libc6 (>= 2.34),\n zlib1g (>= 1:1.2.0)\n" +
		"Section: libs\n" +
		"Priority: optional\n" +
		"Multi-Arch: same\n" +
		"X-Lajstrom-Note: kept as written\n" +
		"Description: the foo library\n Shared objects of foo, built in process.\n .\n\tA line that starts with a tab.\n" +
		"Filename: pool/universe/libf/libfoo/libfoo2_2.1-3_amd64.deb\n"
	if !strings.HasPrefix(libfoo, wantControl) {
		t.Errorf("libfoo2:\n%s\nwant it to start with\n%s", libfoo, wantControl)
	}
	var rest []string
	for line := range strings.Lines(strings.TrimPrefix(libfoo, wantControl)) {
		name, _, _ := strings.Cut(line, ":")
		rest = append(rest, name)
	}
	if !slices.Equal(rest, []string{"Size", "MD5sum", "SHA1", "SHA256"}) {
		t.Errorf("the fields after Filename: %q", rest)
	}
}

// TestRenderAllBeside checks a suite with all packages and concrete architectures (#725): its
// Release names all among them, with no No-Support-for-Architecture-all, so apt on a host of an
// architecture the suite has no package for reads binary-all, and finds the all packages there.
func TestRenderAllBeside(t *testing.T) {
	p := newPipeline(t)
	fixtureSet(t, p)
	jammy := render(t, p, fixedClock(p), "jammy")
	release := string(jammy[kindRelease])
	if arch := field(release, "Architectures"); arch != "all amd64 arm64" {
		t.Errorf("the Architectures of jammy: %q, want all amd64 arm64", arch)
	}
	if strings.Contains(release, "No-Support-for-Architecture-all") {
		t.Errorf("the Release of jammy says No-Support-for-Architecture-all, with which apt skips binary-all")
	}
	for _, kind := range []string{"main/binary-all/Packages", "main/binary-all/Packages.gz", "main/binary-all/Packages.xz"} {
		if _, ok := listed([]byte(release))[kind]; !ok {
			t.Errorf("the Release of jammy does not list %s", kind)
		}
	}
}

// TestRenderAllOnly checks a suite whose packages are all of architecture all: its Release
// names the architecture all, and each component has binary-all alone.
func TestRenderAllOnly(t *testing.T) {
	p := newPipeline(t)
	data, _ := testutil.Deb(testutil.DebOptions{Name: "hello-data", Arch: "all"})
	p.uploadTo(t, data, "noble", "main")
	p.uploadTo(t, data, "noble", "contrib")
	docs := render(t, p, fixedClock(p), "noble")
	kinds := slices.Sorted(maps.Keys(docs))
	want := []string{"Release", "contrib/binary-all/Packages", "contrib/binary-all/Packages.gz", "contrib/binary-all/Packages.xz",
		"main/binary-all/Packages", "main/binary-all/Packages.gz", "main/binary-all/Packages.xz"}
	if !slices.Equal(kinds, want) {
		t.Errorf("noble renders %q, want %q", kinds, want)
	}
	release := string(docs[kindRelease])
	for _, line := range []string{"Architectures: all\n", "Components: contrib main\n", "Suite: noble\n", "Codename: noble\n",
		"Date: Sun, 08 Nov 2026 09:30:00 UTC\n", "Description: apt/ubuntu noble\n", "Acquire-By-Hash: yes\n"} {
		if !strings.Contains(release, line) {
			t.Errorf("the Release has no line %q:\n%s", line, release)
		}
	}
	if got := field(string(docs["main/binary-all/Packages"]), "Filename"); got != "pool/main/h/hello-data/hello-data_1.0.0-1_all.deb" {
		t.Errorf("Filename = %q", got)
	}
}

// version is a row of a version written by a test straight into the store, without the
// pipeline: the metadata is whatever the test needs.
type version struct {
	name, version, suite, component, arch string
	metadata                              string // JSON; "" leaves versions.metadata NULL
}

// addVersion writes v into the store of p.
func (p *pipeline) addVersion(t *testing.T, v version) index.Version {
	t.Helper()
	ctx := t.Context()
	pkg := index.Package{RepoID: p.repo.ID, Name: v.name, NameOCI: profile.EscapeComponent(v.name)}
	must(t, p.st.UpsertPackage(ctx, &pkg))
	variant := variantOf(v.suite, v.component, v.arch)
	row := index.Version{PackageID: pkg.ID, Version: v.version, Variant: variant, Tag: profile.Tag(v.version, variant),
		Digest: digest.FromString(v.name + v.version + variant), ArtifactType: profile.ArtifactType(Type),
		Attrs: map[string]string{profile.AnnotationDebSuite: v.suite, profile.AnnotationDebComponent: v.component, profile.AnnotationDebArch: v.arch}}
	if v.metadata != "" {
		row.Metadata = json.RawMessage(v.metadata)
	}
	must(t, p.st.UpsertVersion(ctx, &row))
	return row
}

// metadataJSON is a deb metadata layer for a package of name, version and arch with the
// control fields extra besides those three; md5 and sha1 are left out when weak is false.
func metadataJSON(name, version, arch string, extra map[string]string, weak bool) string {
	control := map[string]string{"Package": name, "Version": version, "Architecture": arch}
	maps.Copy(control, extra)
	md := map[string]any{"control": control, "filename": Filename(name, version, arch), "md5sums": map[string]string{},
		"size": 42, "sha256": strings.Repeat("a", 64)}
	if weak {
		md["md5"], md["sha1"] = strings.Repeat("b", 32), strings.Repeat("c", 40)
	}
	b, _ := json.Marshal(md)
	return string(b)
}

// TestRenderFields checks the fields of a paragraph that the fixtures do not have: MD5sum and
// SHA1 left out when the metadata has none, fields of the file in the control file left out,
// field names in another case kept as written and ordered without regard to case, unknown
// fields sorted by name before Description, and a field without a value.
func TestRenderFields(t *testing.T) {
	p := newPipeline(t)
	p.addVersion(t, version{name: "bare", version: "1.0", suite: "sid", component: "main", arch: "amd64",
		metadata: metadataJSON("bare", "1.0", "amd64", map[string]string{"Size": "1", "SHA512": "x", "md5SUM": "y", "Filename": "evil.deb",
			"description": "short", "X-B": "2", "x-a": "1", "Empty": ""}, false)})
	docs := render(t, p, fixedClock(p), "sid")
	want := "Package: bare\nVersion: 1.0\nArchitecture: amd64\nEmpty:\nx-a: 1\nX-B: 2\ndescription: short\n" +
		"Filename: pool/main/b/bare/bare_1.0_amd64.deb\nSize: 42\nSHA256: " + strings.Repeat("a", 64) + "\n\n"
	if got := string(docs["main/binary-amd64/Packages"]); got != want {
		t.Errorf("Packages:\n%s\nwant\n%s", got, want)
	}
}

// TestRenderNotFound checks that a scope that is not a suite and a suite without a version are
// index.ErrNotFound.
func TestRenderNotFound(t *testing.T) {
	p := newPipeline(t)
	fixtureSet(t, p)
	for _, scope := range []string{"", "focal", "jammy/main/amd64", "Jammy", "../jammy"} {
		if _, err := fixedClock(p).RenderIndex(t.Context(), p.repo, format.IndexScope(scope), p.st); !errors.Is(err, index.ErrNotFound) {
			t.Errorf("RenderIndex(%q) = %v, want ErrNotFound", scope, err)
		}
	}
}

// TestRenderWithoutMetadata checks the paragraph of a version whose versions.metadata is nil:
// the renderer reads the metadata layer of its manifest from the registry, and the indexes are
// the same as with the metadata in the index.
func TestRenderWithoutMetadata(t *testing.T) {
	p := newPipeline(t)
	fixtureSet(t, p)
	pl := fixedClock(p)
	before := render(t, p, pl, "jammy")
	n := dropMetadata(t, p, "jammy")
	if n != 5 {
		t.Fatalf("dropped the metadata of %d versions of jammy, want 5", n)
	}
	after := render(t, p, pl, "jammy")
	if !maps.EqualFunc(before, after, bytes.Equal) {
		t.Errorf("the render without metadata in the index differs from the render with it")
	}
}

// dropMetadata sets versions.metadata to NULL for the versions of suite and returns how many.
func dropMetadata(t *testing.T, p *pipeline, suite string) int {
	t.Helper()
	ctx := t.Context()
	pkgs, err := p.st.ListPackages(ctx, p.repo.ID, "")
	must(t, err)
	n := 0
	for _, pkg := range pkgs {
		vs, err := p.st.ListVersions(ctx, pkg.ID)
		must(t, err)
		for _, v := range vs {
			if v.Attrs[profile.AnnotationDebSuite] == suite {
				v.Metadata = nil
				must(t, p.st.UpsertVersion(ctx, &v))
				n++
			}
		}
	}
	return n
}

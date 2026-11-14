package deb

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// fileOf is an UploadedFile over b called name.
func fileOf(name string, b []byte) format.UploadedFile {
	return format.UploadedFile{Name: name, Size: int64(len(b)),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}
}

// sourceFiles are the UploadedFiles of a source package: the .dsc, then the files it names.
func sourceFiles(d testutil.DscInfo) []format.UploadedFile {
	files := []format.UploadedFile{fileOf(d.Filename, d.Dsc)}
	for _, f := range d.Files {
		files = append(files, fileOf(f.Name, f.Content))
	}
	return files
}

// pushedOf is a file of a publication session: in the backend, with its size and four checksums
// and nothing to open.
func pushedOf(f testutil.SourceFile) format.UploadedFile {
	return format.UploadedFile{Name: f.Name, Size: f.Size,
		Checksums: map[string]string{"md5": f.MD5, "sha1": f.SHA1, "sha256": f.SHA256, "sha512": f.SHA512}}
}

// TestBuildSource builds the source packages of testutil, signed and not, with an epoch, native:
// the annotations, the variant <suite>/<component>/source and its tag, the layers (the .dsc, then
// the files in the order of Checksums-Sha256, then the metadata), the exact metadata, the blobs,
// the entry; and Validate takes the manifest and returns the same entry.
func TestBuildSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts testutil.DscOptions
		tag  string
	}{
		{"quilt", testutil.DscOptions{}, "1.0.0-1__jammy_2fmain_2fsource"},
		{"signed", testutil.DscOptions{Signed: true}, "1.0.0-1__jammy_2fmain_2fsource"},
		{"epoch", testutil.DscOptions{Source: "libfoo", Version: "1:1.2-3"}, "1_3a1.2-3__jammy_2fmain_2fsource"},
		{"native", testutil.DscOptions{Version: "2.0"}, "2.0__jammy_2fmain_2fsource"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testutil.Dsc(tc.opts)
			b, err := Plugin{}.Build(t.Context(), ubuntu, upload(sourceFiles(d)))
			if err != nil {
				t.Fatal(err)
			}
			checkSourceBuild(t, b, d, tc.tag)
			e, err := Plugin{}.Validate(t.Context(), ubuntu, b.Manifest, fetcherOf(blobsOf(t, b), nil))
			if err != nil {
				t.Fatal(err)
			}
			b.Entry.Files[0].Attrs, e.Files[0].Attrs = nil, nil
			for i := range e.Files {
				e.Files[i].Attrs = nil
			}
			if diff := cmp.Diff(b.Entry, *e); diff != "" {
				t.Errorf("Validate's entry (-Build +Validate):\n%s", diff)
			}
		})
	}
}

// checkSourceBuild checks the Build b of the source package d: the annotations, the tag and the
// path, the file entries, the metadata, and a blob for every layer.
func checkSourceBuild(t *testing.T, b *format.Built, d testutil.DscInfo, tag string) {
	t.Helper()
	parsed, err := profile.Validate(b.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	a := parsed.Annotations
	if a.Name != d.Source || a.Version != d.Version || a.Variant != "jammy/main/source" || a.UploadedBy != "user:alice" ||
		a.Attrs[profile.AnnotationDebArch] != ArchSource || b.Tag != tag || b.RepoPath != "apt/ubuntu/"+d.Source {
		t.Errorf("annotations %+v, tag %s, path %s", a, b.Tag, b.RepoPath)
	}
	want := []format.FileEntry{{Name: d.Filename, MediaType: profile.MediaTypeDebDsc, Digest: digest.FromBytes(d.Dsc), Size: d.Size}}
	for _, f := range d.Files {
		want = append(want, format.FileEntry{Name: f.Name, MediaType: profile.MediaTypeDebSource,
			Digest: digest.FromBytes(f.Content), Size: f.Size})
	}
	if diff := cmp.Diff(want, b.Entry.Files); diff != "" {
		t.Errorf("files (-want +got):\n%s", diff)
	}
	md, _ := json.Marshal(sourceMetadata{Control: d.Control, Filename: d.Filename, MD5: d.MD5, SHA1: d.SHA1, SHA256: d.SHA256, Size: d.Size})
	if string(b.Entry.Metadata) != string(md) {
		t.Errorf("metadata\n%s\nwant\n%s", b.Entry.Metadata, md)
	}
	blobs := blobsOf(t, b)
	for _, f := range append([]format.FileEntry{{Digest: parsed.Metadata.Digest}}, want...) {
		if _, ok := blobs[f.Digest]; !ok {
			t.Errorf("no blob for %s %s", f.Name, f.Digest)
		}
	}
}

// TestBuildSourceSession: the files of a publication session are in the backend already, with
// their four checksums: they are checked by those and left out of the blobs; a file the .dsc
// names and the upload lacks (the pool has it) is a layer without a blob, which ingest finds in
// the backend.
func TestBuildSourceSession(t *testing.T) {
	d := testutil.Dsc(testutil.DscOptions{})
	files := []format.UploadedFile{fileOf(d.Filename, d.Dsc), pushedOf(d.Files[1])}
	b, err := Plugin{}.Build(t.Context(), ubuntu, upload(files))
	if err != nil {
		t.Fatal(err)
	}
	blobs := blobsOf(t, b)
	for _, f := range d.Files {
		if _, ok := blobs[digest.FromBytes(f.Content)]; ok {
			t.Errorf("%s is among the blobs", f.Name)
		}
	}
	if len(b.Manifest.Layers) != 4 || b.Manifest.Layers[1].Digest != digest.FromBytes(d.Files[0].Content) {
		t.Errorf("layers %+v, want the .dsc, the two files and the metadata", b.Manifest.Layers)
	}
}

// TestBuildSourceRefuses: a source upload that is not what its .dsc says is format.ErrInvalid,
// naming the file and what is wrong with it.
func TestBuildSourceRefuses(t *testing.T) {
	d := testutil.Dsc(testutil.DscOptions{})
	orig, debian := d.Files[0], d.Files[1]
	dscFile := fileOf(d.Filename, d.Dsc)
	other := func(f testutil.SourceFile, b []byte) format.UploadedFile { return fileOf(f.Name, b) }
	session := func(f testutil.SourceFile, alg, sum string) format.UploadedFile {
		u := pushedOf(f)
		u.Checksums[alg] = sum
		return u
	}
	sized := pushedOf(debian)
	sized.Size++
	for _, tc := range []struct {
		name  string
		files []format.UploadedFile
		want  string
	}{
		{"the .dsc renamed", []format.UploadedFile{fileOf("x.dsc", d.Dsc)}, "the .dsc is called x.dsc; its Source and Version make it hello_1.0.0-1.dsc"},
		{"not a .dsc", []format.UploadedFile{fileOf(d.Filename, []byte("Package: hello\n"))}, "has no Format field"},
		{"a file it does not name", []format.UploadedFile{dscFile, fileOf("hello_9.tar.gz", []byte("x"))}, "hello_9.tar.gz is not a file hello_1.0.0-1.dsc names"},
		{"a file twice", []format.UploadedFile{dscFile, fileOf(orig.Name, orig.Content), fileOf(orig.Name, orig.Content)}, orig.Name + " is uploaded twice"},
		{"other bytes", []format.UploadedFile{dscFile, other(orig, []byte("other"))}, orig.Name + " has 5 bytes, the .dsc says"},
		{"other bytes of the size", []format.UploadedFile{dscFile, other(orig, bytes.Repeat([]byte{'x'}, int(orig.Size)))},
			"the md5 of " + orig.Name + " is"},
		{"a session's size", []format.UploadedFile{dscFile, sized}, debian.Name + " has"},
		{"a session's md5", []format.UploadedFile{dscFile, session(debian, "md5", strings.Repeat("0", 32))}, "the .dsc's Files says " + debian.MD5},
		{"a session's sha1", []format.UploadedFile{dscFile, session(debian, "sha1", strings.Repeat("0", 40))}, "the .dsc's Checksums-Sha1 says"},
		{"a session's sha256", []format.UploadedFile{dscFile, session(debian, "sha256", strings.Repeat("0", 64))}, "the .dsc's Checksums-Sha256 says"},
		{"a file without bytes", []format.UploadedFile{dscFile, {Name: orig.Name, Size: -1}}, orig.Name + " cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Plugin{}.Build(t.Context(), ubuntu, upload(tc.files))
			if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Build = %v, want ErrInvalid with %q", err, tc.want)
			}
		})
	}
}

// TestBuildRefusesArchSource: a .deb whose Architecture is source, the architecture of source
// packages, is refused, and so is a binary manifest that says it.
func TestBuildRefusesArchSource(t *testing.T) {
	deb, _ := testutil.Deb(testutil.DebOptions{Arch: ArchSource})
	_, err := Plugin{}.Build(t.Context(), ubuntu, upload([]format.UploadedFile{debFile(deb)}))
	if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), `"source" is the architecture of a source package`) {
		t.Errorf("Build = %v, want ErrInvalid naming the architecture", err)
	}
}

// sourceFixture is a manifest of a source package and its blobs, for TestValidateSourceRefuses.
func sourceFixture(t *testing.T) *fixture {
	t.Helper()
	d := testutil.Dsc(testutil.DscOptions{})
	b, err := Plugin{}.Build(t.Context(), ubuntu, upload(sourceFiles(d)))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{m: b.Manifest, blobs: blobsOf(t, b)}
}

// TestValidateSourceRefuses: Validate finds every disagreement between the .dsc, its metadata,
// its file layers and the annotations of a source manifest, each format.ErrInvalid.
func TestValidateSourceRefuses(t *testing.T) {
	good := sourceFixture(t)
	dscDigest := good.m.Layers[0].Digest
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *fixture) *fixture
		want   string
	}{
		{"the variant", func(_ *testing.T, f *fixture) *fixture {
			return f.withAnnotation(profile.AnnotationVariant, "jammy/main/amd64")
		}, `names the architecture "amd64"`},
		{"the version", func(_ *testing.T, f *fixture) *fixture {
			return f.withAnnotation(profile.AnnotationVersion, "1.0.0-2")
		}, `titled "hello_1.0.0-2.dsc"`},
		{"the .dsc not first", func(_ *testing.T, f *fixture) *fixture {
			c := f.clone()
			c.m.Layers[0], c.m.Layers[1] = c.m.Layers[1], c.m.Layers[0]
			return c
		}, "the first file layer of a deb source manifest is the .dsc"},
		{"a layer missing", func(_ *testing.T, f *fixture) *fixture {
			c := f.clone()
			c.m.Layers = append(c.m.Layers[:2], c.m.Layers[3:]...)
			return c
		}, "names hello_1.0.0-1.debian.tar.xz, which the manifest has no layer for"},
		{"a layer too many", func(_ *testing.T, f *fixture) *fixture {
			c := f.clone()
			extra := c.m.Layers[1]
			extra.Annotations = map[string]string{"org.opencontainers.image.title": "hello_x.tar.gz"}
			c.m.Layers = append(c.m.Layers[:3], extra, c.m.Layers[3])
			return c
		}, "hello_x.tar.gz is not a file hello_1.0.0-1.dsc names"},
		{"a layer of another digest", func(_ *testing.T, f *fixture) *fixture {
			c := f.clone()
			c.m.Layers[1].Digest = digest.FromString("other")
			return c
		}, "is not what hello_1.0.0-1.dsc says of it"},
		{"a .deb among the files", func(_ *testing.T, f *fixture) *fixture {
			c := f.clone()
			c.m.Layers[1].MediaType = profile.MediaTypeDebPackage
			return c
		}, "the files of a .dsc are " + profile.MediaTypeDebSource},
		{"a control field", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { control(md)["Binary"] = "other" })
		}, "the metadata control field Binary"},
		{"the md5", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { md["md5"] = strings.Repeat("0", 32) })
		}, "the metadata md5 is not the .dsc's"},
		{"the size", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { md["size"] = 1 })
		}, "the metadata size is not the .dsc layer's"},
		{"the filename", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { delete(md, "filename") })
		}, "the metadata filename is not the .dsc layer's title"},
		{"another .dsc", func(_ *testing.T, f *fixture) *fixture {
			c := f.clone()
			other := testutil.Dsc(testutil.DscOptions{Binary: "hello-bin"})
			c.blobs[dscDigest] = other.Dsc
			return c
		}, "the metadata size is not the .dsc layer's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.change(t, good)
			_, err := Plugin{}.Validate(t.Context(), ubuntu, f.m, fetcherOf(f.blobs, nil))
			if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate = %v, want ErrInvalid with %q", err, tc.want)
			}
		})
	}
}

// TestIngestSource publishes source packages through the real pipeline: hello 1.0.0-1 whole,
// then 1.0.0-2 without its .orig.tar.gz, which 1.0.0-1 holds, so that the manifest refers to the
// blob in the OCI repository of hello; a source whose .orig.tar.gz no version holds is refused
// by ingest, the blob unknown. A source version is no paragraph of Packages and makes no
// binary-source: with the binary hello in jammy/main, the suite has binary-amd64 and Sources.
func TestIngestSource(t *testing.T) {
	p := newPipeline(t)
	ctx := t.Context()
	first := testutil.Dsc(testutil.DscOptions{})
	pub, err := p.pipe.Upload(ctx, p.repo, upload(sourceFiles(first)), alice)
	if err != nil {
		t.Fatal(err)
	}
	if pub.Tag != "1.0.0-1__jammy_2fmain_2fsource" || pub.RepoPath != "apt/ubuntu/hello" {
		t.Errorf("published %s at %s", pub.Tag, pub.RepoPath)
	}
	second := testutil.Dsc(testutil.DscOptions{Version: "1.0.0-2"})
	if !bytes.Equal(second.Files[0].Content, first.Files[0].Content) {
		t.Fatal("the two revisions do not share their .orig.tar.gz")
	}
	files := []format.UploadedFile{fileOf(second.Filename, second.Dsc), fileOf(second.Files[1].Name, second.Files[1].Content)}
	if _, err := p.pipe.Upload(ctx, p.repo, upload(files), alice); err != nil {
		t.Fatalf("the second revision without its .orig.tar.gz: %v", err)
	}
	matches, err := p.st.FindFiles(ctx, p.repo.ID, first.Files[0].Name)
	must(t, err)
	if len(matches) != 2 || matches[0].File.Digest != matches[1].File.Digest {
		t.Errorf("FindFiles(%s) = %+v, want both revisions over one blob", first.Files[0].Name, matches)
	}
	lone := testutil.Dsc(testutil.DscOptions{Source: "lone"})
	if _, err := p.pipe.Upload(ctx, p.repo, upload([]format.UploadedFile{fileOf(lone.Filename, lone.Dsc)}), alice); err == nil {
		t.Error("a source whose files no version holds was published")
	}

	deb, _ := testutil.Deb(testutil.DebOptions{})
	p.upload(t, deb, "jammy")
	docs := render(t, p, fixedClock(p), "jammy")
	kinds := slices.Sorted(maps.Keys(docs))
	if want := []string{"Release", "main/binary-amd64/Packages", "main/binary-amd64/Packages.gz", "main/binary-amd64/Packages.xz",
		"main/source/Sources", "main/source/Sources.gz", "main/source/Sources.xz"}; !slices.Equal(kinds, want) {
		t.Errorf("jammy renders %q, want %q", kinds, want)
	}
	if n := len(paragraphsOf(docs["main/binary-amd64/Packages"])); n != 1 {
		t.Errorf("Packages has %d paragraphs, want the binary's alone", n)
	}
}

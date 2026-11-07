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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

var ubuntu = index.Repo{ID: 1, Format: "apt", Name: "ubuntu", Registry: "local", Prefix: "apt/ubuntu", Kind: "hosted"}

// debFile is an UploadedFile over b.
func debFile(b []byte) format.UploadedFile {
	return format.UploadedFile{Name: "upload.deb", Size: int64(len(b)), ContentType: "application/vnd.debian.binary-package",
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}
}

// upload is the Upload of files to jammy/main by user:alice; params are more name, value pairs.
func upload(files []format.UploadedFile, params ...string) format.Upload {
	p := map[string]string{ParamSuite: "jammy", ParamComponent: "main", ParamUploadedBy: "user:alice"}
	for i := 0; i+1 < len(params); i += 2 {
		p[params[i]] = params[i+1]
	}
	return format.Upload{Files: files, Params: p}
}

// blobsOf reads the blobs of a Build into a map by digest, checking each against its descriptor.
func blobsOf(t *testing.T, b *format.Built) map[digest.Digest][]byte {
	t.Helper()
	m := map[digest.Digest][]byte{}
	for _, s := range b.Blobs {
		rc, err := s.Open(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if digest.FromBytes(data) != s.Descriptor.Digest || int64(len(data)) != s.Descriptor.Size {
			t.Errorf("blob %s: the bytes are not the descriptor's", s.Descriptor.Digest)
		}
		m[s.Descriptor.Digest] = data
	}
	return m
}

// hashedTag is the hashed form of a tag longer than 128 characters (manifest-profile §4.2):
// "_h" and the first 40 hex digits of the sha256 of the unhashed tag.
func hashedTag(unhashed string) string {
	sum := sha256.Sum256([]byte(unhashed))
	return "_h" + hex.EncodeToString(sum[:20])
}

// TestBuild checks every part of a Build for one package per control compression, suite and
// tag form: the manifest passes the profile with the deb annotations, the variant
// <suite>/<component>/<arch>, the layers, the exact metadata JSON (compact, keys sorted), the
// tag of version and variant, hashed past 128 characters, the repository path, and that
// Validate accepts the manifest and returns the same entry.
func TestBuild(t *testing.T) {
	long := "1:2.0+git20261107.0123456789abcdef0123456789abcdef01234567~ubuntu22.04.1-0ubuntu1~ppa3"
	for _, tc := range []struct {
		name     string
		opts     testutil.DebOptions
		params   []string
		variant  string
		tag      string
		repoPath string
	}{
		{name: "gz", variant: "jammy/main/amd64", tag: "1.0.0-1__jammy_2fmain_2famd64", repoPath: "apt/ubuntu/hello"},
		{name: "epoch, xz", opts: testutil.DebOptions{Name: "nginx", Version: "1:1.18.0-6ubuntu14.4", ControlCompression: "xz"},
			params: []string{ParamArch, "amd64"}, variant: "jammy/main/amd64", tag: "1_3a1.18.0-6ubuntu14.4__jammy_2fmain_2famd64",
			repoPath: "apt/ubuntu/nginx"},
		{name: "all, zst", opts: testutil.DebOptions{Name: "libstdc++6", Version: "14.2.0-4ubuntu2", Arch: "all", ControlCompression: "zst"},
			variant: "jammy/main/all", tag: "14.2.0-4ubuntu2__jammy_2fmain_2fall", repoPath: "apt/ubuntu/libstdc_2b_2b6"},
		{name: "uncompressed control", opts: testutil.DebOptions{ControlCompression: "none"}, variant: "jammy/main/amd64",
			tag: "1.0.0-1__jammy_2fmain_2famd64", repoPath: "apt/ubuntu/hello"},
		{name: "another suite", params: []string{ParamSuite, "jammy-updates"}, variant: "jammy-updates/main/amd64",
			tag: "1.0.0-1__jammy-updates_2fmain_2famd64", repoPath: "apt/ubuntu/hello"},
		{name: "hashed tag", opts: testutil.DebOptions{Version: long, Arch: "arm64"},
			params: []string{ParamSuite, "jammy-backports", ParamComponent, "universe"}, variant: "jammy-backports/universe/arm64",
			tag:      hashedTag("1_3a2.0_2bgit20261107.0123456789abcdef0123456789abcdef01234567_7eubuntu22.04.1-0ubuntu1_7eppa3__jammy-backports_2funiverse_2farm64"),
			repoPath: "apt/ubuntu/hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deb, info := testutil.Deb(tc.opts)
			before := time.Now().UTC().Truncate(time.Second)
			b, err := Plugin{}.Build(t.Context(), ubuntu, upload([]format.UploadedFile{debFile(deb)}, tc.params...))
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := profile.Validate(b.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			a := parsed.Annotations
			if a.Created.Before(before) || a.Created.After(time.Now()) {
				t.Errorf("created = %v, want now", a.Created)
			}
			dist := strings.Split(tc.variant, "/")
			attrs := map[string]string{profile.AnnotationDebArch: info.Architecture, profile.AnnotationDebSuite: dist[0],
				profile.AnnotationDebComponent: dist[1]}
			wantA := profile.Annotations{Format: "apt", Repo: "ubuntu", Name: info.Package, Version: info.Version,
				Variant: tc.variant, Created: a.Created, UploadedBy: "user:alice", Attrs: attrs}
			if d := cmp.Diff(wantA, a); d != "" {
				t.Errorf("annotations (-want +got):\n%s", d)
			}
			file := profile.Layer{MediaType: profile.MediaTypeDebPackage, Digest: digest.Digest("sha256:" + info.SHA256),
				Size: info.Size, Title: info.Filename}
			if d := cmp.Diff([]profile.Layer{file}, parsed.Files); d != "" {
				t.Errorf("file layers (-want +got):\n%s", d)
			}
			if b.Tag != tc.tag || b.RepoPath != tc.repoPath {
				t.Errorf("tag %q, repo path %q; want %q, %q", b.Tag, b.RepoPath, tc.tag, tc.repoPath)
			}
			checkBlobsAndEntry(t, b, deb, info, parsed.Metadata.Digest, wantA)
		})
	}
}

// checkBlobsAndEntry checks the blobs of the Build b of the package deb, the exact metadata JSON
// (compact, keys sorted) of the layer md, the entry with the variant and attributes of a, and
// that Validate returns the same entry.
func checkBlobsAndEntry(t *testing.T, b *format.Built, deb []byte, info testutil.DebInfo, md digest.Digest, a profile.Annotations) {
	t.Helper()
	fileDigest := digest.Digest("sha256:" + info.SHA256)
	blobs := blobsOf(t, b)
	if len(blobs) != 3 || !bytes.Equal(blobs[fileDigest], deb) {
		t.Errorf("blobs: %d, the file layer's bytes are the upload's: %v", len(blobs), bytes.Equal(blobs[fileDigest], deb))
	}
	wantMD, _ := json.Marshal(map[string]any{
		"control": info.Control, "md5sums": info.MD5Sums, "size": info.Size, "filename": info.Filename,
		"md5": info.MD5, "sha1": info.SHA1, "sha256": info.SHA256,
	})
	if got := blobs[md]; !bytes.Equal(got, wantMD) {
		t.Errorf("metadata\n got %s\nwant %s", got, wantMD)
	}
	wantEntry := format.Entry{Name: info.Package, Version: info.Version, Variant: a.Variant, Attrs: a.Attrs, Metadata: wantMD,
		Files: []format.FileEntry{{Name: info.Filename, MediaType: profile.MediaTypeDebPackage, Digest: fileDigest, Size: info.Size}}}
	if d := cmp.Diff(wantEntry, b.Entry); d != "" {
		t.Errorf("entry (-want +got):\n%s", d)
	}
	entry, err := Plugin{}.Validate(t.Context(), ubuntu, b.Manifest, fetcherOf(blobs, nil))
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff(b.Entry, *entry); d != "" {
		t.Errorf("Validate entry (-Build +Validate):\n%s", d)
	}
}

// TestBuildHandWritten builds and validates the package of a control file written by hand:
// field names in another case, an unusual order, a Source with a version, no md5sums.
func TestBuildHandWritten(t *testing.T) {
	b, err := Plugin{}.Build(t.Context(), ubuntu, upload([]format.UploadedFile{debFile(debOf(t, handControl, nil, "zst"))}))
	if err != nil {
		t.Fatal(err)
	}
	if b.Tag != "2.0-1_2bb1__jammy_2fmain_2farm64" || b.RepoPath != "apt/ubuntu/hello-bin" || b.Entry.Files[0].Name != "hello-bin_2.0-1+b1_arm64.deb" {
		t.Errorf("tag %q, repo path %q, file %q", b.Tag, b.RepoPath, b.Entry.Files[0].Name)
	}
	if !bytes.Contains(b.Entry.Metadata, []byte(`"architecture":"arm64"`)) || !bytes.Contains(b.Entry.Metadata, []byte(`"md5sums":{}`)) {
		t.Errorf("metadata %s", b.Entry.Metadata)
	}
	entry, err := Plugin{}.Validate(t.Context(), ubuntu, b.Manifest, fetcherOf(blobsOf(t, b), nil))
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff(b.Entry, *entry); d != "" {
		t.Errorf("Validate entry (-Build +Validate):\n%s", d)
	}
}

// TestBuildRefuses checks the upload rules: the parameters, one file, the architecture
// parameter, the declared size, a broken package, a name too long for the registry. Every case
// is format.ErrInvalid except a file that cannot be opened.
func TestBuildRefuses(t *testing.T) {
	deb, _ := testutil.Deb(testutil.DebOptions{})
	file := debFile(deb)
	short := file
	short.Size--
	broken := debFile([]byte("!<arch>\nnot a package"))
	unopenable := file
	unopenable.Open = func() (io.ReadCloser, error) { return nil, errBoom }
	long, _ := testutil.Deb(testutil.DebOptions{Name: strings.Repeat("a", 250), Files: map[string]string{"usr/x": "x"}})
	noSuite := upload([]format.UploadedFile{file})
	delete(noSuite.Params, ParamSuite)
	for _, tc := range []struct {
		name string
		in   format.Upload
		want string
	}{
		{"no suite", noSuite, "parameter suite is missing"},
		{"bad suite", upload([]format.UploadedFile{file}, ParamSuite, "Jammy"), `parameter suite "Jammy" is not lowercase letters`},
		{"bad component", upload([]format.UploadedFile{file}, ParamComponent, "main/extra"), `parameter component "main/extra"`},
		{"no file", upload(nil), "an apt upload is one .deb, got 0 files"},
		{"two files", upload([]format.UploadedFile{file, file}), "an apt upload is one .deb, got 2 files"},
		{"other architecture", upload([]format.UploadedFile{file}, ParamArch, "arm64"),
			`the upload is for architecture "arm64", the control file of hello_1.0.0-1_amd64.deb says "amd64"`},
		{"size", upload([]format.UploadedFile{short}), "the upload says"},
		{"broken", upload([]format.UploadedFile{broken}), `"upload.deb": format: invalid: truncated archive`},
		{"name too long", upload([]format.UploadedFile{debFile(long)}), "makes the OCI repository name 261 bytes long, at most 255"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Plugin{}.Build(t.Context(), ubuntu, tc.in)
			if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build = %v, %v; want ErrInvalid containing %q", b, err, tc.want)
			}
		})
	}
	if _, err := (Plugin{}).Build(t.Context(), ubuntu, upload([]format.UploadedFile{unopenable})); !errors.Is(err, errBoom) || errors.Is(err, format.ErrInvalid) {
		t.Errorf("Build of a file that cannot be opened = %v, want errBoom and not ErrInvalid", err)
	}
}

// TestMetadataTooLarge checks that Build refuses a metadata layer Validate would not read.
func TestMetadataTooLarge(t *testing.T) {
	sums := map[string]string{}
	for i := range 8000 { // "<" is six bytes in JSON: 8000 paths make 5 MB
		sums[fmt.Sprintf("usr/share/%s/%d", strings.Repeat("<", 100), i)] = strings.Repeat("0", 32)
	}
	pkg := &Package{Filename: "big_1_all.deb", MD5Sums: sums}
	if _, err := metadataOf(pkg); !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "the metadata of big_1_all.deb has") {
		t.Errorf("metadataOf = %v, want ErrInvalid", err)
	}
}

// fixture is a manifest of Build and its blobs, which a test changes.
type fixture struct {
	m     ocispec.Manifest
	blobs map[digest.Digest][]byte
}

// newFixture builds the manifest of testutil.Deb(DebOptions{}).
func newFixture(t *testing.T) *fixture {
	t.Helper()
	deb, _ := testutil.Deb(testutil.DebOptions{})
	b, err := Plugin{}.Build(t.Context(), ubuntu, upload([]format.UploadedFile{debFile(deb)}))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{m: b.Manifest, blobs: blobsOf(t, b)}
}

// clone returns a copy of the fixture whose manifest annotations and layers can be changed.
func (f *fixture) clone() *fixture {
	m := f.m
	m.Annotations = maps.Clone(f.m.Annotations)
	m.Layers = make([]ocispec.Descriptor, len(f.m.Layers))
	for i, l := range f.m.Layers {
		l.Annotations = maps.Clone(l.Annotations)
		m.Layers[i] = l
	}
	return &fixture{m: m, blobs: maps.Clone(f.blobs)}
}

// withAnnotation returns a copy of the fixture with the manifest annotation key set to value,
// or removed when value is empty.
func (f *fixture) withAnnotation(key, value string) *fixture {
	c := f.clone()
	if value == "" {
		delete(c.m.Annotations, key)
	} else {
		c.m.Annotations[key] = value
	}
	return c
}

// withMetadata replaces the metadata layer with change applied to its decoded object.
func (f *fixture) withMetadata(t *testing.T, change func(md map[string]any)) *fixture {
	t.Helper()
	c := f.clone()
	last := len(c.m.Layers) - 1
	var md map[string]any
	if err := json.Unmarshal(c.blobs[c.m.Layers[last].Digest], &md); err != nil {
		t.Fatal(err)
	}
	change(md)
	b, err := json.Marshal(md)
	if err != nil {
		t.Fatal(err)
	}
	return c.withMetadataBytes(b)
}

// withMetadataBytes replaces the metadata layer with b.
func (f *fixture) withMetadataBytes(b []byte) *fixture {
	c := f.clone()
	last := len(c.m.Layers) - 1
	c.m.Layers[last].Digest, c.m.Layers[last].Size = digest.FromBytes(b), int64(len(b))
	c.blobs[c.m.Layers[last].Digest] = b
	return c
}

// control returns the control object of a decoded metadata object.
func control(md map[string]any) map[string]any { return md["control"].(map[string]any) }

// TestValidateRefuses checks that Validate finds every disagreement between the file layer,
// the annotations, the metadata layer and the control file of the package, and that all of
// them are format.ErrInvalid.
func TestValidateRefuses(t *testing.T) {
	good := newFixture(t)
	fileDigest := good.m.Layers[0].Digest
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *fixture) *fixture
		want   string
	}{
		{"artifact type", func(_ *testing.T, f *fixture) *fixture {
			c := f.clone()
			c.m.ArtifactType = profile.ArtifactType("raw")
			return c
		}, "format: invalid"},
		{"wrong title", func(_ *testing.T, f *fixture) *fixture {
			c := f.clone()
			c.m.Layers[0].Annotations[ocispec.AnnotationTitle] = "hello_1.0.0-2_amd64.deb"
			return c
		}, `the file layer is titled "hello_1.0.0-2_amd64.deb", not "hello_1.0.0-1_amd64.deb"`},
		{"no variant", func(_ *testing.T, f *fixture) *fixture {
			return f.withAnnotation(profile.AnnotationVariant, "")
		}, `a deb manifest has lajstrom.variant, <suite>/<component>/<arch>: "jammy/main/amd64"`},
		{"variant: the architecture alone, as in #39", func(_ *testing.T, f *fixture) *fixture {
			return f.withAnnotation(profile.AnnotationVariant, "amd64")
		}, `lajstrom.variant "amd64" is the architecture alone; the variant of a deb manifest is lajstrom.deb.suite, ` +
			`lajstrom.deb.component and lajstrom.deb.arch joined with "/": "jammy/main/amd64", and its tag 1.0.0-1__jammy_2fmain_2famd64`},
		{"variant: two parts", func(_ *testing.T, f *fixture) *fixture {
			return f.withAnnotation(profile.AnnotationVariant, "jammy/amd64")
		}, `lajstrom.variant "jammy/amd64" is not <suite>/<component>/<arch>; lajstrom.deb.suite, lajstrom.deb.component ` +
			`and lajstrom.deb.arch make it "jammy/main/amd64"`},
		{"variant: four parts", func(_ *testing.T, f *fixture) *fixture {
			return f.withAnnotation(profile.AnnotationVariant, "jammy/main/amd64/x")
		}, `lajstrom.variant "jammy/main/amd64/x" is not <suite>/<component>/<arch>`},
		{"variant: suite", func(_ *testing.T, f *fixture) *fixture {
			return f.withAnnotation(profile.AnnotationVariant, "jammy-updates/main/amd64")
		}, `lajstrom.variant "jammy-updates/main/amd64" names the suite "jammy-updates", lajstrom.deb.suite is "jammy"`},
		{"variant: component", func(_ *testing.T, f *fixture) *fixture {
			return f.withAnnotation(profile.AnnotationVariant, "jammy/universe/amd64")
		}, `lajstrom.variant "jammy/universe/amd64" names the component "universe", lajstrom.deb.component is "main"`},
		{"variant: architecture", func(_ *testing.T, f *fixture) *fixture {
			return f.withAnnotation(profile.AnnotationVariant, "jammy/main/arm64")
		}, `lajstrom.variant "jammy/main/arm64" names the architecture "arm64", lajstrom.deb.arch is "amd64"`},
		{"arch annotation", func(_ *testing.T, f *fixture) *fixture {
			return f.withAnnotation(profile.AnnotationDebArch, "arm64")
		}, `lajstrom.variant "jammy/main/amd64" names the architecture "amd64", lajstrom.deb.arch is "arm64"`},
		{"no arch annotation", func(_ *testing.T, f *fixture) *fixture {
			return f.withAnnotation(profile.AnnotationDebArch, "")
		}, `lajstrom.deb.arch "" is not an architecture name`},
		{"no suite", func(_ *testing.T, f *fixture) *fixture {
			return f.withAnnotation(profile.AnnotationDebSuite, "")
		}, "lajstrom.deb.suite is missing"},
		{"bad component", func(_ *testing.T, f *fixture) *fixture {
			return f.withAnnotation(profile.AnnotationDebComponent, "Main")
		}, `lajstrom.deb.component "Main" is not lowercase letters`},
		{"missing metadata layer", func(_ *testing.T, f *fixture) *fixture {
			c := f.clone()
			c.m.Layers = c.m.Layers[:1]
			return c
		}, "type deb requires one"},
		{"second file layer", func(_ *testing.T, f *fixture) *fixture {
			c := f.clone()
			c.m.Layers = []ocispec.Descriptor{c.m.Layers[0], c.m.Layers[0], c.m.Layers[1]}
			return c
		}, "a deb manifest has one file layer, got 2"},
		{"metadata layer too large", func(_ *testing.T, f *fixture) *fixture {
			c := f.clone()
			c.m.Layers[1].Size = maxMetadata + 1
			return c
		}, "the metadata layer has 4194305 bytes, at most 4194304"},
		{"metadata not JSON", func(_ *testing.T, f *fixture) *fixture {
			return f.withMetadataBytes([]byte("not json"))
		}, "the metadata layer is not the deb metadata object"},
		{"metadata without control", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { delete(md, "control") })
		}, "the metadata has no control object"},
		{"metadata without md5sums", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { delete(md, "md5sums") })
		}, "the metadata has no md5sums object"},
		{"metadata size", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { md["size"] = md["size"].(float64) + 1 })
		}, "the metadata size is not the file layer's"},
		{"file digest not sha256", func(_ *testing.T, f *fixture) *fixture {
			c := f.clone()
			c.m.Layers[0].Digest = digest.SHA512.FromBytes(c.blobs[fileDigest])
			return c
		}, "is not sha256, which the metadata and apt name"},
		{"metadata sha256", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { md["sha256"] = strings.Repeat("0", 64) })
		}, "the metadata sha256 is not the file layer's"},
		{"metadata filename", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { md["filename"] = "hello.deb" })
		}, `the metadata filename is not the file layer's title "hello_1.0.0-1_amd64.deb"`},
		{"metadata md5", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { md["md5"] = strings.ToUpper(md["md5"].(string)) })
		}, "is not 32 lowercase hex digits"},
		{"metadata sha1", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { md["sha1"] = "abc" })
		}, `the metadata sha1 "abc" is not 40 lowercase hex digits`},
		{"metadata without Package", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { delete(control(md), "Package") })
		}, "the metadata control has no field Package"},
		{"architecture: annotations and metadata", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { control(md)["Architecture"] = "arm64" })
		}, `the metadata control field Architecture is "arm64", lajstrom.deb.arch is "amd64"`},
		{"architecture: package and the rest", func(t *testing.T, f *fixture) *fixture {
			c := f.withMetadata(t, func(md map[string]any) {
				control(md)["Architecture"] = "arm64"
				md["filename"] = "hello_1.0.0-1_arm64.deb"
			})
			c.m.Annotations[profile.AnnotationVariant] = "jammy/main/arm64"
			c.m.Annotations[profile.AnnotationDebArch] = "arm64"
			c.m.Layers[0].Annotations[ocispec.AnnotationTitle] = "hello_1.0.0-1_arm64.deb"
			return c
		}, `the metadata control field Architecture is "arm64", the package's control file says "amd64"`},
		{"version: package and the rest", func(t *testing.T, f *fixture) *fixture {
			c := f.withMetadata(t, func(md map[string]any) {
				control(md)["Version"] = "1:1.0.0-1"
			})
			c.m.Annotations[profile.AnnotationVersion] = "1:1.0.0-1"
			return c
		}, `the metadata control field Version is "1:1.0.0-1", the package's control file says "1.0.0-1"`},
		{"metadata control has more", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { control(md)["X-Extra"] = "x" })
		}, "the metadata control has a field X-Extra that the package's control file lacks"},
		{"metadata control has less", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { delete(control(md), "Section") })
		}, "the metadata control lacks the field Section of the package's control file"},
		{"metadata md5sums", func(t *testing.T, f *fixture) *fixture {
			return f.withMetadata(t, func(md map[string]any) { md["md5sums"] = map[string]any{} })
		}, "the metadata md5sums is not the md5sums of the package (2 files, the metadata lists 0)"},
		{"package is not one", func(_ *testing.T, f *fixture) *fixture {
			c := f.clone()
			c.blobs[fileDigest] = []byte("not an archive")
			return c
		}, "the package hello_1.0.0-1_amd64.deb: format: invalid: not an ar archive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.change(t, good)
			entry, err := Plugin{}.Validate(t.Context(), ubuntu, f.m, fetcherOf(f.blobs, nil))
			if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %+v, %v; want ErrInvalid containing %q", entry, err, tc.want)
			}
		})
	}
}

// TestValidateOnlyReadsTheControlMember checks that Validate accepts a manifest whose package
// cannot be read past its control member, and one pushed without md5 and sha1 in the metadata.
func TestValidateOnlyReadsTheControlMember(t *testing.T) {
	good := newFixture(t)
	fileDigest := good.m.Layers[0].Digest
	deb := good.blobs[fileDigest]
	head := deb[:controlEnd(t, deb)]
	if _, err := (Plugin{}).Validate(t.Context(), ubuntu, good.m, fetcherOf(good.blobs, map[digest.Digest]io.Reader{fileDigest: failAfter(head)})); err != nil {
		t.Errorf("Validate with a package that fails after %d of %d bytes = %v", len(head), len(deb), err)
	}
	bare := good.withMetadata(t, func(md map[string]any) { delete(md, "md5"); delete(md, "sha1") })
	entry, err := Plugin{}.Validate(t.Context(), ubuntu, bare.m, fetcherOf(bare.blobs, nil))
	if err != nil {
		t.Fatalf("Validate without md5 and sha1 = %v", err)
	}
	if strings.Contains(string(entry.Metadata), "md5\"") || len(entry.Files) != 1 || entry.Variant != "jammy/main/amd64" {
		t.Errorf("Validate = %+v", entry)
	}
}

// TestValidateFetchFailures checks that a failure to fetch or read a blob is returned and is
// not format.ErrInvalid.
func TestValidateFetchFailures(t *testing.T) {
	good := newFixture(t)
	fileDigest, mdDigest := good.m.Layers[0].Digest, good.m.Layers[1].Digest
	for name, override := range map[string]map[digest.Digest]io.Reader{
		"metadata fetch": {mdDigest: nil},
		"metadata read":  {mdDigest: failAfter(nil)},
		"package fetch":  {fileDigest: nil},
		"package read":   {fileDigest: failAfter(good.blobs[fileDigest][:100])},
	} {
		if _, err := (Plugin{}).Validate(t.Context(), ubuntu, good.m, fetcherOf(good.blobs, override)); !errors.Is(err, errBoom) || errors.Is(err, format.ErrInvalid) {
			t.Errorf("%s fails: Validate = %v, want errBoom and not ErrInvalid", name, err)
		}
	}
	if _, err := (Plugin{}).Validate(t.Context(), ubuntu, good.m, nil); err == nil || errors.Is(err, format.ErrInvalid) {
		t.Errorf("Validate without a fetcher = %v, want an error that is not ErrInvalid", err)
	}
}

// TestPlugin checks the plugin's identity, its registration, EscapeName, and the parts that
// come with later issues.
func TestPlugin(t *testing.T) {
	if p, ok := format.Lookup("apt"); !ok || p != (Plugin{}) {
		t.Errorf("format.Lookup(apt) = %v, %v", p, ok)
	}
	if p := (Plugin{}); p.Name() != "apt" || p.ArtifactType() != "application/vnd.lajstrom.deb.v1" {
		t.Errorf("Name %q, ArtifactType %q", Plugin{}.Name(), Plugin{}.ArtifactType())
	}
	for name, want := range map[string]string{"libstdc++6": "libstdc_2b_2b6", "nginx": "nginx", "0ad": "0ad", "g++-14": "g_2b_2b-14"} {
		if got, err := (Plugin{}).EscapeName(name); err != nil || got != want {
			t.Errorf("EscapeName(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"", "a", "Nginx", "-x", "a_b", "a b"} {
		if got, err := (Plugin{}).EscapeName(name); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("EscapeName(%q) = %q, %v; want ErrInvalid", name, got, err)
		}
	}
	if _, err := (Plugin{}).RenderIndex(t.Context(), ubuntu, "jammy/main/amd64", nil); !errors.Is(err, format.ErrUnsupported) {
		t.Errorf("RenderIndex = %v, want ErrUnsupported", err)
	}
	mux := http.NewServeMux()
	deps := format.Deps{Index: struct{ index.Store }{}}
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/apt/ubuntu/dists/jammy/Release", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /apt/ubuntu/dists/jammy/Release = %d, want 404 until #40", rr.Code)
	}
}

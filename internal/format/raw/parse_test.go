package raw

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

var scratch = index.Repo{ID: 1, Format: "raw", Name: "scratch", Registry: "local", Prefix: "raw/scratch", Kind: "hosted"}

// hashed is the hashed form of a path component.
func hashed(c string) string {
	sum := sha256.Sum256([]byte(c))
	return "h0_h" + hex.EncodeToString(sum[:])[:32]
}

// giteaName is the name grammar of Gitea's and Forgejo's container registry, the distribution
// specification's of 1.0 (docs/registries/quirks.md): no "__", no "--".
var giteaName = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)

// TestEscapeNameGrammar: the package paths of raw paths that a site or a repository holds keep
// to ADR 0017, Gitea's grammar and no word that Zot's layout or Harbor's routes keep, as
// raw/<repo>/<path>.
func TestEscapeNameGrammar(t *testing.T) {
	for _, p := range []string{
		".gitignore", "_config.yml", "site/_config.yml", ".github/workflows/ci.yml", "a--b.txt", "netlify/blobs",
		"x/blobs/sha256/0f", "my.uploads", "cache/.sync", "a.sync/f", "x/index.json", "x/oci-layout", "__init__.py",
		"Docs/Readme.MD", "--", "-/-", "~user/.bashrc", "node_modules/.bin/tsc", "k8s/manifests/app.yaml", "manifests",
		"x/referrers/y", "x/tags/list", "tags/Listing.TXT", "tags/tags/list", "x/tags/list/tags/list-1",
	} {
		pkgPath, err := Plugin{}.EscapeName(p)
		if err != nil {
			t.Fatalf("EscapeName(%q): %v", p, err)
		}
		if name := "raw/scratch/" + pkgPath; !giteaName.MatchString(name) {
			t.Errorf("EscapeName(%q): %q does not match Gitea's grammar", p, name)
		}
		if strings.Contains("/"+pkgPath, "/tags/list") {
			t.Errorf("EscapeName(%q) = %q, which Harbor takes for a tag list", p, pkgPath)
		}
		for c := range strings.SplitSeq(pkgPath, "/") {
			if profile.ReservedComponent(c) {
				t.Errorf("EscapeName(%q) = %q, whose component %q is reserved", p, pkgPath, c)
			}
		}
	}
}

func TestEscapeName(t *testing.T) {
	long := strings.Repeat("x", 300)
	tests := []struct {
		name, want string
	}{
		{"Docs/Readme.MD", "docs/readme.md"},
		{"file.txt", "file.txt"},
		{"a b.txt", "a_20b.txt"},
		{"dir/aф.txt", "dir/a_d1_84.txt"},
		// An escaped component that starts with "_" breaks the path grammar, so a component
		// that starts with a non-ASCII letter is hashed (manifest-profile.md §4.3, step 3).
		{"с пробелом/файл.txt", hashed("с пробелом") + "/" + hashed("файл.txt")},
		{"tools/" + long, "tools/" + hashed(long)},
		{strings.Repeat("a", 64), strings.Repeat("a", 64)},
		{strings.Repeat("a", 65), hashed(strings.Repeat("a", 65))},
		// The limit is on the escaped form: 21 underscores escape to 63 bytes.
		{"a" + strings.Repeat("_", 21), "a" + strings.Repeat("_5f", 21)},
		{"a" + strings.Repeat("_", 22), hashed("a" + strings.Repeat("_", 22))},
		{"h0__abc", "h0_5f_5fabc"}, // never a hashed component: Escape writes no "__"
		{"h0_habc", "h0_5fhabc"},
		// ADR 0017: a run of dashes and the words of Zot's layout.
		{"a--b.txt", "a_2d_2db.txt"},
		{"netlify/blobs", "netlify/blob_73"},
		{"x/blobs/sha256/0f", "x/blob_73/sha256/0f"},
		{"x/index.json", "x/index.jso_6e"},
		{"cache/my.sync", "cache/my.syn_63"},
		// The amendment of ADR 0017: Harbor's routes.
		{"k8s/manifests/app.yaml", "k8s/manifest_73/app.yaml"},
		{"x/referrers/y", "x/referrer_73/y"},
		{"x/tags/list", "x/tag_73/list"},
		{"x/tags/listing.txt", "x/tag_73/listing.txt"},
		{"x/tags/other", "x/tags/other"},
		{".gitignore", hashed(".gitignore")},
		{"x/" + strings.Repeat("y", maxPathLen-2), "x/" + hashed(strings.Repeat("y", maxPathLen-2))},
	}
	for _, tt := range tests {
		got, err := Plugin{}.EscapeName(tt.name)
		if err != nil || got != tt.want {
			t.Errorf("EscapeName(%q) = %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}
}

func TestEscapeNameRefuses(t *testing.T) {
	for _, name := range []string{
		"", "/a", "a/", "a//b", ".", "..", "a/../b", "./a", "a/.",
		"a\x00b", "a\x1fb", "a\x7fb", "a\nb", "\xff", "a/\xc3",
		strings.Repeat("a", maxPathLen+1),
	} {
		if got, err := (Plugin{}).EscapeName(name); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("EscapeName(%q) = %q, %v; want ErrInvalid", name, got, err)
		}
	}
}

// TestRepoPath checks the OCI repository name raw/<repo>/<path>. Its length is the registry's
// to bound, with the instance prefix: ingest refuses a longer one (TestNameLength).
func TestRepoPath(t *testing.T) {
	long := strings.Repeat(strings.Repeat("a", 60)+"/", 4) + "a" // 245 bytes
	pkgPath, name, err := repoPath(scratch, long)
	if err != nil || pkgPath != long || name != "raw/scratch/"+long {
		t.Errorf("repoPath(245 bytes) = %q, %q, %v", pkgPath, name, err)
	}
	// Hashing keeps a long component short, so its length alone never makes a long name.
	if _, name, err := repoPath(scratch, "docs/"+strings.Repeat("z", 1000)); err != nil || len(name) != len("raw/scratch/docs/")+36 {
		t.Errorf("repoPath of a 1000-byte component = %q, %v", name, err)
	}
}

// memFile is an UploadedFile over content.
func memFile(name, contentType, content string) format.UploadedFile {
	return format.UploadedFile{Name: name, Size: int64(len(content)), ContentType: contentType,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(content)), nil }}
}

// upload is the Upload of one file at path p.
func upload(p string, f ...format.UploadedFile) format.Upload {
	return format.Upload{Files: f, Params: map[string]string{ParamPath: p, ParamUploadedBy: "user:alice"}}
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

// fetcherOf is a BlobFetcher over blobs.
func fetcherOf(blobs map[digest.Digest][]byte) format.BlobFetcher {
	return func(_ context.Context, d digest.Digest) (io.ReadCloser, error) {
		b, ok := blobs[d]
		if !ok {
			return nil, fmt.Errorf("no blob %s", d)
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}

// TestBuild checks every part of a Build: the manifest passes the profile with the raw
// annotations, the blobs are config, file and metadata, the metadata is the exact JSON, and
// Validate accepts the manifest and returns the same entry.
func TestBuild(t *testing.T) {
	const p, content = "Docs/Readme.MD", "hello raw\n"
	sum := digest.FromString(content)
	before := time.Now().UTC().Truncate(time.Second)
	b, err := Plugin{}.Build(t.Context(), scratch, upload(p, memFile(p, "text/markdown", content)))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := profile.Validate(b.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if c := parsed.Annotations.Created; c.Before(before) || c.After(time.Now()) {
		t.Errorf("created = %v, want now", c)
	}
	version := sum.Encoded()[:16]
	type summary struct {
		Tag, RepoPath, Format, Repo, Name, Version, Variant, UploadedBy string
		Attrs                                                           map[string]string
		Titles                                                          []string
		File                                                            digest.Digest
	}
	got := summary{b.Tag, b.RepoPath, parsed.Annotations.Format, parsed.Annotations.Repo, parsed.Annotations.Name,
		parsed.Annotations.Version, parsed.Annotations.Variant, parsed.Annotations.UploadedBy, parsed.Annotations.Attrs,
		[]string{parsed.Files[0].Title, parsed.Metadata.Title}, parsed.Files[0].Digest}
	want := summary{version, "raw/scratch/docs/readme.md", "raw", "scratch", p, version, "", "user:alice",
		map[string]string{profile.AnnotationRawPath: p}, []string{p, "metadata.json"}, sum}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("Build (-want +got):\n%s", d)
	}
	blobs := blobsOf(t, b)
	wantBlobs := map[digest.Digest][]byte{
		profile.EmptyConfig.Digest: []byte("{}"),
		sum:                        []byte(content),
		parsed.Metadata.Digest:     []byte(`{"path":"Docs/Readme.MD","contentType":"text/markdown","size":10,"sha256":"` + sum.Encoded() + `"}`),
	}
	if d := cmp.Diff(wantBlobs, blobs); d != "" || string(b.Entry.Metadata) != string(wantBlobs[parsed.Metadata.Digest]) {
		t.Errorf("blobs (-want +got):\n%s\nentry metadata %s", d, b.Entry.Metadata)
	}
	entry, err := Plugin{}.Validate(t.Context(), scratch, b.Manifest, fetcherOf(blobs))
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff(&b.Entry, entry); d != "" {
		t.Errorf("Validate of the built manifest (-Build +Validate):\n%s", d)
	}
}

// TestBuildDefaults checks the default content type, an empty file and no uploaded-by.
func TestBuildDefaults(t *testing.T) {
	up := upload("empty", memFile("empty", "", ""))
	delete(up.Params, ParamUploadedBy)
	b, err := Plugin{}.Build(t.Context(), scratch, up)
	if err != nil {
		t.Fatal(err)
	}
	empty := digest.FromString("")
	want := `{"path":"empty","contentType":"application/octet-stream","size":0,"sha256":"` + empty.Encoded() + `"}`
	if string(b.Entry.Metadata) != want || b.Entry.Files[0].Size != 0 || b.Tag != empty.Encoded()[:16] {
		t.Errorf("Build of an empty file: %s, %+v, %s", b.Entry.Metadata, b.Entry.Files, b.Tag)
	}
	if _, ok := b.Manifest.Annotations[profile.AnnotationUploadedBy]; ok {
		t.Errorf("annotations = %v, want no %s", b.Manifest.Annotations, profile.AnnotationUploadedBy)
	}
}

func TestBuildRefuses(t *testing.T) {
	failing := format.UploadedFile{Name: "a", Size: -1, Open: func() (io.ReadCloser, error) { return nil, errors.New("gone") }}
	short := memFile("a", "", "abc")
	short.Size = 4
	tests := []struct {
		name    string
		up      format.Upload
		invalid bool
	}{
		{"no file", upload("a"), true},
		{"two files", upload("a", memFile("a", "", "1"), memFile("b", "", "2")), true},
		{"bad path", upload("a/../b", memFile("a", "", "1")), true},
		{"no path", upload("", memFile("a", "", "1")), true},
		{"too long for the OCI name", upload(strings.Repeat(strings.Repeat("a", 60)+"/", 4), memFile("a", "", "1")), true},
		{"size other than declared", upload("a", short), true},
		{"open fails", upload("a", failing), false},
	}
	for _, tt := range tests {
		b, err := Plugin{}.Build(t.Context(), scratch, tt.up)
		if err == nil || errors.Is(err, format.ErrInvalid) != tt.invalid {
			t.Errorf("%s: Build = %v, %v; want an error, ErrInvalid %v", tt.name, b, err, tt.invalid)
		}
	}
}

// fixture is a raw manifest under construction, for the Validate tests: what Build makes,
// before profile.Build.
type fixture struct {
	ann   profile.Annotations
	files []profile.Layer
	md    []byte // the metadata layer; nil for none
	mdLen int64  // the metadata layer size in the manifest; 0 means len(md)
}

// newFixture is the manifest Build makes for content at p.
func newFixture(p, content string) *fixture {
	d := digest.FromString(content)
	return &fixture{
		ann: profile.Annotations{Format: "raw", Repo: "scratch", Name: p, Version: d.Encoded()[:16],
			Created: time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC), Attrs: map[string]string{profile.AnnotationRawPath: p}},
		files: []profile.Layer{{MediaType: profile.MediaTypeRawFile, Digest: d, Size: int64(len(content)), Title: p}},
		md:    []byte(`{"path":"` + p + `","contentType":"text/plain","size":` + fmt.Sprint(len(content)) + `,"sha256":"` + d.Encoded() + `"}`),
	}
}

// build returns the manifest and the fetcher of its metadata.
func (f *fixture) build(t *testing.T) (ocispec.Manifest, format.BlobFetcher) {
	t.Helper()
	var md *profile.Layer
	blobs := map[digest.Digest][]byte{}
	if f.md != nil {
		size := f.mdLen
		if size == 0 {
			size = int64(len(f.md))
		}
		md = &profile.Layer{Digest: digest.FromBytes(f.md), Size: size}
		blobs[md.Digest] = f.md
	}
	m, err := profile.Build(f.ann, f.files, md)
	if err != nil {
		t.Fatal(err)
	}
	return m, fetcherOf(blobs)
}

func TestValidateRefuses(t *testing.T) {
	const p, content = "docs/file.txt", "hello"
	other := digest.FromString("other")
	tests := []struct {
		name    string
		edit    func(f *fixture)
		invalid bool
	}{
		{"title is not the name", func(f *fixture) { f.files[0].Title = "file.txt" }, true},
		{"raw.path is not the name", func(f *fixture) { f.ann.Attrs[profile.AnnotationRawPath] = "docs/other.txt" }, true},
		{"no raw.path", func(f *fixture) { f.ann.Attrs = nil }, true},
		{"name breaks the path rules", func(f *fixture) {
			f.ann.Name, f.files[0].Title, f.ann.Attrs[profile.AnnotationRawPath] = "a/../b", "a/../b", "a/../b"
		}, true},
		{"wrong version", func(f *fixture) { f.ann.Version = other.Encoded()[:16] }, true},
		{"version of the full digest", func(f *fixture) { f.ann.Version = digest.FromString(content).Encoded() }, true},
		{"variant", func(f *fixture) { f.ann.Variant = "amd64" }, true},
		{"two file layers", func(f *fixture) { f.files = append(f.files, f.files[0]) }, true},
		{"sha512 layer", func(f *fixture) { f.files[0].Digest = digest.SHA512.FromString(content) }, true},
		{"metadata path", func(f *fixture) { f.md = bytes.Replace(f.md, []byte(p), []byte("docs/x"), 1) }, true},
		{"metadata size", func(f *fixture) { f.md = bytes.Replace(f.md, []byte(`"size":5`), []byte(`"size":6`), 1) }, true},
		{"metadata sha256", func(f *fixture) {
			f.md = bytes.Replace(f.md, []byte(digest.FromString(content).Encoded()), []byte(other.Encoded()), 1)
		}, true},
		{"metadata without size", func(f *fixture) { f.md = bytes.Replace(f.md, []byte(`"size":5,`), nil, 1) }, true},
		{"metadata contentType not a string", func(f *fixture) {
			f.md = bytes.Replace(f.md, []byte(`"text/plain"`), []byte(`5`), 1)
		}, true},
		{"metadata not an object", func(f *fixture) { f.md = []byte(`["docs/file.txt"]`) }, true},
		{"metadata too large", func(f *fixture) { f.mdLen = maxMetadata + 1 }, true},
		{"metadata fetch fails", func(f *fixture) { f.mdLen = int64(len(f.md)) + 1 }, false}, // another digest than the fetcher has
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(p, content)
			tt.edit(f)
			m, fetch := f.build(t)
			if tt.name == "metadata fetch fails" {
				m.Layers[1].Digest = other
			}
			e, err := Plugin{}.Validate(t.Context(), scratch, m, fetch)
			if err == nil || errors.Is(err, format.ErrInvalid) != tt.invalid {
				t.Errorf("Validate = %+v, %v; want an error, ErrInvalid %v", e, err, tt.invalid)
			}
		})
	}
}

// TestValidateAccepts checks a manifest without a metadata layer (oras push may leave it out),
// one whose metadata has another contentType and an unknown key, and that the profile errors
// are ErrInvalid.
func TestValidateAccepts(t *testing.T) {
	f := newFixture("bin/tool", "binary")
	f.md = nil
	m, fetch := f.build(t)
	e, err := Plugin{}.Validate(t.Context(), scratch, m, fetch)
	if err != nil || e.Metadata != nil || e.Name != "bin/tool" || len(e.Files) != 1 {
		t.Errorf("Validate without metadata = %+v, %v", e, err)
	}
	if _, err := (Plugin{}).Validate(t.Context(), scratch, m, nil); err != nil {
		t.Errorf("Validate without metadata and without a fetcher: %v", err)
	}

	f = newFixture("bin/tool", "binary")
	f.md = bytes.Replace(f.md, []byte(`"text/plain"`), []byte(`"application/x-elf","note":"x"`), 1)
	m, fetch = f.build(t)
	if e, err := (Plugin{}).Validate(t.Context(), scratch, m, fetch); err != nil || string(e.Metadata) != string(f.md) {
		t.Errorf("Validate with another contentType = %+v, %v", e, err)
	}
	if _, err := (Plugin{}).Validate(t.Context(), scratch, m, nil); err == nil || errors.Is(err, format.ErrInvalid) {
		t.Errorf("Validate of a metadata layer without a fetcher: %v, want a plain error", err)
	}

	m.Config.Digest = digest.FromString("{}x")
	if _, err := (Plugin{}).Validate(t.Context(), scratch, m, fetch); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("Validate of a manifest the profile refuses: %v, want ErrInvalid", err)
	}
}

func TestContentTypeOf(t *testing.T) {
	for md, want := range map[string]string{
		``:                             "application/octet-stream",
		`{"contentType":"text/plain"}`: "text/plain",
		`{"contentType":""}`:           "application/octet-stream",
		`{"files":[]}`:                 "application/octet-stream",
		`not json`:                     "application/octet-stream",
	} {
		if got := contentTypeOf([]byte(md)); got != want {
			t.Errorf("contentTypeOf(%s) = %q, want %q", md, got, want)
		}
	}
}

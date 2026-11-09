package maven

import (
	"bytes"
	"context"
	"crypto/md5"  //nolint:gosec // maven names md5 checksums; not a security use
	"crypto/sha1" //nolint:gosec // maven names sha1 checksums; not a security use
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

var repo = index.Repo{ID: 1, Format: Name, Name: "releases", Registry: "local", Prefix: "maven/releases", Kind: "hosted"}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// upFile is the uploaded file of path with the bytes b.
func upFile(path string, b []byte) format.UploadedFile {
	return format.UploadedFile{Name: path, Size: int64(len(b)), Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}
}

// sumsOf returns the four checksums of b as the metadata layer writes them, computed apart
// from the code under test.
func sumsOf(b []byte) map[string]string {
	return map[string]string{
		MD5: fmt.Sprintf("%x", md5.Sum(b)), SHA1: fmt.Sprintf("%x", sha1.Sum(b)), //nolint:gosec // maven checksums
		SHA256: fmt.Sprintf("%x", sha256.Sum256(b)), SHA512: fmt.Sprintf("%x", sha512.Sum512(b)),
	}
}

// fromTestutil returns the checksums testutil reports for a fixture, as
// format.UploadedFile.Checksums holds them.
func fromTestutil(c testutil.Checksums) map[string]string {
	return map[string]string{MD5: c.MD5, SHA1: c.SHA1, SHA256: c.SHA256, SHA512: c.SHA512}
}

// session is upFile as a publication session hands it over: with the four checksums the
// session computed, and an Open that fails the test unless open is true (the POM).
func session(t *testing.T, path string, b []byte, sums testutil.Checksums, open bool) format.UploadedFile {
	f := upFile(path, b)
	f.Checksums = fromTestutil(sums)
	if !open {
		f.Open = func() (io.ReadCloser, error) {
			t.Errorf("Build opened %s, which came with its checksums", path)
			return nil, errors.New("not to be opened")
		}
	}
	return f
}

// fetcher serves the blobs of built and the blobs of the registry, and records what it serves.
func fetcher(t *testing.T, built *format.Built, registry map[digest.Digest][]byte, fetched *[]digest.Digest) format.BlobFetcher {
	t.Helper()
	blobs := map[digest.Digest][]byte{}
	for d, b := range registry {
		blobs[d] = b
	}
	for _, b := range built.Blobs {
		rc, err := b.Open(t.Context())
		must(t, err)
		data, err := io.ReadAll(rc)
		must(t, err)
		_ = rc.Close()
		blobs[b.Descriptor.Digest] = data
	}
	return func(_ context.Context, d digest.Digest) (io.ReadCloser, error) {
		if fetched != nil {
			*fetched = append(*fetched, d)
		}
		b, ok := blobs[d]
		if !ok {
			return nil, fmt.Errorf("no blob %s", d)
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}

func TestEscapeName(t *testing.T) {
	for in, want := range map[string]string{
		"com.example:lib-core": "com.example/lib-core", "Com.Example:Lib-Core": "com.example/lib-core",
		"junit:junit": "junit/junit", "org.eclipse.jdt:org.eclipse.jdt.core": "org.eclipse.jdt/org.eclipse.jdt.core",
		"com.example:lib_core": "com.example/lib_5fcore",
	} {
		if got, err := (Plugin{}).EscapeName(in); err != nil || got != want {
			t.Errorf("EscapeName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"lib-core", "com/example:lib-core", "com.example:", ":lib-core", "com.example:lib:core", "com.example:.."} {
		if _, err := (Plugin{}).EscapeName(in); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("EscapeName(%q): %v, want ErrInvalid", in, err)
		}
	}
	if (Plugin{}).Name() != "maven" || (Plugin{}).ArtifactType() != "application/vnd.lajstrom.maven.v1" {
		t.Errorf("Name, ArtifactType = %q, %q", (Plugin{}).Name(), (Plugin{}).ArtifactType())
	}
}

// fixture is a version to build: its files, and what the manifest must say.
type fixture struct {
	name      string
	files     []format.UploadedFile
	c         Coordinates
	packaging string
	// layers are the titles in manifest order, with the classifier and extension of each.
	layers [][3]string
	parent *Coordinates
	deps   []Dependency
}

// fixtures are the versions of the acceptance criteria of #52, built with testutil: a jar and
// its POM with dependencies; classifiers and signatures; a pom-packaging project, the POM alone;
// a version inherited from the parent; a tar.gz with a classifier under a dotted artifactId.
func fixtures() []fixture {
	jar, ji := testutil.Jar(testutil.JarOptions{})
	src, si := testutil.Jar(testutil.JarOptions{Classifier: "sources"})
	doc, di := testutil.Jar(testutil.JarOptions{Classifier: "javadoc"})
	deps := []testutil.DependencyOptions{{GroupID: "com.example", ArtifactID: "lib-api", Version: "1.0.0"}, {GroupID: "org.junit.jupiter", ArtifactID: "junit-jupiter", Version: "5.11.3", Scope: "test"}}
	pom, pi := testutil.POM(testutil.POMOptions{Dependencies: deps})
	bom, bi := testutil.POM(testutil.POMOptions{ArtifactID: "lib-bom", Packaging: "pom"})
	dist, di2 := testutil.POM(testutil.POMOptions{GroupID: "org.example", ArtifactID: "org.example.dist", Version: "3.0", Packaging: "pom"})
	inherited := project(`<parent><groupId>com.example</groupId><artifactId>lib-parent</artifactId><version>1.2.0</version></parent><artifactId>lib-core</artifactId>`)
	sig := []byte("-----BEGIN PGP SIGNATURE-----\n\n-----END PGP SIGNATURE-----\n")
	lib := Coordinates{GroupID: "com.example", ArtifactID: "lib-core", Version: "1.2.0"}
	return []fixture{
		{name: "jar and POM", files: []format.UploadedFile{upFile(ji.Path, jar), upFile(pi.Path, pom)}, c: lib, packaging: "jar",
			layers: [][3]string{{"lib-core-1.2.0.jar", "", "jar"}, {"lib-core-1.2.0.pom", "", "pom"}},
			deps:   []Dependency{{GroupID: "com.example", ArtifactID: "lib-api", Version: "1.0.0"}, {GroupID: "org.junit.jupiter", ArtifactID: "junit-jupiter", Version: "5.11.3", Scope: "test"}}},
		{name: "classifiers and signatures", c: lib, packaging: "jar", deps: []Dependency{{GroupID: "com.example", ArtifactID: "lib-api", Version: "1.0.0"}, {GroupID: "org.junit.jupiter", ArtifactID: "junit-jupiter", Version: "5.11.3", Scope: "test"}},
			files: []format.UploadedFile{upFile(pi.Path, pom), upFile(ji.Path, jar), upFile(si.Path, src), upFile(di.Path, doc),
				upFile(ji.Path+".asc", sig), upFile(pi.Path+".asc", sig), upFile(si.Path+".asc", sig)},
			layers: [][3]string{{"lib-core-1.2.0-javadoc.jar", "javadoc", "jar"}, {"lib-core-1.2.0-sources.jar", "sources", "jar"},
				{"lib-core-1.2.0-sources.jar.asc", "sources", "jar.asc"}, {"lib-core-1.2.0.jar", "", "jar"}, {"lib-core-1.2.0.jar.asc", "", "jar.asc"},
				{"lib-core-1.2.0.pom", "", "pom"}, {"lib-core-1.2.0.pom.asc", "", "pom.asc"}}},
		{name: "pom packaging only", files: []format.UploadedFile{upFile(bi.Path, bom)}, c: Coordinates{"com.example", "lib-bom", "1.2.0"}, packaging: "pom",
			layers: [][3]string{{"lib-bom-1.2.0.pom", "", "pom"}}},
		{name: "version from the parent", files: []format.UploadedFile{upFile(ji.Path, jar), upFile(pi.Path, inherited)}, c: lib, packaging: "jar",
			layers: [][3]string{{"lib-core-1.2.0.jar", "", "jar"}, {"lib-core-1.2.0.pom", "", "pom"}},
			parent: &Coordinates{GroupID: "com.example", ArtifactID: "lib-parent", Version: "1.2.0"}},
		{name: "tar.gz under a dotted artifactId", c: Coordinates{"org.example", "org.example.dist", "3.0"}, packaging: "pom",
			files:  []format.UploadedFile{upFile(di2.Path, dist), upFile("org/example/org.example.dist/3.0/org.example.dist-3.0-bin.tar.gz", []byte("tar.gz bytes"))},
			layers: [][3]string{{"org.example.dist-3.0-bin.tar.gz", "bin", "tar.gz"}, {"org.example.dist-3.0.pom", "", "pom"}}},
	}
}

// want returns the file layers and the metadata layer the fixture must build: a layer per title
// of tt.layers, digested from the bytes of its file, the POM typed pom without annotations and
// any other file typed artifact with its extension and classifier, and the metadata with the
// four checksums of each file computed apart from the code under test.
func (tt fixture) want(t *testing.T) ([]ocispec.Descriptor, metadata) {
	t.Helper()
	byPath := map[string][]byte{}
	for _, f := range tt.files {
		rc, err := f.Open()
		must(t, err)
		byPath[f.Name], err = io.ReadAll(rc)
		must(t, err)
	}
	md := metadata{GroupID: tt.c.GroupID, ArtifactID: tt.c.ArtifactID, Version: tt.c.Version, Packaging: tt.packaging,
		POM: pomEssentials{Dependencies: tt.deps, Parent: tt.parent}}
	if md.POM.Dependencies == nil {
		md.POM.Dependencies = []Dependency{}
	}
	var layers []ocispec.Descriptor
	for _, l := range tt.layers {
		title, classifier, ext := l[0], l[1], l[2]
		b := byPath[tt.c.Dir()+"/"+title]
		d := ocispec.Descriptor{MediaType: profile.MediaTypeMavenArtifact, Digest: digest.FromBytes(b), Size: int64(len(b)),
			Annotations: map[string]string{ocispec.AnnotationTitle: title, profile.AnnotationMavenExtension: ext}}
		if classifier != "" {
			d.Annotations[profile.AnnotationMavenClassifier] = classifier
		}
		if ext == "pom" {
			d.MediaType, d.Annotations = profile.MediaTypeMavenPom, map[string]string{ocispec.AnnotationTitle: title}
		}
		layers = append(layers, d)
		f := fileMetadata{Name: title, Classifier: classifier, Extension: ext, Size: int64(len(b))}
		for alg, v := range sumsOf(b) {
			*f.checksum(alg) = v
		}
		md.Files = append(md.Files, f)
	}
	return layers, md
}

// TestBuildValidate builds each fixture and checks the manifest: the annotations, the tag, the
// path, the layers in order of file name with their media types and annotations, the metadata
// layer with the four checksums of every file and the POM's parent and dependencies; then
// Validate of the same manifest returns the same entry.
func TestBuildValidate(t *testing.T) {
	for _, tt := range fixtures() {
		t.Run(tt.name, func(t *testing.T) {
			built, err := Plugin{}.Build(t.Context(), repo, format.Upload{Files: tt.files, Params: map[string]string{ParamUploadedBy: "user:alice"}})
			must(t, err)
			a := built.Manifest.Annotations
			for k, want := range map[string]string{profile.AnnotationName: tt.c.Name(), profile.AnnotationVersion: tt.c.Version,
				profile.AnnotationFormat: Name, profile.AnnotationRepo: "releases", profile.AnnotationUploadedBy: "user:alice",
				profile.AnnotationMavenGroupID: tt.c.GroupID, profile.AnnotationMavenArtifactID: tt.c.ArtifactID, profile.AnnotationMavenPackaging: tt.packaging,
				profile.AnnotationVariant: ""} {
				if a[k] != want {
					t.Errorf("%s = %q, want %q", k, a[k], want)
				}
			}
			if pkg := packagePath(tt.c.GroupID, tt.c.ArtifactID); built.Tag != tt.c.Version || built.RepoPath != "maven/releases/"+pkg {
				t.Errorf("tag %q, path %q", built.Tag, built.RepoPath)
			}
			layers, want := tt.want(t)
			if d := cmp.Diff(layers, built.Manifest.Layers[:len(built.Manifest.Layers)-1]); d != "" {
				t.Errorf("file layers (-want +got)\n%s", d)
			}
			var md metadata
			must(t, json.Unmarshal(built.Entry.Metadata, &md))
			if d := cmp.Diff(want, md); d != "" {
				t.Errorf("metadata (-want +got)\n%s", d)
			}
			if n := len(built.Blobs); n != len(tt.files)+2 {
				t.Errorf("%d blobs, want the config, %d files and the metadata", n, len(tt.files))
			}
			entry, err := Plugin{}.Validate(t.Context(), repo, built.Manifest, fetcher(t, built, nil, nil))
			must(t, err)
			if d := cmp.Diff(&built.Entry, entry); d != "" {
				t.Errorf("Validate: (-Build +Validate)\n%s", d)
			}
		})
	}
}

// TestBuildChecksums builds a version as a publication session hands it over, every file with
// the four checksums the session computed while it pushed the file: Build opens the POM and not
// the jar, describes the jar's layer from its checksums, and leaves both out of Built.Blobs,
// which are the config and the metadata layer. Validate reads the metadata layer and the POM
// and not the jar.
func TestBuildChecksums(t *testing.T) {
	ctx := t.Context()
	jar, ji := testutil.Jar(testutil.JarOptions{})
	pom, pi := testutil.POM(testutil.POMOptions{})
	built, err := Plugin{}.Build(ctx, repo, format.Upload{Files: []format.UploadedFile{
		session(t, ji.Path, jar, ji.Checksums, false), session(t, pi.Path, pom, pi.Checksums, true)}})
	must(t, err)
	if l := built.Manifest.Layers[0]; l.Digest.Encoded() != ji.SHA256 || l.Size != ji.Size {
		t.Errorf("jar layer %s, %d bytes; want sha256:%s, %d", l.Digest, l.Size, ji.SHA256, ji.Size)
	}
	var blobs []string
	for _, b := range built.Blobs {
		blobs = append(blobs, b.Descriptor.MediaType)
	}
	if want := []string{ocispec.MediaTypeEmptyJSON, profile.MetadataMediaType(Name)}; !slices.Equal(blobs, want) {
		t.Errorf("blobs %v, want %v", blobs, want)
	}
	var md metadata
	must(t, json.Unmarshal(built.Entry.Metadata, &md))
	for _, f := range md.Files {
		want := fromTestutil(pi.Checksums)
		if f.Name == ji.Filename {
			want = fromTestutil(ji.Checksums)
		}
		if got := (map[string]string{MD5: f.MD5, SHA1: f.SHA1, SHA256: f.SHA256, SHA512: f.SHA512}); !cmp.Equal(got, want) {
			t.Errorf("metadata checksums of %s = %v, want %v", f.Name, got, want)
		}
	}
	var fetched []digest.Digest
	_, err = Plugin{}.Validate(ctx, repo, built.Manifest, fetcher(t, built, map[digest.Digest][]byte{digest.FromBytes(pom): pom}, &fetched))
	must(t, err)
	if want := []digest.Digest{built.Manifest.Layers[2].Digest, digest.FromBytes(pom)}; !slices.Equal(fetched, want) {
		t.Errorf("Validate fetched %v, want the metadata layer and the POM %v", fetched, want)
	}
}

// TestBuildChecksumsRead: checksums that do not match the bytes Build reads are refused, and a
// file with some of them, or without a size, is read and pushed.
func TestBuildChecksumsRead(t *testing.T) {
	ctx := t.Context()
	jar, ji := testutil.Jar(testutil.JarOptions{})
	pom, pi := testutil.POM(testutil.POMOptions{})
	wrongPOM := session(t, pi.Path, pom, ji.Checksums, true)
	someWrong := upFile(ji.Path, jar)
	someWrong.Checksums = map[string]string{SHA1: strings.Repeat("0", 40)}
	upper := session(t, ji.Path, jar, ji.Checksums, false)
	upper.Checksums[SHA256] = strings.ToUpper(ji.SHA256)
	for _, tt := range []struct {
		name string
		jar  format.UploadedFile
		pom  format.UploadedFile
		msg  string
	}{
		{"a POM that is not its checksums", upFile(ji.Path, jar), wrongPOM, "the md5 of " + pi.Path},
		{"a jar with a wrong sha1", someWrong, upFile(pi.Path, pom), "the sha1 of " + ji.Path},
		{"checksums in upper case", upper, upFile(pi.Path, pom), "lowercase hex"},
	} {
		_, err := Plugin{}.Build(ctx, repo, format.Upload{Files: []format.UploadedFile{tt.jar, tt.pom}})
		if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: %v, want ErrInvalid with %q", tt.name, err, tt.msg)
		}
	}

	partial := upFile(ji.Path, jar)
	partial.Checksums = map[string]string{SHA1: ji.SHA1}
	noSize := session(t, ji.Path, jar, ji.Checksums, true)
	noSize.Size = -1
	for _, f := range []format.UploadedFile{partial, noSize} {
		built, err := Plugin{}.Build(ctx, repo, format.Upload{Files: []format.UploadedFile{f, upFile(pi.Path, pom)}})
		must(t, err)
		if len(built.Blobs) != 4 || built.Blobs[1].Descriptor.Digest.Encoded() != ji.SHA256 || built.Blobs[1].Descriptor.Size != ji.Size {
			t.Errorf("checksums %v, size %d: blobs %+v, want the jar read and pushed", f.Checksums, f.Size, built.Blobs)
		}
	}
}

// TestBuildRefuses is the list of the rules of #52 on the upload path, each with a part of the
// message that names it; a ${revision} POM is refused as a CI-friendly version.
func TestBuildRefuses(t *testing.T) {
	jar, ji := testutil.Jar(testutil.JarOptions{})
	pom, pi := testutil.POM(testutil.POMOptions{})
	other, oi := testutil.POM(testutil.POMOptions{Version: "1.3.0"})
	long, li := testutil.POM(testutil.POMOptions{GroupID: strings.Repeat("g", 250)})
	revision := project(`<groupId>com.example</groupId><artifactId>lib-core</artifactId><version>${revision}</version>`)
	wrongSize := upFile(ji.Path, jar)
	wrongSize.Size = 3
	for _, tt := range []struct {
		name  string
		files []format.UploadedFile
		msg   string
	}{
		{"no files", nil, "this one none"},
		{"no POM", []format.UploadedFile{upFile(ji.Path, jar)}, "has no POM, lib-core-1.2.0.pom"},
		{"two versions", []format.UploadedFile{upFile(ji.Path, jar), upFile(oi.Path, other)}, "one upload is one version"},
		{"a checksum file", []format.UploadedFile{upFile(pi.Path, pom), upFile(ji.Path+".sha1", []byte(ji.SHA1))}, "sha1 checksum file"},
		{"maven-metadata.xml", []format.UploadedFile{upFile(pi.Path, pom), upFile("com/example/lib-core/maven-metadata.xml", nil)}, "is a maven-metadata.xml"},
		{"a file twice", []format.UploadedFile{upFile(pi.Path, pom), upFile(ji.Path, jar), upFile(ji.Path, jar)}, "lib-core-1.2.0.jar twice"},
		{"a POM of another version", []format.UploadedFile{upFile(pi.Path, other)}, "the POM is of com.example:lib-core:1.3.0, its path of com.example:lib-core:1.2.0"},
		{"${revision}", []format.UploadedFile{upFile(pi.Path, revision)}, "${revision} is a CI-friendly version"},
		{"a jar as the POM", []format.UploadedFile{upFile(pi.Path, jar)}, "not a POM"},
		{"a SNAPSHOT", []format.UploadedFile{upFile("com/example/lib-core/1.2.0-SNAPSHOT/lib-core-1.2.0-SNAPSHOT.pom", pom)}, "SNAPSHOT"},
		{"a size that is not the declared one", []format.UploadedFile{wrongSize, upFile(pi.Path, pom)}, "the upload says 3"},
		{"a repository name too long", []format.UploadedFile{upFile(li.Path, long)}, "OCI repository name"},
	} {
		_, err := Plugin{}.Build(t.Context(), repo, format.Upload{Files: tt.files})
		if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: %v, want ErrInvalid with %q", tt.name, err, tt.msg)
		}
	}
}

// TestValidateRefuses changes one thing of a built manifest, its metadata layer or its POM at a
// time: each disagreement is ErrInvalid, with a part of the message that names it. The layers
// are the sources jar, the jar and the POM, in this order.
func TestValidateRefuses(t *testing.T) {
	jar, ji := testutil.Jar(testutil.JarOptions{})
	src, si := testutil.Jar(testutil.JarOptions{Classifier: "sources"})
	pom, pi := testutil.POM(testutil.POMOptions{Dependencies: []testutil.DependencyOptions{{GroupID: "com.example", ArtifactID: "lib-api", Version: "1.0.0"}}})
	other, _ := testutil.POM(testutil.POMOptions{Version: "1.3.0"})
	revision := project(`<groupId>com.example</groupId><artifactId>lib-core</artifactId><version>${revision}</version>`)
	const sources, main, pomAt = 0, 1, 2
	ann := func(i int, k, v string) func(m *ocispec.Manifest) {
		return func(m *ocispec.Manifest) {
			if v == "" {
				delete(m.Layers[i].Annotations, k)
			} else {
				m.Layers[i].Annotations[k] = v
			}
		}
	}
	file := func(i int, k string, v any) func(md map[string]any) {
		return func(md map[string]any) { md["files"].([]any)[i].(map[string]any)[k] = v }
	}
	for _, tt := range []struct {
		name     string
		manifest func(m *ocispec.Manifest)
		metadata func(md map[string]any)
		pom      []byte
		msg      string
	}{
		{name: "a variant", manifest: func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationVariant] = "jar" }, msg: "no lajstrom.variant"},
		{name: "an unknown maven annotation", manifest: func(m *ocispec.Manifest) { m.Annotations["lajstrom.maven.scope"] = "x" }, msg: "lajstrom.maven.scope is not"},
		{name: "no packaging", manifest: func(m *ocispec.Manifest) { delete(m.Annotations, profile.AnnotationMavenPackaging) }, msg: "has lajstrom.maven.packaging"},
		{name: "a name that is not groupId:artifactId", manifest: func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationName] = "com.example:lib-api" },
			msg: `lajstrom.name is "com.example:lib-api"`},
		{name: "a SNAPSHOT version", manifest: func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationVersion] = "1.2.0-SNAPSHOT" }, msg: "SNAPSHOT"},
		{name: "another groupId", manifest: func(m *ocispec.Manifest) {
			m.Annotations[profile.AnnotationMavenGroupID], m.Annotations[profile.AnnotationName] = "org.example", "org.example:lib-core"
		}, msg: `the metadata groupId is "com.example", the annotations say "org.example"`},
		{name: "a title of another version", manifest: ann(sources, ocispec.AnnotationTitle, "lib-core-1.3.0-sources.jar"), msg: "does not start with"},
		{name: "two layers with one title", manifest: func(m *ocispec.Manifest) { m.Layers[sources] = m.Layers[main] }, msg: "two file layers"},
		{name: "the POM typed artifact", manifest: func(m *ocispec.Manifest) { m.Layers[pomAt].MediaType = profile.MediaTypeMavenArtifact }, msg: "not a application/vnd.lajstrom.maven.pom.v1+xml"},
		{name: "a jar typed pom", manifest: func(m *ocispec.Manifest) { m.Layers[main].MediaType = profile.MediaTypeMavenPom }, msg: "not a application/vnd.lajstrom.maven.artifact.v1"},
		{name: "another extension", manifest: ann(main, profile.AnnotationMavenExtension, "zip"), msg: "its file name gives"},
		{name: "no classifier on the sources", manifest: ann(sources, profile.AnnotationMavenClassifier, ""), msg: "its file name gives"},
		{name: "a classifier on the jar", manifest: ann(main, profile.AnnotationMavenClassifier, "tests"), msg: "its file name gives"},
		{name: "an annotation on the POM", manifest: func(m *ocispec.Manifest) { m.Layers[pomAt].Annotations[profile.AnnotationMavenExtension] = "pom" }, msg: "its file name gives"},
		{name: "no POM", manifest: func(m *ocispec.Manifest) { m.Layers = append(m.Layers[:pomAt], m.Layers[pomAt+1:]...) }, msg: "has the POM lib-core-1.2.0.pom"},
		{name: "a sha512 layer digest", manifest: func(m *ocispec.Manifest) { m.Layers[main].Digest = digest.SHA512.FromBytes(jar) }, msg: "sha512 digest"},
		{name: "metadata of another version", metadata: func(md map[string]any) { md["version"] = "1.3.0" }, msg: "the metadata version"},
		{name: "metadata of another packaging", metadata: func(md map[string]any) { md["packaging"] = "war" }, msg: "the metadata packaging"},
		{name: "a packaging the POM does not say", manifest: func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationMavenPackaging] = "war" },
			metadata: func(md map[string]any) { md["packaging"] = "war" }, msg: `the POM has packaging "jar"`},
		{name: "a file less", metadata: func(md map[string]any) { md["files"] = md["files"].([]any)[1:] }, msg: "lists 2 files"},
		{name: "a file of another name", metadata: file(main, "name", "lib-core-1.2.0.war"), msg: `lists no file "lib-core-1.2.0.jar"`},
		{name: "another classifier", metadata: file(sources, "classifier", "tests"), msg: `classifier "tests"`},
		{name: "another size", metadata: file(main, "size", 1), msg: "1 bytes"},
		{name: "another sha256", metadata: file(main, "sha256", strings.Repeat("0", 64)), msg: "the metadata sha256"},
		{name: "an md5 that is not hex", metadata: file(main, "md5", "not hex"), msg: "the md5 of lib-core-1.2.0.jar"},
		{name: "a sha512 in upper case", metadata: file(sources, "sha512", strings.ToUpper(sumsOf(src)[SHA512])), msg: "lowercase hex"},
		{name: "another sha1 of the POM", metadata: file(pomAt, "sha1", strings.Repeat("0", 40)), msg: "the metadata sha1 of the POM"},
		{name: "other dependencies", metadata: func(md map[string]any) { md["pom"].(map[string]any)["dependencies"] = []any{} }, msg: "dependencies"},
		{name: "a parent", metadata: func(md map[string]any) {
			md["pom"].(map[string]any)["parent"] = map[string]any{"groupId": "com.example", "artifactId": "parent", "version": "1"}
		}, msg: "parent"},
		{name: "a POM of another version", pom: other, msg: "the POM is of com.example:lib-core:1.3.0, the annotations of com.example:lib-core:1.2.0"},
		{name: "a ${revision} POM", pom: revision, msg: "CI-friendly version"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			built, err := Plugin{}.Build(t.Context(), repo, format.Upload{Files: []format.UploadedFile{upFile(ji.Path, jar), upFile(si.Path, src), upFile(pi.Path, pom)}})
			must(t, err)
			registry := map[digest.Digest][]byte{}
			if tt.pom != nil {
				d := digest.FromBytes(tt.pom)
				registry[d] = tt.pom
				built.Manifest.Layers[pomAt].Digest, built.Manifest.Layers[pomAt].Size = d, int64(len(tt.pom))
				tt.metadata = func(md map[string]any) {
					for k, v := range sumsOf(tt.pom) {
						file(pomAt, k, v)(md)
					}
					file(pomAt, "size", len(tt.pom))(md)
				}
			}
			if tt.metadata != nil {
				var md map[string]any
				must(t, json.Unmarshal(built.Entry.Metadata, &md))
				tt.metadata(md)
				b, err := json.Marshal(md)
				must(t, err)
				last := len(built.Manifest.Layers) - 1
				desc := built.Manifest.Layers[last]
				desc.Digest, desc.Size = digest.FromBytes(b), int64(len(b))
				built.Manifest.Layers[last], built.Blobs[len(built.Blobs)-1] = desc, bytesBlob(desc, b)
			}
			if tt.manifest != nil {
				tt.manifest(&built.Manifest)
			}
			_, err = Plugin{}.Validate(t.Context(), repo, built.Manifest, fetcher(t, built, registry, nil))
			if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tt.msg) {
				t.Errorf("Validate: %v, want ErrInvalid with %q", err, tt.msg)
			}
		})
	}
}

// TestIngestSession publishes through the real pipeline a version whose jar and POM a
// publication session has pushed already: Build opens the POM only, ingest finds both files in
// the registry and pushes the config, the metadata layer and the manifest, and the index has
// the version with its files and their annotations.
func TestIngestSession(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:maven-"+t.Name()+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	r := index.Repo{Format: Name, Name: "releases", Registry: "local", Prefix: "maven/releases", Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &r))
	fake := &providertest.Fake{RegistryName: "local"}
	jar, ji := testutil.Jar(testutil.JarOptions{})
	pom, pi := testutil.POM(testutil.POMOptions{})
	rr, err := fake.Repository(ctx, "maven/releases/"+ji.PackagePath)
	must(t, err)
	for _, b := range [][]byte{jar, pom} {
		must(t, rr.Push(ctx, ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromBytes(b), Size: int64(len(b))}, bytes.NewReader(b)))
	}
	pipe := ingest.New(ingest.Options{Providers: provider.Map{"local": fake}, Index: st, Log: slog.New(slog.DiscardHandler),
		Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }})
	up := format.Upload{Files: []format.UploadedFile{session(t, ji.Path, jar, ji.Checksums, false), session(t, pi.Path, pom, pi.Checksums, true)}}
	pub, err := pipe.Upload(ctx, r, up, auth.Principal{Kind: auth.KindUser, Name: "alice"})
	must(t, err)
	if pub.Tag != "1.2.0" || pub.RepoPath != "maven/releases/com.example/lib-core" {
		t.Errorf("published %s:%s", pub.RepoPath, pub.Tag)
	}
	pkg, err := st.GetPackageByNameOCI(ctx, r.ID, ji.PackagePath)
	must(t, err)
	v, err := st.GetVersionByTag(ctx, pkg.ID, "1.2.0")
	must(t, err)
	files, err := st.ListFiles(ctx, v.ID)
	must(t, err)
	var got []string
	for _, f := range files {
		got = append(got, fmt.Sprintf("%s %s %s", f.Name, f.Digest.Encoded(), f.Attrs[profile.AnnotationMavenExtension]))
	}
	if want := []string{ji.Filename + " " + ji.SHA256 + " jar", pi.Filename + " " + pi.SHA256 + " "}; !slices.Equal(got, want) {
		t.Errorf("files %q, want %q", got, want)
	}
	if pkg.Name != "com.example:lib-core" || v.Attrs[profile.AnnotationMavenPackaging] != "jar" {
		t.Errorf("package %q, version attrs %v", pkg.Name, v.Attrs)
	}
}

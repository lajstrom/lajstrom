package maven

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

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

// fetchEnv is a maven proxy repo over SQLite, the fake registry and the real ingest pipeline,
// into which a test streams files as the proxy fetcher does.
type fetchEnv struct {
	st   *sqlite.Store
	fake *providertest.Fake
	pipe *ingest.Pipeline
	repo index.Repo
}

func newFetchEnv(t *testing.T) *fetchEnv {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:maven-"+t.Name()+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	e := &fetchEnv{st: st, fake: &providertest.Fake{RegistryName: "local"},
		repo: index.Repo{Format: Name, Name: "central", Registry: "local", Prefix: "maven/central", Kind: index.KindProxy,
			Upstream: json.RawMessage(`{"name": "central"}`)}}
	must(t, st.CreateRepo(ctx, &e.repo))
	e.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": e.fake}, Index: st, Log: slog.New(slog.DiscardHandler),
		Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }})
	return e
}

// fetch streams the file at path with the bytes b as the proxy fetcher does: named by its file
// name, its path in the parameters, with an origin.
func (e *fetchEnv) fetch(t *testing.T, path string, b []byte) (*format.Published, error) {
	t.Helper()
	sum := sumsOf(b)
	return e.pipe.UploadStream(t.Context(), e.repo, format.StreamUpload{Name: "com.example:lib-core",
		File: format.UploadedFile{Name: path[strings.LastIndex(path, "/")+1:], Size: -1}, Body: bytes.NewReader(b),
		Params: map[string]string{ParamPath: path},
		Origin: &format.Origin{Upstream: "central", URL: "https://repo.example/maven2/" + path, Checksum: "sha1:" + sum[SHA1]}},
		auth.Principal{Kind: auth.KindUser, Name: "alice"})
}

// TestIngestFetched: each file a proxy fetches is a version of that one file, with the variant
// of its extension and classifier and the tag of version and variant; the POM is kept as the
// upstream has it, not read, even one a deploy would refuse; the manifest carries the origin and
// no packaging, and Validate takes it as reindex reads it from the registry
// (docs/spec/manifest-profile.md §4.2).
func TestIngestFetched(t *testing.T) {
	ctx := t.Context()
	e := newFetchEnv(t)
	src, si := testutil.Jar(testutil.JarOptions{Classifier: "sources"})
	pom := project(`<groupId>com.example</groupId><artifactId>lib-core</artifactId><version>${revision}</version>`)
	pomPath := "com/example/lib-core/1.2.0/lib-core-1.2.0.pom"
	for _, tt := range []struct {
		path, variant, tag string
		body               []byte
	}{
		{si.Path, "jar:sources", "1.2.0__jar_3asources", src},
		{pomPath, "pom", "1.2.0__pom", pom},
		{"com/example/lib-core/1.2.0/lib-core-1.2.0.jar.asc", "jar.asc", "1.2.0__jar.asc", []byte("sig")},
	} {
		e.checkFetched(t, tt.path, tt.variant, tt.tag, tt.body)
	}
	pkg, err := e.st.GetPackage(ctx, e.repo.ID, "com.example:lib-core")
	must(t, err)
	vs, err := e.st.ListVersions(ctx, pkg.ID)
	must(t, err)
	if len(vs) != 3 {
		t.Errorf("%d version rows, want one per file", len(vs))
	}
	if _, err := e.fetch(t, si.Path, src); err != nil {
		t.Errorf("the same file again: %v, want the idempotent repeat", err)
	}
}

// checkFetched fetches the file at path with the bytes b and checks the version it makes: its
// variant and tag, a manifest of the one file with the origin and no packaging, the metadata
// layer, and Validate as reindex runs it on the manifest in the registry.
func (e *fetchEnv) checkFetched(t *testing.T, path, variant, tag string, b []byte) {
	t.Helper()
	pub, err := e.fetch(t, path, b)
	if err != nil {
		t.Fatalf("fetch %s: %v", path, err)
	}
	if pub.Tag != tag || pub.Entry.Variant != variant {
		t.Errorf("%s: tag %s, variant %q; want %s, %q", path, pub.Tag, pub.Entry.Variant, tag, variant)
	}
	m := manifestOf(t, e.fake, pub)
	a := m.Annotations
	got := []string{a[profile.AnnotationOriginURL], a[profile.AnnotationMavenPackaging], a[profile.AnnotationUploadedBy],
		m.Layers[0].Annotations[ocispec.AnnotationTitle]}
	if want := []string{"https://repo.example/maven2/" + path, "", "", path[strings.LastIndex(path, "/")+1:]}; !slices.Equal(got, want) ||
		len(m.Layers) != 2 {
		t.Errorf("%s: origin, packaging, uploaded-by and title %q, want %q; %d layers", path, got, want, len(m.Layers))
	}
	var md metadata
	must(t, json.Unmarshal(pub.Entry.Metadata, &md))
	if md.Packaging != "" || len(md.Files) != 1 || md.Files[0].SHA1 != sumsOf(b)[SHA1] || md.POM.Dependencies == nil {
		t.Errorf("%s: metadata %+v", path, md)
	}
	entry, err := Plugin{}.Validate(t.Context(), e.repo, m, registryFetcher(e.fake, pub.RepoPath))
	if err != nil {
		t.Fatalf("Validate %s as reindex reads it: %v", path, err)
	}
	if entry.Variant != variant || entry.Version != pub.Entry.Version {
		t.Errorf("%s: Validate gives %s %q", path, entry.Version, entry.Variant)
	}
}

// TestFetchedRefused: a fetched file whose path parameter is missing or names another file is
// refused before anything is stored.
func TestFetchedRefused(t *testing.T) {
	e := newFetchEnv(t)
	jar, ji := testutil.Jar(testutil.JarOptions{})
	for _, p := range []string{"", "com/example/lib-core/1.2.0/lib-core-1.2.0-sources.jar", "com/example/lib-core/1.0-SNAPSHOT/lib-core-1.0-SNAPSHOT.jar"} {
		_, err := e.pipe.UploadStream(t.Context(), e.repo, format.StreamUpload{Name: "com.example:lib-core",
			File: format.UploadedFile{Name: ji.Filename, Size: -1}, Body: bytes.NewReader(jar), Params: map[string]string{ParamPath: p},
			Origin: &format.Origin{Upstream: "central", URL: "https://repo.example/" + ji.Path, Checksum: "sha1:" + ji.SHA1}},
			auth.Principal{Kind: auth.KindUser, Name: "alice"})
		if !errors.Is(err, format.ErrInvalid) {
			t.Errorf("path %q: %v, want ErrInvalid", p, err)
		}
	}
	if n := len(e.fake.PushReferences()); n != 0 {
		t.Errorf("%d manifests pushed", n)
	}
}

// TestValidateFetchedRefuses: a manifest with a variant is a fetched file's only with the
// origin, one file layer of that variant and no packaging.
func TestValidateFetchedRefuses(t *testing.T) {
	ctx := t.Context()
	e := newFetchEnv(t)
	src, si := testutil.Jar(testutil.JarOptions{Classifier: "sources"})
	pub, err := e.fetch(t, si.Path, src)
	must(t, err)
	base := manifestOf(t, e.fake, pub)
	for _, tt := range []struct {
		name   string
		change func(m *ocispec.Manifest)
		msg    string
	}{
		{"no origin", func(m *ocispec.Manifest) { delete(m.Annotations, profile.AnnotationOriginURL) }, "without lajstrom.origin.url"},
		{"another variant", func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationVariant] = "jar" }, "one file layer, of that extension"},
		{"a packaging", func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationMavenPackaging] = "jar" }, "has no lajstrom.maven.packaging"},
		{"two file layers", func(m *ocispec.Manifest) {
			l := m.Layers[0]
			l.Annotations = maps.Clone(l.Annotations)
			l.Annotations[ocispec.AnnotationTitle] = "lib-core-1.2.0-javadoc.jar"
			l.Annotations[profile.AnnotationMavenClassifier] = "javadoc"
			m.Layers = append([]ocispec.Descriptor{l}, m.Layers...)
		}, "one file layer"},
	} {
		m := base
		m.Annotations = maps.Clone(base.Annotations)
		m.Layers = append([]ocispec.Descriptor(nil), base.Layers...)
		tt.change(&m)
		_, err := Plugin{}.Validate(ctx, e.repo, m, registryFetcher(e.fake, pub.RepoPath))
		if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: %v, want ErrInvalid with %q", tt.name, err, tt.msg)
		}
	}
}

// manifestOf reads the manifest pub names from the fake registry.
func manifestOf(t *testing.T, fake *providertest.Fake, pub *format.Published) ocispec.Manifest {
	t.Helper()
	rc, err := registryFetcher(fake, pub.RepoPath)(t.Context(), pub.Digest)
	must(t, err)
	defer func() { _ = rc.Close() }()
	var m ocispec.Manifest
	must(t, json.NewDecoder(rc).Decode(&m))
	return m
}

// registryFetcher reads blobs and manifests of the OCI repository repoPath of the fake registry.
func registryFetcher(fake *providertest.Fake, repoPath string) format.BlobFetcher {
	return func(ctx context.Context, d digest.Digest) (io.ReadCloser, error) {
		rr, err := fake.Repository(ctx, repoPath)
		if err != nil {
			return nil, err
		}
		return rr.Fetch(ctx, ocispec.Descriptor{Digest: d})
	}
}

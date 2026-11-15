package terraform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

var infra = index.Repo{Format: Name, Name: "infra"}

// alice is the principal of the publishes.
var alice = auth.Principal{Kind: auth.KindUser, Name: "alice"}

// archiveFile is an UploadedFile of b that Build reads with Open.
func archiveFile(b []byte) format.UploadedFile {
	return format.UploadedFile{Name: "module.tar.gz", Size: int64(len(b)), Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}
}

// params are the parameters of a publish of the package name and version.
func params(name, version string) map[string]string {
	return map[string]string{ParamName: name, ParamVersion: version, ParamUploadedBy: "user:alice"}
}

// fetcherOf fetches the blobs of built, the archive b among them, as the registry would.
func fetcherOf(built *format.Built, b []byte) format.BlobFetcher {
	return func(_ context.Context, d digest.Digest) (io.ReadCloser, error) {
		if d == built.Manifest.Layers[0].Digest {
			return io.NopCloser(bytes.NewReader(b)), nil
		}
		return nil, fmt.Errorf("no blob %s", d)
	}
}

// builtWant is what TestBuild expects of a build.
type builtWant struct{ name, version, tag, repoPath, title string }

// TestBuild builds the manifest of a module version: the annotations, the tag, the repository
// path and the one archive layer titled with the file name, and no metadata layer; Validate
// takes what Build made.
func TestBuild(t *testing.T) {
	b, sums := testutil.TerraformModule(testutil.TerraformOptions{})
	for _, c := range []builtWant{
		{"vpc/aws", "1.2.0", "1.2.0", "terraform/infra/vpc/aws", "vpc-aws-1.2.0.tar.gz"},
		{"My_Network/azurerm", "2.0.0-rc.1", "2.0.0-rc.1", "terraform/infra/my_5fnetwork/azurerm", "My_Network-azurerm-2.0.0-rc.1.tar.gz"},
	} {
		built, err := Plugin{}.Build(t.Context(), infra, format.Upload{Files: []format.UploadedFile{archiveFile(b)}, Params: params(c.name, c.version)})
		if err != nil {
			t.Fatal(err)
		}
		checkBuilt(t, built, c, sums.SHA256, int64(len(b)))
		if _, err := (Plugin{}).Validate(t.Context(), infra, built.Manifest, fetcherOf(built, b)); err != nil {
			t.Errorf("%s: Validate of what Build made: %v", c.name, err)
		}
	}
}

// checkBuilt checks built against c, the archive's sha256 and size.
func checkBuilt(t *testing.T, built *format.Built, c builtWant, sha256 string, size int64) {
	t.Helper()
	m, e := built.Manifest, built.Entry
	if len(m.Layers) != 1 || len(e.Files) != 1 {
		t.Fatalf("%s: layers %+v, files %+v", c.name, m.Layers, e.Files)
	}
	a, l := m.Annotations, m.Layers[0]
	for _, check := range []struct {
		what      string
		got, want any
	}{
		{"artifactType", m.ArtifactType, "application/vnd.lajstrom.terraform.v1"},
		{"format", a[profile.AnnotationFormat], "terraform"},
		{"repo", a[profile.AnnotationRepo], "infra"},
		{"name", a[profile.AnnotationName], c.name},
		{"version", a[profile.AnnotationVersion], c.version},
		{"variant", a[profile.AnnotationVariant], ""},
		{"uploaded-by", a[profile.AnnotationUploadedBy], "user:alice"},
		{"tag", built.Tag, c.tag},
		{"repo path", built.RepoPath, c.repoPath},
		{"layer media type", l.MediaType, profile.MediaTypeTerraformModule},
		{"layer digest", l.Digest.Encoded(), sha256},
		{"layer size", l.Size, size},
		{"layer title", l.Annotations[ocispec.AnnotationTitle], c.title},
		{"entry", fmt.Sprint(e.Name, " ", e.Version, " ", e.Variant, " ", e.Metadata == nil), fmt.Sprint(c.name, " ", c.version, "  true")},
		{"file", e.Files[0].Name, c.title},
		{"blobs", len(built.Blobs), 2}, // the config and the archive
	} {
		if check.got != check.want {
			t.Errorf("%s: %s %v, want %v", c.name, check.what, check.got, check.want)
		}
	}
}

// TestBuildRefuses: a name, a system or a version out of their grammars, two files, and a body
// that is no module archive or not as long as it says, each format.ErrInvalid.
func TestBuildRefuses(t *testing.T) {
	b, _ := testutil.TerraformModule(testutil.TerraformOptions{})
	short := archiveFile(b)
	short.Size = int64(len(b)) + 1
	for name, c := range map[string]struct {
		files        []format.UploadedFile
		pkg, version string
	}{
		"no file":      {nil, "vpc/aws", "1.2.0"},
		"two files":    {[]format.UploadedFile{archiveFile(b), archiveFile(b)}, "vpc/aws", "1.2.0"},
		"name":         {[]format.UploadedFile{archiveFile(b)}, "v.pc/aws", "1.2.0"},
		"system":       {[]format.UploadedFile{archiveFile(b)}, "vpc/AWS", "1.2.0"},
		"no system":    {[]format.UploadedFile{archiveFile(b)}, "vpc", "1.2.0"},
		"v":            {[]format.UploadedFile{archiveFile(b)}, "vpc/aws", "v1.2.0"},
		"build":        {[]format.UploadedFile{archiveFile(b)}, "vpc/aws", "1.2.0+7"},
		"not a module": {[]format.UploadedFile{archiveFile([]byte("not gzip"))}, "vpc/aws", "1.2.0"},
		"size":         {[]format.UploadedFile{short}, "vpc/aws", "1.2.0"},
		"not streamed": {[]format.UploadedFile{{Name: "x", Size: 1, Checksums: map[string]string{"sha256": strings.Repeat("0", 64)}}}, "vpc/aws", "1.2.0"},
	} {
		_, err := Plugin{}.Build(t.Context(), infra, format.Upload{Files: c.files, Params: params(c.pkg, c.version)})
		if !errors.Is(err, format.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

// TestValidate refuses a manifest that disagrees with the profile of terraform, and keeps the
// error of a fetch that fails.
func TestValidate(t *testing.T) {
	b, _ := testutil.TerraformModule(testutil.TerraformOptions{})
	built, err := Plugin{}.Build(t.Context(), infra, format.Upload{Files: []format.UploadedFile{archiveFile(b)}, Params: params("vpc/aws", "1.2.0")})
	must(t, err)
	edit := func(f func(m *ocispec.Manifest)) ocispec.Manifest {
		m := built.Manifest
		m.Annotations = clone(m.Annotations)
		m.Layers = append([]ocispec.Descriptor(nil), m.Layers...)
		m.Layers[0].Annotations = clone(m.Layers[0].Annotations)
		f(&m)
		return m
	}
	md := ocispec.Descriptor{MediaType: profile.MetadataMediaType(Type), Digest: digest.FromString("{}"), Size: 2,
		Annotations: map[string]string{ocispec.AnnotationTitle: profile.MetadataFileName}}
	for name, m := range map[string]ocispec.Manifest{
		"name":         edit(func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationName] = "vpc" }),
		"version":      edit(func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationVersion] = "1.2" }),
		"variant":      edit(func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationVariant] = "linux" }),
		"attr":         edit(func(m *ocispec.Manifest) { m.Annotations["lajstrom.terraform.namespace"] = "infra" }),
		"title":        edit(func(m *ocispec.Manifest) { m.Layers[0].Annotations[ocispec.AnnotationTitle] = "vpc-aws-1.2.1.tar.gz" }),
		"metadata":     edit(func(m *ocispec.Manifest) { m.Layers = append(m.Layers, md) }),
		"two archives": edit(func(m *ocispec.Manifest) { m.Layers = append(m.Layers, m.Layers[0]) }),
		"sha512": edit(func(m *ocispec.Manifest) {
			m.Layers[0].Digest = digest.SHA512.FromBytes(b)
		}),
	} {
		if _, err := (Plugin{}).Validate(t.Context(), infra, m, fetcherOf(built, b)); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	notModule := func(context.Context, digest.Digest) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("no")), nil
	}
	if _, err := (Plugin{}).Validate(t.Context(), infra, built.Manifest, notModule); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("an archive that is no module: %v, want ErrInvalid", err)
	}
	boom := errors.New("registry down")
	failing := func(context.Context, digest.Digest) (io.ReadCloser, error) { return nil, boom }
	if _, err := (Plugin{}).Validate(t.Context(), infra, built.Manifest, failing); !errors.Is(err, boom) || errors.Is(err, format.ErrInvalid) {
		t.Errorf("a fetch that fails: %v", err)
	}
	streamed := func(context.Context, digest.Digest) (io.ReadCloser, error) { return nil, format.ErrStreamed }
	if _, err := (Plugin{}).Validate(t.Context(), infra, built.Manifest, streamed); err != nil {
		t.Errorf("the archive of a publish that streams: %v", err)
	}
}

func clone(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ingestEnv is the real ingest pipeline over the real SQLite store in memory and the fake
// registry "local", with the terraform repo infra.
type ingestEnv struct {
	st   *sqlite.Store
	prov *providertest.Fake
	pipe *ingest.Pipeline
	repo index.Repo
}

func newIngestEnv(t *testing.T) *ingestEnv {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:terraform-"+t.Name()+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	e := &ingestEnv{st: st, prov: &providertest.Fake{RegistryName: "local"},
		repo: index.Repo{Format: Name, Name: "infra", Registry: "local", Prefix: "terraform/infra", Kind: "hosted"}}
	must(t, st.CreateRepo(ctx, &e.repo))
	e.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": e.prov}, Index: st,
		Log: slog.New(slog.DiscardHandler), Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }})
	return e
}

// stream publishes b as version of vpc/aws through UploadStream, as the publish route does.
func (e *ingestEnv) stream(ctx context.Context, b []byte, version string) (*format.Published, error) {
	return e.pipe.UploadStream(ctx, e.repo, format.StreamUpload{Name: "vpc/aws",
		File: format.UploadedFile{Name: "module.tar.gz", Size: int64(len(b))}, Body: bytes.NewReader(b),
		Params: params("vpc/aws", version)}, alice)
}

// TestIngest publishes through the real pipeline, by Upload and by UploadStream, Build then
// Validate, and checks the rows: the package by its name at its package path, the versions, the
// tag, the file by its title.
func TestIngest(t *testing.T) {
	ctx := t.Context()
	e := newIngestEnv(t)
	one, _ := testutil.TerraformModule(testutil.TerraformOptions{})
	pub, err := e.pipe.Upload(ctx, e.repo, format.Upload{Files: []format.UploadedFile{archiveFile(one)}, Params: params("vpc/aws", "1.0.0")}, alice)
	must(t, err)
	if pub.Tag != "1.0.0" || pub.RepoPath != "terraform/infra/vpc/aws" || pub.Existed {
		t.Errorf("published %+v", pub)
	}
	two, _ := testutil.TerraformModule(testutil.TerraformOptions{Files: map[string]string{"main.tf": "# two\n"}})
	pub2, err := e.stream(ctx, two, "1.1.0")
	must(t, err)
	if pub2.Tag != "1.1.0" || pub2.Existed {
		t.Errorf("streamed %+v", pub2)
	}
	pkg, err := e.st.GetPackage(ctx, e.repo.ID, "vpc/aws")
	must(t, err)
	versions, err := e.st.ListVersions(ctx, pkg.ID)
	must(t, err)
	if pkg.NameOCI != "vpc/aws" || len(versions) != 2 || versions[0].Version != "1.0.0" || versions[1].Version != "1.1.0" || versions[1].Digest != pub2.Digest {
		t.Fatalf("package %+v, versions %+v", pkg, versions)
	}
	if files, err := e.st.FindFiles(ctx, e.repo.ID, "vpc-aws-1.1.0.tar.gz"); err != nil || len(files) != 1 || files[0].File.Size != int64(len(two)) {
		t.Errorf("files of 1.1.0: %+v, %v", files, err)
	}
}

// TestIngestRepeat: the same bytes again are the idempotent repeat, other bytes under the
// version a conflict; a body that is no module is refused and stores nothing.
func TestIngestRepeat(t *testing.T) {
	ctx := t.Context()
	e := newIngestEnv(t)
	one, _ := testutil.TerraformModule(testutil.TerraformOptions{})
	two, _ := testutil.TerraformModule(testutil.TerraformOptions{Files: map[string]string{"main.tf": "# two\n"}})
	pub, err := e.stream(ctx, two, "1.1.0")
	must(t, err)
	if again, err := e.stream(ctx, two, "1.1.0"); err != nil || !again.Existed || again.Digest != pub.Digest {
		t.Errorf("the repeat = %+v, %v", again, err)
	}
	if _, err := e.stream(ctx, one, "1.1.0"); !errors.Is(err, index.ErrConflict) {
		t.Errorf("other bytes under 1.1.0: %v, want ErrConflict", err)
	}
	if _, err := e.stream(ctx, []byte("not a module"), "1.2.0"); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("a body that is no module: %v, want ErrInvalid", err)
	}
	pkg, err := e.st.GetPackage(ctx, e.repo.ID, "vpc/aws")
	must(t, err)
	if _, err := e.st.GetVersion(ctx, pkg.ID, "1.2.0", ""); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the refused 1.2.0: %v, want ErrNotFound", err)
	}
}

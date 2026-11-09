package pypi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

var repo = index.Repo{ID: 1, Format: Name, Name: "internal", Registry: "local", Prefix: "pypi/internal", Kind: "hosted"}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// upload is the Upload of one file.
func upload(name string, b []byte) format.Upload {
	return format.Upload{Params: map[string]string{ParamUploadedBy: "user:alice"}, Files: []format.UploadedFile{{
		Name: name, Size: int64(len(b)), Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil },
	}}}
}

// fetcher serves the blobs of built.
func fetcher(t *testing.T, built *format.Built) format.BlobFetcher {
	t.Helper()
	blobs := map[digest.Digest][]byte{}
	for _, b := range built.Blobs {
		rc, err := b.Open(t.Context())
		must(t, err)
		data, err := io.ReadAll(rc)
		must(t, err)
		_ = rc.Close()
		blobs[b.Descriptor.Digest] = data
	}
	return func(_ context.Context, d digest.Digest) (io.ReadCloser, error) {
		b, ok := blobs[d]
		if !ok {
			return nil, fmt.Errorf("no blob %s", d)
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}

func TestEscapeName(t *testing.T) {
	for in, want := range map[string]string{"Requests": "requests", "My_Pkg": "my-pkg", "zope.interface": "zope-interface"} {
		got, err := Plugin{}.EscapeName(in)
		if err != nil || got != want {
			t.Errorf("EscapeName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := (Plugin{}).EscapeName("../x"); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("EscapeName of a path: %v", err)
	}
}

// TestBuildValidate builds a wheel and an sdist and checks the manifest: the annotations (the
// normalized name, the version as written, the tag set or sdist), the tag, the path, the
// metadata layer; then Validate of the same manifest returns the same entry.
func TestBuildValidate(t *testing.T) {
	ctx := t.Context()
	whl, wi := testutil.Wheel(testutil.WheelOptions{Name: "My_Pkg", Version: "1.2.0", PythonTag: "cp311", AbiTag: "cp311",
		PlatformTag: "manylinux_2_17_x86_64.manylinux2014_x86_64", RequiresDist: []string{"requests>=2"}})
	tgz, si := testutil.Sdist(testutil.SdistOptions{Name: "My_Pkg", Version: "1.2.0"})
	for _, tt := range []struct {
		name, filename string
		data           []byte
		variant        string
		mediaType      string
		packagetype    string
		core           []byte
	}{
		{"wheel", wi.Filename, whl, "cp311-cp311-manylinux_2_17_x86_64.manylinux2014_x86_64", profile.MediaTypePypiWheel, PackagetypeWheel, wi.Metadata},
		{"sdist", si.Filename, tgz, VariantSdist, profile.MediaTypePypiSdistTarGz, PackagetypeSdist, si.PKGInfo},
	} {
		t.Run(tt.name, func(t *testing.T) {
			built, err := Plugin{}.Build(ctx, repo, upload(tt.filename, tt.data))
			must(t, err)
			a := built.Manifest.Annotations
			for k, want := range map[string]string{profile.AnnotationName: "my-pkg", profile.AnnotationVersion: "1.2.0",
				profile.AnnotationVariant: tt.variant, profile.AnnotationFormat: Name, profile.AnnotationRepo: "internal"} {
				if a[k] != want {
					t.Errorf("%s = %q, want %q", k, a[k], want)
				}
			}
			if _, ok := a[profile.AnnotationPypiNonstandard]; ok {
				t.Error("a PEP 440 version is flagged nonstandard")
			}
			if built.Tag != profile.Tag("1.2.0", tt.variant) || built.RepoPath != "pypi/internal/my-pkg" {
				t.Errorf("tag %q, path %q", built.Tag, built.RepoPath)
			}
			file := built.Manifest.Layers[0]
			if file.MediaType != tt.mediaType || file.Digest != digest.FromBytes(tt.data) || file.Annotations[ocispec.AnnotationTitle] != tt.filename {
				t.Errorf("file layer %+v", file)
			}
			var md map[string]any
			must(t, json.Unmarshal(built.Entry.Metadata, &md))
			for k, want := range map[string]any{"name": "My_Pkg", "version": "1.2.0", FieldFilename: tt.filename,
				FieldSHA256: digest.FromBytes(tt.data).Encoded(), FieldPackagetype: tt.packagetype, FieldCoreMetadata: string(tt.core)} {
				if md[k] != want {
					t.Errorf("metadata %s = %v, want %v", k, md[k], want)
				}
			}
			entry, err := Plugin{}.Validate(ctx, repo, built.Manifest, fetcher(t, built))
			must(t, err)
			entry.Files[0].Attrs = nil
			if d := cmp.Diff(&built.Entry, entry); d != "" {
				t.Errorf("Validate: (-Build +Validate)\n%s", d)
			}
		})
	}
}

// TestBuildRefuses is the list of validation errors of #31 on the upload path: a name and a
// version that the core metadata does not say, a wheel without METADATA, an sdist without
// PKG-INFO, a file that is no distribution, two files; and a version that is not PEP 440 is
// accepted, flagged.
func TestBuildRefuses(t *testing.T) {
	ctx := t.Context()
	whl, wi := testutil.Wheel(testutil.WheelOptions{Name: "tool", Version: "1.0"})
	tgz, si := testutil.Sdist(testutil.SdistOptions{Name: "tool", Version: "1.0"})
	for _, tt := range []struct {
		name string
		up   format.Upload
	}{
		{"another name in the file name", upload("other-1.0-py3-none-any.whl", whl)},
		{"another version in the file name", upload("tool-2.0-py3-none-any.whl", whl)},
		{"a wheel without METADATA", upload(wi.Filename, zipOf(t, "tool/__init__.py", "tool-1.0.dist-info/WHEEL"))},
		{"an sdist without PKG-INFO", upload("tool-2.0.tar.gz", tgz)},
		{"an sdist of another kind", upload("tool-1.0.tar.bz2", tgz)},
		{"a wheel that is no zip", upload(wi.Filename, []byte("not a zip"))},
		{"a size that is not the declared one", func() format.Upload { u := upload(si.Filename, tgz); u.Files[0].Size = 3; return u }()},
		{"two files", func() format.Upload {
			u := upload(wi.Filename, whl)
			u.Files = append(u.Files, upload(si.Filename, tgz).Files...)
			return u
		}()},
	} {
		if _, err := (Plugin{}).Build(ctx, repo, tt.up); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", tt.name, err)
		}
	}

	odd, oi := testutil.Wheel(testutil.WheelOptions{Name: "tool", Version: "latest"})
	built, err := Plugin{}.Build(ctx, repo, upload(oi.Filename, odd))
	must(t, err)
	if built.Manifest.Annotations[profile.AnnotationPypiNonstandard] != "true" {
		t.Errorf("version latest: annotations %v", built.Manifest.Annotations)
	}
	if _, err := (Plugin{}).Validate(ctx, repo, built.Manifest, fetcher(t, built)); err != nil {
		t.Errorf("Validate of a nonstandard version: %v", err)
	}
}

// TestValidateRefuses changes one thing of a built wheel's manifest or metadata layer at a time:
// each disagreement is ErrInvalid.
func TestValidateRefuses(t *testing.T) {
	ctx := t.Context()
	whl, wi := testutil.Wheel(testutil.WheelOptions{Name: "Tool", Version: "1.0"})
	for _, tt := range []struct {
		name     string
		manifest func(m *ocispec.Manifest)
		metadata func(md map[string]any)
	}{
		{name: "lajstrom.name not normalized", manifest: func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationName] = "Tool" }},
		{name: "another variant", manifest: func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationVariant] = "py2-none-any" }},
		{name: "flagged nonstandard", manifest: func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationPypiNonstandard] = "true" }},
		{name: "an unknown pypi annotation", manifest: func(m *ocispec.Manifest) { m.Annotations["lajstrom.pypi.x"] = "y" }},
		{name: "a file name of another project", manifest: func(m *ocispec.Manifest) {
			m.Layers[0].Annotations[ocispec.AnnotationTitle] = "other-1.0-py3-none-any.whl"
		}},
		{name: "a sdist media type on a wheel", manifest: func(m *ocispec.Manifest) { m.Layers[0].MediaType = profile.MediaTypePypiSdistTarGz }},
		{name: "two file layers", manifest: func(m *ocispec.Manifest) {
			m.Layers = append([]ocispec.Descriptor{m.Layers[0]}, m.Layers...)
		}},
		{name: "metadata of another name", metadata: func(md map[string]any) { md["name"] = "other" }},
		{name: "metadata of another version", metadata: func(md map[string]any) { md["version"] = "2.0" }},
		{name: "metadata of another sha256", metadata: func(md map[string]any) { md[FieldSHA256] = strings.Repeat("0", 64) }},
		{name: "metadata of another filename", metadata: func(md map[string]any) { md[FieldFilename] = "tool-1.0.tar.gz" }},
		{name: "metadata of another packagetype", metadata: func(md map[string]any) { md[FieldPackagetype] = PackagetypeSdist }},
		{name: "core_metadata of another version", metadata: func(md map[string]any) {
			md[FieldCoreMetadata] = strings.Replace(md[FieldCoreMetadata].(string), "Version: 1.0", "Version: 9.9", 1)
		}},
		{name: "no core_metadata", metadata: func(md map[string]any) { delete(md, FieldCoreMetadata) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			built, err := Plugin{}.Build(ctx, repo, upload(wi.Filename, whl))
			must(t, err)
			if tt.metadata != nil {
				var md map[string]any
				must(t, json.Unmarshal(built.Entry.Metadata, &md))
				tt.metadata(md)
				b, err := json.Marshal(md)
				must(t, err)
				desc := ocispec.Descriptor{MediaType: built.Manifest.Layers[1].MediaType, Digest: digest.FromBytes(b), Size: int64(len(b)),
					Annotations: built.Manifest.Layers[1].Annotations}
				built.Manifest.Layers[1] = desc
				built.Blobs[2] = bytesBlob(desc, b)
			}
			if tt.manifest != nil {
				tt.manifest(&built.Manifest)
			}
			if _, err := (Plugin{}).Validate(ctx, repo, built.Manifest, fetcher(t, built)); !errors.Is(err, format.ErrInvalid) {
				t.Errorf("Validate: %v, want ErrInvalid", err)
			}
		})
	}
}

// TestIngestSpellings uploads two versions through the real pipeline, the first as Requests
// and the second as requests: one package, as decided on #31, where the original spelling
// would have been a 409 on the second; the package row has the normalized name.
func TestIngestSpellings(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:pypi-"+t.Name()+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	r := index.Repo{Format: Name, Name: "internal", Registry: "local", Prefix: "pypi/internal", Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &r))
	pipe := ingest.New(ingest.Options{Providers: provider.Map{"local": &providertest.Fake{RegistryName: "local"}}, Index: st,
		Log: slog.New(slog.DiscardHandler), Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }})
	alice := auth.Principal{Kind: auth.KindUser, Name: "alice"}
	for _, o := range []testutil.WheelOptions{{Name: "Requests", Version: "2.31.0"}, {Name: "requests", Version: "2.32.3"}} {
		whl, wi := testutil.Wheel(o)
		if _, err := pipe.Upload(ctx, r, upload(wi.Filename, whl), alice); err != nil {
			t.Fatalf("upload %s: %v", wi.Filename, err)
		}
	}
	pkgs, err := st.ListPackages(ctx, r.ID, "")
	must(t, err)
	if len(pkgs) != 1 || pkgs[0].Name != "requests" || pkgs[0].NameOCI != "requests" {
		t.Fatalf("packages: %+v", pkgs)
	}
	versions, err := st.ListVersions(ctx, pkgs[0].ID)
	must(t, err)
	if len(versions) != 2 {
		t.Errorf("%d versions, want 2", len(versions))
	}
}

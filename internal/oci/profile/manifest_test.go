package profile

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The canonical encoding of debManifest. Changing the encoding, the field order or the fixture
// changes these; a change of the encoding changes every manifest digest and needs an ADR.
const (
	debManifestJSON = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.lajstrom.deb.v1",` +
		`"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},` +
		`"layers":[{"mediaType":"application/vnd.lajstrom.deb.package.v1","digest":"sha256:770531e3e6e6a2fb35ea7c4f53a204820fa14ceeb0f86dc899ffb86dda090e38","size":1258496,"annotations":{"org.opencontainers.image.title":"nginx_1.18.0-6ubuntu14.4_amd64.deb"}}` +
		`,{"mediaType":"application/vnd.lajstrom.deb.metadata.v1+json","digest":"sha256:2410a10264cdb9cd9fe8d44770c9493a0bea9147a728b7ff5c569d01d3588ef1","size":1830,"annotations":{"org.opencontainers.image.title":"metadata.json"}}],` +
		`"annotations":{"lajstrom.deb.arch":"amd64","lajstrom.deb.component":"main","lajstrom.deb.suite":"jammy","lajstrom.format":"apt","lajstrom.name":"nginx","lajstrom.repo":"ubuntu-internal",` +
		`"lajstrom.schema":"1","lajstrom.uploaded-by":"user:ci","lajstrom.variant":"jammy/main/amd64","lajstrom.version":"1:1.18.0-6ubuntu14.4","org.opencontainers.image.created":"2026-11-06T10:00:00Z","org.opencontainers.image.source":"https://example.com/nginx.git"}}`
	debManifestDigest = digest.Digest("sha256:348b5a95297f4a1f4a49fa7c911bf1586f222b56a16de1c87b3ba75bf18192ae")
)

// The layers of the §1 example; the digests stand in for the real blobs.
func debFile() Layer {
	return Layer{
		MediaType: MediaTypeDebPackage,
		Digest:    digest.FromString("nginx_1.18.0-6ubuntu14.4_amd64.deb"),
		Size:      1258496,
		Title:     "nginx_1.18.0-6ubuntu14.4_amd64.deb",
	}
}

func debMetadata() *Layer {
	return &Layer{
		MediaType: MetadataMediaType("deb"),
		Digest:    digest.FromString(`{"control":{}}`),
		Size:      1830,
		Title:     MetadataFileName,
	}
}

// debManifest is the §1 example built with Build.
func debManifest(t testing.TB) ocispec.Manifest {
	t.Helper()
	m, err := Build(debAnnotations(), []Layer{debFile()}, debMetadata())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// rawManifest is a raw manifest without a metadata layer and with a directory in the title.
func rawManifest(t testing.TB) ocispec.Manifest {
	t.Helper()
	a := Annotations{
		Format: "raw", Repo: "assets", Name: "Docs/Readme.MD", Version: "1",
		Created: time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC),
		Attrs:   map[string]string{AnnotationRawPath: "Docs/Readme.MD"},
	}
	f := Layer{MediaType: MediaTypeRawFile, Digest: digest.FromString("# readme"), Size: 8, Title: "Docs/Readme.MD"}
	m, err := Build(a, []Layer{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestEncodePinned(t *testing.T) {
	m := debManifest(t)
	b1, d1, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	b2, d2, err := Encode(debManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) || d1 != d2 {
		t.Fatalf("Encode is not stable: %s %s", d1, d2)
	}
	if string(b1) != debManifestJSON {
		t.Errorf("Encode =\n%s\nwant\n%s", b1, debManifestJSON)
	}
	if d1 != debManifestDigest || d1 != digest.FromBytes(b1) {
		t.Errorf("digest = %s, want %s", d1, debManifestDigest)
	}
}

func TestBuildValidateRoundTrip(t *testing.T) {
	b, _, err := Encode(debManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	p, err := Validate(m)
	if err != nil {
		t.Fatal(err)
	}
	want := &Parsed{
		Type: Type{
			Name: "deb", Format: "apt", FileMediaTypes: []string{MediaTypeDebPackage, MediaTypeDebDsc, MediaTypeDebSource}, MetadataRequired: true,
		},
		Annotations: debAnnotations(),
		Files:       []Layer{debFile()},
		Metadata:    debMetadata(),
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("Validate =\n%+v\nwant\n%+v", p, want)
	}
}

func TestBuild(t *testing.T) {
	// Layer annotations are kept, Title wins over a title in them, and the inputs are not modified.
	f := debFile()
	f.Annotations = map[string]string{ocispec.AnnotationTitle: "old.deb", "x": "y"}
	md := &Layer{Digest: digest.FromString("{}"), Size: 2} // MediaType and Title default
	m, err := Build(debAnnotations(), []Layer{f}, md)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Layers[0].Annotations; !maps.Equal(got, map[string]string{ocispec.AnnotationTitle: f.Title, "x": "y"}) {
		t.Errorf("file layer annotations = %v", got)
	}
	if got := m.Layers[1]; got.MediaType != MetadataMediaType("deb") || got.Annotations[ocispec.AnnotationTitle] != MetadataFileName {
		t.Errorf("metadata layer = %+v, want the defaults", got)
	}
	if f.Annotations[ocispec.AnnotationTitle] != "old.deb" || md.Title != "" || md.MediaType != "" {
		t.Error("Build modified its arguments")
	}
	if m.Config.Data != nil || m.Subject != nil || m.SchemaVersion != 2 {
		t.Errorf("manifest = %+v", m)
	}

	a := debAnnotations()
	a.Format = "cargo"
	if _, err := Build(a, []Layer{f}, md); !errors.Is(err, ErrArtifactType) {
		t.Errorf("Build with format cargo: %v, want ErrArtifactType", err)
	}
	if _, err := Build(debAnnotations(), nil, md); !errors.Is(err, ErrNoLayers) {
		t.Errorf("Build without files: %v, want ErrNoLayers", err)
	}
	if _, err := Build(debAnnotations(), []Layer{f}, nil); !errors.Is(err, ErrMetadata) {
		t.Errorf("Build of deb without metadata: %v, want ErrMetadata", err)
	}
}

// sentinels are the Validate errors; each one must be distinct from the others.
var sentinels = []error{
	ErrMediaType, ErrArtifactType, ErrConfig, ErrAnnotation, ErrFormatMismatch,
	ErrNoLayers, ErrLayerMediaType, ErrLayerDescriptor, ErrLayerTitle, ErrMetadata,
}

func TestValidate(t *testing.T) {
	pypiMeta := ocispec.Descriptor{
		MediaType: MetadataMediaType("pypi"), Digest: digest.FromString("{}"), Size: 2,
		Annotations: map[string]string{ocispec.AnnotationTitle: MetadataFileName},
	}
	for _, tc := range []struct {
		name   string
		raw    bool // start from rawManifest instead of debManifest
		mutate func(m *ocispec.Manifest)
		want   error // nil: valid
	}{
		{"valid deb", false, func(*ocispec.Manifest) {}, nil},
		{"valid raw without metadata, directory in title", true, func(*ocispec.Manifest) {}, nil},
		{"config with embedded data", false, func(m *ocispec.Manifest) { m.Config = ocispec.DescriptorEmptyJSON }, nil},
		{"subject set", false, func(m *ocispec.Manifest) { s := m.Layers[0]; m.Subject = &s }, nil},

		{"media type index", false, func(m *ocispec.Manifest) { m.MediaType = ocispec.MediaTypeImageIndex }, ErrMediaType},
		{"media type empty", false, func(m *ocispec.Manifest) { m.MediaType = "" }, ErrMediaType},
		{"schemaVersion 1", false, func(m *ocispec.Manifest) { m.SchemaVersion = 1 }, ErrMediaType},
		{"artifactType unknown", false, func(m *ocispec.Manifest) { m.ArtifactType = ArtifactType("cargo") }, ErrArtifactType},
		{"artifactType foreign", false, func(m *ocispec.Manifest) { m.ArtifactType = "application/vnd.cncf.helm.config.v1+json" }, ErrArtifactType},
		{"artifactType empty", false, func(m *ocispec.Manifest) { m.ArtifactType = "" }, ErrArtifactType},
		{"config image config", false, func(m *ocispec.Manifest) { m.Config.MediaType = ocispec.MediaTypeImageConfig }, ErrConfig},
		{"config digest", false, func(m *ocispec.Manifest) { m.Config.Digest = digest.FromString("{ }") }, ErrConfig},
		{"config size", false, func(m *ocispec.Manifest) { m.Config.Size = 3 }, ErrConfig},
		{"annotation missing", false, func(m *ocispec.Manifest) { delete(m.Annotations, AnnotationRepo) }, ErrAnnotation},
		{"annotation empty", false, func(m *ocispec.Manifest) { m.Annotations[AnnotationVersion] = "" }, ErrAnnotation},
		{"annotation schema", false, func(m *ocispec.Manifest) { m.Annotations[AnnotationSchema] = "2" }, ErrAnnotation},
		{"annotation created", false, func(m *ocispec.Manifest) { m.Annotations[ocispec.AnnotationCreated] = "now" }, ErrAnnotation},
		{"no annotations", false, func(m *ocispec.Manifest) { m.Annotations = nil }, ErrAnnotation},
		{"format of another type", false, func(m *ocispec.Manifest) { m.Annotations[AnnotationFormat] = "pypi" }, ErrFormatMismatch},
		{"format is the type name", false, func(m *ocispec.Manifest) { m.Annotations[AnnotationFormat] = "deb" }, ErrFormatMismatch},
		{"no layers", false, func(m *ocispec.Manifest) { m.Layers = nil }, ErrNoLayers},
		{"only the metadata layer", false, func(m *ocispec.Manifest) { m.Layers = m.Layers[1:] }, ErrNoLayers},
		{"bad layer digest", false, func(m *ocispec.Manifest) { m.Layers[0].Digest = "sha256:zz" }, ErrLayerDescriptor},
		{"unknown digest algorithm", false, func(m *ocispec.Manifest) { m.Layers[0].Digest = "md5:00000000000000000000000000000000" }, ErrLayerDescriptor},
		{"negative layer size", false, func(m *ocispec.Manifest) { m.Layers[1].Size = -1 }, ErrLayerDescriptor},
		{"file role of another type", false, func(m *ocispec.Manifest) { m.Layers[0].MediaType = MediaTypePypiWheel }, ErrLayerMediaType},
		{"metadata of another type", false, func(m *ocispec.Manifest) {
			m.Layers = append([]ocispec.Descriptor{m.Layers[0], pypiMeta}, m.Layers[1:]...)
		}, ErrLayerMediaType},
		{"no title", false, func(m *ocispec.Manifest) { m.Layers[0].Annotations = nil }, ErrLayerTitle},
		{"empty title", false, func(m *ocispec.Manifest) { m.Layers[0].Annotations[ocispec.AnnotationTitle] = "" }, ErrLayerTitle},
		{"directory in a deb title", false, func(m *ocispec.Manifest) {
			m.Layers[0].Annotations[ocispec.AnnotationTitle] = "pool/nginx.deb"
		}, ErrLayerTitle},
		{"metadata missing", false, func(m *ocispec.Manifest) { m.Layers = m.Layers[:1] }, ErrMetadata},
		{"metadata not last", false, func(m *ocispec.Manifest) { m.Layers[0], m.Layers[1] = m.Layers[1], m.Layers[0] }, ErrMetadata},
		{"two metadata layers", false, func(m *ocispec.Manifest) { m.Layers = append(m.Layers, m.Layers[1]) }, ErrMetadata},
		{"metadata title", false, func(m *ocispec.Manifest) {
			m.Layers[1].Annotations = map[string]string{ocispec.AnnotationTitle: "control.json"}
		}, ErrMetadata},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := debManifest(t)
			if tc.raw {
				m = rawManifest(t)
			}
			tc.mutate(&m)
			p, err := Validate(m)
			if tc.want == nil {
				if err != nil || p == nil {
					t.Fatalf("Validate = %v, %v, want a valid manifest", p, err)
				}
				return
			}
			if p != nil || !errors.Is(err, tc.want) || !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate = %v, %v, want %v", p, err, tc.want)
			}
			for _, other := range sentinels {
				if !errors.Is(other, tc.want) && errors.Is(err, other) {
					t.Errorf("Validate = %v, which also matches %v", err, other)
				}
			}
		})
	}
}

func TestValidateReportsAll(t *testing.T) {
	m := debManifest(t)
	m.MediaType = ""
	m.Config.Size = 0
	delete(m.Annotations, AnnotationName)
	m.Annotations[AnnotationFormat] = "maven"
	m.Layers[0].MediaType = MediaTypeMavenPom
	delete(m.Layers[0].Annotations, ocispec.AnnotationTitle)
	m.Layers = append(m.Layers, m.Layers[1])
	_, err := Validate(m)
	for _, want := range []error{ErrMediaType, ErrConfig, ErrAnnotation, ErrFormatMismatch, ErrLayerMediaType, ErrLayerTitle, ErrMetadata} {
		if !errors.Is(err, want) {
			t.Errorf("Validate = %v, want it to include %v", err, want)
		}
	}
}

// checkValidate asserts that Validate does not contradict itself on m: an error comes without a
// Parsed, and a Parsed builds back into a manifest that validates to the same Parsed (with
// created at whole seconds, as Map writes it).
func checkValidate(t *testing.T, m ocispec.Manifest) {
	t.Helper()
	p, err := Validate(m)
	if err != nil {
		if p != nil || !errors.Is(err, ErrInvalid) {
			t.Fatalf("Validate = %v, %v: an error must wrap ErrInvalid and come without a Parsed", p, err)
		}
		return
	}
	if p == nil {
		t.Fatal("Validate returned neither a Parsed nor an error")
	}
	m2, err := Build(p.Annotations, p.Files, p.Metadata)
	if err != nil {
		t.Fatalf("Build(Validate(m)) = %v", err)
	}
	p2, err := Validate(m2)
	if err != nil {
		t.Fatalf("Validate(Build(Validate(m))) = %v", err)
	}
	p.Annotations.Created = p.Annotations.Created.Truncate(time.Second)
	if !p2.Annotations.Created.Equal(p.Annotations.Created) {
		t.Fatalf("created %v became %v", p.Annotations.Created, p2.Annotations.Created)
	}
	p2.Annotations.Created = p.Annotations.Created
	if !reflect.DeepEqual(p, p2) {
		t.Fatalf("Validate(Build(Validate(m))) =\n%+v\nwant\n%+v", p2, p)
	}
}

func FuzzValidate(f *testing.F) {
	for _, m := range []ocispec.Manifest{debManifest(f), rawManifest(f)} {
		b, _, err := Encode(m)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`))
	f.Add([]byte(debManifestJSON[:len(debManifestJSON)/2]))
	f.Add([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"artifactType":"application/vnd.lajstrom.raw.v1","config":{"mediaType":"application/vnd.oci.empty.v1+json",` +
		`"digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},` +
		`"layers":[{"mediaType":"application/vnd.lajstrom.raw.file.v1","digest":"sha256:00","size":1,` +
		`"annotations":{"org.opencontainers.image.title":"a"}}],"annotations":{"lajstrom.schema":"1",` +
		`"lajstrom.format":"raw","lajstrom.repo":"r","lajstrom.name":"a","lajstrom.version":"1",` +
		`"org.opencontainers.image.created":"2026-11-06T10:00:00.123+05:00"}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		var m ocispec.Manifest
		if json.Unmarshal(b, &m) != nil {
			return
		}
		checkValidate(t, m)
	})
}

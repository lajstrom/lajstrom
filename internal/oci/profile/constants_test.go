package profile

import (
	"slices"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestArtifactTypes(t *testing.T) {
	for _, typ := range Types() {
		at := ArtifactType(typ.Name)
		if got, ok := ParseArtifactType(at); !ok || got != typ.Name {
			t.Errorf("ParseArtifactType(%q) = %q, %v", at, got, ok)
		}
		mt := MetadataMediaType(typ.Name)
		if !IsMetadataMediaType(mt) || IsMetadataMediaType(at) {
			t.Errorf("IsMetadataMediaType: %q should be metadata, %q not", mt, at)
		}
		for _, f := range typ.FileMediaTypes {
			if IsMetadataMediaType(f) {
				t.Errorf("IsMetadataMediaType(%q) = true for a file role", f)
			}
		}
	}
	if got := ArtifactType("deb"); got != "application/vnd.lajstrom.deb.v1" {
		t.Errorf("ArtifactType(deb) = %q", got)
	}
	if got := MetadataMediaType("deb"); got != "application/vnd.lajstrom.deb.metadata.v1+json" {
		t.Errorf("MetadataMediaType(deb) = %q", got)
	}
	for _, s := range []string{
		"", "application/vnd.lajstrom..v1", "application/vnd.lajstrom.Deb.v1", "application/vnd.lajstrom.deb.v2",
		"application/vnd.lajstrom.deb.metadata.v1+json", "application/vnd.oci.image.manifest.v1+json",
	} {
		if typ, ok := ParseArtifactType(s); ok {
			t.Errorf("ParseArtifactType(%q) = %q, true", s, typ)
		}
	}
}

func TestTypes(t *testing.T) {
	var names []string
	for _, typ := range Types() {
		names = append(names, typ.Name)
		if typ.MetadataRequired != (typ.Name != "raw") {
			t.Errorf("type %s: MetadataRequired = %v", typ.Name, typ.MetadataRequired)
		}
	}
	if want := []string{"raw", "pypi", "deb", "maven"}; !slices.Equal(names, want) {
		t.Errorf("Types() = %v, want %v", names, want)
	}
	if typ, ok := TypeByFormat("apt"); !ok || typ.Name != "deb" {
		t.Errorf("TypeByFormat(apt) = %+v, %v, want deb", typ, ok)
	}
	if typ, ok := TypeByName("deb"); !ok || typ.Format != "apt" || !slices.Equal(typ.FileMediaTypes, []string{MediaTypeDebPackage}) {
		t.Errorf("TypeByName(deb) = %+v, %v", typ, ok)
	}
	if _, ok := TypeByFormat("deb"); ok {
		t.Error("TypeByFormat(deb) found a type; deb is a type name, the format is apt")
	}
	if _, ok := TypeByName("rpm"); ok {
		t.Error("TypeByName(rpm) found a type")
	}
	// The results are copies.
	Types()[0].FileMediaTypes[0] = "x"
	typ, _ := TypeByName("raw")
	typ.FileMediaTypes[0] = "y"
	if raw, _ := TypeByName("raw"); raw.FileMediaTypes[0] != MediaTypeRawFile {
		t.Errorf("the type table was modified through a result: %v", raw.FileMediaTypes)
	}
}

func TestEmptyConfig(t *testing.T) {
	want := ocispec.DescriptorEmptyJSON
	if EmptyConfig.MediaType != want.MediaType || EmptyConfig.Digest != want.Digest || EmptyConfig.Size != want.Size {
		t.Errorf("EmptyConfig = %+v, want the descriptor of %+v", EmptyConfig, want)
	}
	if EmptyConfig.Digest != digest.FromString("{}") || EmptyConfig.Data != nil {
		t.Errorf("EmptyConfig = %+v, want digest of {} and no data", EmptyConfig)
	}
}

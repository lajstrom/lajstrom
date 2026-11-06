package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Layer is one layer of a Lajstrom manifest: a file layer or the metadata layer.
type Layer struct {
	// MediaType is the layer media type: a file role of the type or its metadata media type.
	MediaType string
	// Digest is the digest of the blob.
	Digest digest.Digest
	// Size is the size of the blob in bytes.
	Size int64
	// Title is org.opencontainers.image.title, the exact file name.
	Title string
	// Annotations are the other layer annotations, for example lajstrom.maven.extension; nil
	// when there are none. Build adds Title to them in the manifest.
	Annotations map[string]string
}

// Parsed is a manifest that satisfies the profile, as Validate returns it.
type Parsed struct {
	// Type is the type named by artifactType.
	Type Type
	// Annotations are the parsed manifest annotations.
	Annotations Annotations
	// Files are the file layers in manifest order.
	Files []Layer
	// Metadata is the metadata layer; nil when the manifest has none (allowed for raw only).
	Metadata *Layer
}

// Build assembles the manifest of docs/spec/manifest-profile.md §1: schemaVersion 2, the OCI
// image manifest media type, the artifactType of the type whose protocol format is a.Format,
// EmptyConfig, the file layers followed by the metadata layer, and a.Map() as annotations.
// metadata may be nil; its MediaType and Title default to the type's metadata media type and
// MetadataFileName. Build does not modify its arguments. The result is checked with Validate,
// and its errors are returned as they are.
func Build(a Annotations, files []Layer, metadata *Layer) (ocispec.Manifest, error) {
	typ, ok := TypeByFormat(a.Format)
	if !ok {
		return ocispec.Manifest{}, fmt.Errorf("%w: %s %q has no artifact type", ErrArtifactType, AnnotationFormat, a.Format)
	}
	layers := make([]ocispec.Descriptor, 0, len(files)+1)
	for _, f := range files {
		layers = append(layers, f.descriptor())
	}
	if metadata != nil {
		md := *metadata
		if md.MediaType == "" {
			md.MediaType = MetadataMediaType(typ.Name)
		}
		if md.Title == "" {
			md.Title = MetadataFileName
		}
		layers = append(layers, md.descriptor())
	}
	var m ocispec.Manifest
	m.SchemaVersion = 2
	m.MediaType = ocispec.MediaTypeImageManifest
	m.ArtifactType = ArtifactType(typ.Name)
	m.Config = EmptyConfig
	m.Layers = layers
	m.Annotations = a.Map()
	if _, err := Validate(m); err != nil {
		return ocispec.Manifest{}, err
	}
	return m, nil
}

// descriptor returns the layer descriptor: Title is set as org.opencontainers.image.title, or
// removed when empty.
func (l Layer) descriptor() ocispec.Descriptor {
	ann := make(map[string]string, len(l.Annotations)+1)
	maps.Copy(ann, l.Annotations)
	delete(ann, ocispec.AnnotationTitle)
	if l.Title != "" {
		ann[ocispec.AnnotationTitle] = l.Title
	}
	if len(ann) == 0 {
		ann = nil
	}
	return ocispec.Descriptor{MediaType: l.MediaType, Digest: l.Digest, Size: l.Size, Annotations: ann}
}

// Encode returns the canonical bytes of a manifest, json.Marshal without indentation, and their
// digest. These are the bytes to push: the digest identifies them, not the manifest value.
func Encode(m ocispec.Manifest) ([]byte, digest.Digest, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, "", fmt.Errorf("profile: encode manifest: %w", err)
	}
	return b, digest.FromBytes(b), nil
}

// Validate checks every rule of docs/spec/manifest-profile.md (§1, §5) that the manifest alone
// can answer and returns the parsed manifest. Each rule has its own sentinel (ErrMediaType,
// ErrArtifactType, ErrConfig, ErrAnnotation, ErrFormatMismatch, ErrNoLayers, ErrLayerMediaType,
// ErrLayerDescriptor, ErrLayerTitle, ErrMetadata), all wrapping ErrInvalid; all problems are
// joined into one error.
// It does not compare the annotations with the repository path and tag the manifest is pushed
// to, check that the blobs exist, or run the plugin's checks: ingest does. Subject, layer URLs,
// data and platform are not looked at. On error the Parsed is nil.
func Validate(m ocispec.Manifest) (*Parsed, error) {
	var errs []error
	errs = append(errs, checkHeader(m)...)
	typ, ok, err := checkType(m.ArtifactType)
	errs = append(errs, err)
	errs = append(errs, checkConfig(m.Config))
	a, err := ParseAnnotations(m.Annotations)
	errs = append(errs, err)
	if f := m.Annotations[AnnotationFormat]; ok && f != "" && f != typ.Format {
		errs = append(errs, fmt.Errorf("%w: %s is %q, artifactType %s needs %q",
			ErrFormatMismatch, AnnotationFormat, f, m.ArtifactType, typ.Format))
	}
	files, metadata, layerErrs := checkLayers(m.Layers, typ, ok)
	errs = append(errs, layerErrs...)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Parsed{Type: typ, Annotations: a, Files: files, Metadata: metadata}, nil
}

// checkHeader checks schemaVersion and mediaType.
func checkHeader(m ocispec.Manifest) []error {
	var errs []error
	if m.MediaType != ocispec.MediaTypeImageManifest {
		errs = append(errs, fmt.Errorf("%w: %q, want %q", ErrMediaType, m.MediaType, ocispec.MediaTypeImageManifest))
	}
	if m.SchemaVersion != 2 {
		errs = append(errs, fmt.Errorf("%w: schemaVersion %d, want 2", ErrMediaType, m.SchemaVersion))
	}
	return errs
}

// checkType returns the type named by artifactType; ok is false when there is none.
func checkType(artifactType string) (Type, bool, error) {
	name, ok := ParseArtifactType(artifactType)
	if !ok {
		return Type{}, false, fmt.Errorf("%w: %q is not %s<type>%s", ErrArtifactType, artifactType, ArtifactTypePrefix, artifactTypeSuffix)
	}
	typ, ok := TypeByName(name)
	if !ok {
		return Type{}, false, fmt.Errorf("%w: unknown type %q", ErrArtifactType, name)
	}
	return typ, true, nil
}

// checkConfig compares the config descriptor with EmptyConfig: media type, digest and size.
func checkConfig(c ocispec.Descriptor) error {
	if c.MediaType != EmptyConfig.MediaType || c.Digest != EmptyConfig.Digest || c.Size != EmptyConfig.Size {
		return fmt.Errorf("%w: %s %s size %d is not the empty descriptor", ErrConfig, c.MediaType, c.Digest, c.Size)
	}
	return nil
}

// checkLayers checks the layer list and splits it into file layers and the metadata layer.
// Without a known type (known false) it checks only that there are layers and that they have
// titles; with one, that at least one of them is a file layer.
func checkLayers(ds []ocispec.Descriptor, typ Type, known bool) ([]Layer, *Layer, []error) {
	if len(ds) == 0 {
		return nil, nil, []error{ErrNoLayers}
	}
	var (
		errs     []error
		files    []Layer
		metadata []int
	)
	metaType := MetadataMediaType(typ.Name)
	for i, d := range ds {
		errs = append(errs, checkDescriptor(i, d), checkTitle(i, d, typ.Name))
		switch {
		case !known:
		case d.MediaType == metaType:
			metadata = append(metadata, i)
			continue
		case !slices.Contains(typ.FileMediaTypes, d.MediaType):
			errs = append(errs, fmt.Errorf("%w: layer %d: %q is not a file or metadata media type of type %s",
				ErrLayerMediaType, i, d.MediaType, typ.Name))
		}
		files = append(files, layerOf(d))
	}
	if !known {
		return files, nil, errs
	}
	if len(files) == 0 {
		errs = append(errs, ErrNoLayers)
	}
	errs = append(errs, checkMetadata(ds, metadata, typ)...)
	if len(metadata) == 0 {
		return files, nil, errs
	}
	md := layerOf(ds[metadata[0]])
	return files, &md, errs
}

// checkDescriptor checks that layer i has a well-formed digest and a non-negative size.
func checkDescriptor(i int, d ocispec.Descriptor) error {
	if err := d.Digest.Validate(); err != nil {
		return fmt.Errorf("%w: layer %d: digest %q: %w", ErrLayerDescriptor, i, d.Digest, err)
	}
	if d.Size < 0 {
		return fmt.Errorf("%w: layer %d: size %d", ErrLayerDescriptor, i, d.Size)
	}
	return nil
}

// checkTitle checks that layer i has a title, and a plain file name unless the type is raw.
func checkTitle(i int, d ocispec.Descriptor, typ string) error {
	title := d.Annotations[ocispec.AnnotationTitle]
	switch {
	case title == "":
		return fmt.Errorf("%w: layer %d has no %s", ErrLayerTitle, i, ocispec.AnnotationTitle)
	case typ != "raw" && strings.Contains(title, "/"):
		return fmt.Errorf("%w: layer %d: %q is not a file name", ErrLayerTitle, i, title)
	}
	return nil
}

// checkMetadata checks the metadata layers, given by their indices in ds.
func checkMetadata(ds []ocispec.Descriptor, metadata []int, typ Type) []error {
	var errs []error
	if len(metadata) > 1 {
		errs = append(errs, fmt.Errorf("%w: %d metadata layers, at most one", ErrMetadata, len(metadata)))
	}
	if len(metadata) == 0 && typ.MetadataRequired {
		errs = append(errs, fmt.Errorf("%w: type %s requires one", ErrMetadata, typ.Name))
	}
	for _, i := range metadata {
		if i != len(ds)-1 {
			errs = append(errs, fmt.Errorf("%w: layer %d of %d, must be the last", ErrMetadata, i, len(ds)))
		}
		if t := ds[i].Annotations[ocispec.AnnotationTitle]; t != "" && t != MetadataFileName {
			errs = append(errs, fmt.Errorf("%w: layer %d: title %q, want %q", ErrMetadata, i, t, MetadataFileName))
		}
	}
	return errs
}

// layerOf converts a descriptor to a Layer; the title moves from the annotations to Title.
func layerOf(d ocispec.Descriptor) Layer {
	l := Layer{MediaType: d.MediaType, Digest: d.Digest, Size: d.Size, Title: d.Annotations[ocispec.AnnotationTitle]}
	for k, v := range d.Annotations {
		if k == ocispec.AnnotationTitle {
			continue
		}
		if l.Annotations == nil {
			l.Annotations = map[string]string{}
		}
		l.Annotations[k] = v
	}
	return l
}

package profile

import (
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// SchemaVersion is the value of lajstrom.schema this package writes and accepts.
	SchemaVersion = "1"

	// MetadataFileName is the org.opencontainers.image.title of the metadata layer.
	MetadataFileName = "metadata.json"

	// ArtifactTypePrefix starts every Lajstrom artifact type and layer media type.
	ArtifactTypePrefix = "application/vnd.lajstrom."
)

// File layer media types (docs/spec/manifest-profile.md §2).
const (
	// MediaTypeRawFile is any file of a raw repo.
	MediaTypeRawFile = "application/vnd.lajstrom.raw.file.v1"
	// MediaTypePypiWheel is a wheel, *.whl.
	MediaTypePypiWheel = "application/vnd.lajstrom.pypi.wheel.v1+zip"
	// MediaTypePypiSdistTarGz is a source distribution, *.tar.gz.
	MediaTypePypiSdistTarGz = "application/vnd.lajstrom.pypi.sdist.v1.tar+gzip"
	// MediaTypePypiSdistZip is a legacy source distribution, *.zip.
	MediaTypePypiSdistZip = "application/vnd.lajstrom.pypi.sdist.v1+zip"
	// MediaTypeDebPackage is a Debian binary package, *.deb.
	MediaTypeDebPackage = "application/vnd.lajstrom.deb.package.v1"
	// MediaTypeMavenArtifact is a maven artifact file other than the POM: jar, war, aar, zip, …
	MediaTypeMavenArtifact = "application/vnd.lajstrom.maven.artifact.v1"
	// MediaTypeMavenPom is a maven POM, *.pom.
	MediaTypeMavenPom = "application/vnd.lajstrom.maven.pom.v1+xml"
)

// Annotation keys (docs/spec/manifest-profile.md §3). The OCI keys are used from ocispec:
// AnnotationCreated (required), AnnotationTitle (every layer), AnnotationSource and
// AnnotationRevision (passed through).
const (
	// AnnotationSchema is the profile version, always SchemaVersion. Required.
	AnnotationSchema = "lajstrom.schema"
	// AnnotationFormat is the repo format, the protocol: raw, pypi, apt, maven. Required.
	AnnotationFormat = "lajstrom.format"
	// AnnotationRepo is the repo name. Required.
	AnnotationRepo = "lajstrom.repo"
	// AnnotationName is the package name in the format's own spelling. Required.
	AnnotationName = "lajstrom.name"
	// AnnotationVersion is the version exactly as the format spells it. Required.
	AnnotationVersion = "lajstrom.version"
	// AnnotationVariant is the platform discriminator: deb suite/component/arch, pypi wheel tag
	// or sdist.
	AnnotationVariant = "lajstrom.variant"
	// AnnotationUploadedBy is the principal that performed the ingest (informational).
	AnnotationUploadedBy = "lajstrom.uploaded-by"
	// AnnotationOriginURL is the URL a proxied artifact's first file was asked for, before any
	// redirect (§3.1). Only the proxy fetcher writes it, through ingest.
	AnnotationOriginURL = "lajstrom.origin.url"
	// AnnotationOriginChecksum is the upstream's checksum that was verified before a proxied
	// artifact was ingested, <algorithm>:<lowercase hex> (§3.1).
	AnnotationOriginChecksum = "lajstrom.origin.checksum"
	// AnnotationOriginPrefix starts every key that only the proxy fetcher writes: a manifest a
	// client pushes or uploads with one is refused (§3.1).
	AnnotationOriginPrefix = "lajstrom.origin."
	// AnnotationDebArch is the Debian architecture of the upload.
	AnnotationDebArch = "lajstrom.deb.arch"
	// AnnotationDebSuite is the apt suite of the upload.
	AnnotationDebSuite = "lajstrom.deb.suite"
	// AnnotationDebComponent is the apt component of the upload.
	AnnotationDebComponent = "lajstrom.deb.component"
	// AnnotationPypiNonstandard is "true" on a pypi version that is not a PEP 440 version.
	AnnotationPypiNonstandard = "lajstrom.pypi.nonstandard"
	// AnnotationMavenGroupID is the maven groupId.
	AnnotationMavenGroupID = "lajstrom.maven.groupId"
	// AnnotationMavenArtifactID is the maven artifactId.
	AnnotationMavenArtifactID = "lajstrom.maven.artifactId"
	// AnnotationMavenPackaging is the maven packaging.
	AnnotationMavenPackaging = "lajstrom.maven.packaging"
	// AnnotationMavenExtension is the file extension of a maven artifact layer (layer annotation).
	AnnotationMavenExtension = "lajstrom.maven.extension"
	// AnnotationMavenClassifier is the classifier of a maven artifact layer (layer annotation).
	AnnotationMavenClassifier = "lajstrom.maven.classifier"
	// AnnotationRawPath is the original path of a raw file, case preserved, /-separated.
	AnnotationRawPath = "lajstrom.raw.path"
)

const (
	artifactTypeSuffix = ".v1"
	metadataSuffix     = ".metadata.v1+json"
)

// EmptyConfig is the config descriptor of every Lajstrom manifest: the OCI empty descriptor,
// the 2-byte blob {}. Unlike ocispec.DescriptorEmptyJSON it has no Data field, so manifests do
// not embed the blob. Validate compares MediaType, Digest and Size and ignores the rest.
// Treat it as a constant.
var EmptyConfig = ocispec.Descriptor{
	MediaType: ocispec.MediaTypeEmptyJSON,
	Digest:    digest.Digest("sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"),
	Size:      2,
}

// ArtifactType returns the manifest artifactType of a type: application/vnd.lajstrom.<typ>.v1.
func ArtifactType(typ string) string {
	return ArtifactTypePrefix + typ + artifactTypeSuffix
}

// ParseArtifactType is the inverse of ArtifactType. It reports false when s does not have the
// shape application/vnd.lajstrom.<typ>.v1 with typ made of [a-z0-9]; it does not check that typ
// is a known type (see TypeByName).
func ParseArtifactType(s string) (typ string, ok bool) {
	return cutType(s, artifactTypeSuffix)
}

// MetadataMediaType returns the media type of the metadata layer of a type:
// application/vnd.lajstrom.<typ>.metadata.v1+json.
func MetadataMediaType(typ string) string {
	return ArtifactTypePrefix + typ + metadataSuffix
}

// IsMetadataMediaType reports whether mt is the metadata media type of some type.
func IsMetadataMediaType(mt string) bool {
	_, ok := cutType(mt, metadataSuffix)
	return ok
}

// cutType returns <typ> from ArtifactTypePrefix + <typ> + suffix when typ is a type name.
func cutType(s, suffix string) (string, bool) {
	rest, ok := strings.CutPrefix(s, ArtifactTypePrefix)
	if !ok {
		return "", false
	}
	typ, ok := strings.CutSuffix(rest, suffix)
	if !ok || !isTypeName(typ) {
		return "", false
	}
	return typ, true
}

// isTypeName reports whether s is non-empty and made of [a-z0-9].
func isTypeName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// Type describes one artifact type of the profile.
type Type struct {
	// Name is the type in artifactType and in the layer media types: "deb".
	Name string
	// Format is the protocol format, the value of lajstrom.format: "apt" for type deb.
	Format string
	// FileMediaTypes are the media types of the file layers (the roles of §2).
	FileMediaTypes []string
	// MetadataRequired is true when the manifest must have a metadata layer.
	MetadataRequired bool
}

// types is the type table: artifactType deb is protocol format apt (§1). A later type is added
// here together with its plugin.
var types = []Type{
	{Name: "raw", Format: "raw", FileMediaTypes: []string{MediaTypeRawFile}},
	{
		Name: "pypi", Format: "pypi", MetadataRequired: true,
		FileMediaTypes: []string{MediaTypePypiWheel, MediaTypePypiSdistTarGz, MediaTypePypiSdistZip},
	},
	{Name: "deb", Format: "apt", FileMediaTypes: []string{MediaTypeDebPackage}, MetadataRequired: true},
	{
		Name: "maven", Format: "maven", MetadataRequired: true,
		FileMediaTypes: []string{MediaTypeMavenArtifact, MediaTypeMavenPom},
	},
}

// Types returns every known type: raw, pypi, deb, maven. The result is a copy.
func Types() []Type {
	out := make([]Type, len(types))
	for i, t := range types {
		out[i] = t.clone()
	}
	return out
}

// TypeByName returns the type with the given name ("deb").
func TypeByName(typ string) (Type, bool) {
	return lookup(func(t Type) bool { return t.Name == typ })
}

// TypeByFormat returns the type of a protocol format ("apt" gives type deb).
func TypeByFormat(format string) (Type, bool) {
	return lookup(func(t Type) bool { return t.Format == format })
}

func lookup(match func(Type) bool) (Type, bool) {
	i := slices.IndexFunc(types, match)
	if i < 0 {
		return Type{}, false
	}
	return types[i].clone(), true
}

func (t Type) clone() Type {
	t.FileMediaTypes = slices.Clone(t.FileMediaTypes)
	return t
}

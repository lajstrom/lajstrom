package maven

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// The file a maven proxy repo fetches (docs/spec/manifest-profile.md §4.2, proxy-and-group.md
// §5): a Maven 2 repository lists no files of a version, so a proxy fetches each file a client
// asks for alone, and keeps it as a version of that one file with a variant, its extension and
// classifier. Its bytes stream from the upstream into the registry (Publisher.UploadStream),
// checked against the upstream's sha1, and are not read: a POM's neither.

// ParamPath is the upload parameter with the path below /maven/<repo>/ of the one file of a
// fetch, com/example/lib-core/1.2.0/lib-core-1.2.0.jar: the proxy's entry gives it
// (format.UpstreamEntry.Params), since a streamed file is named by its file name alone.
const ParamPath = "path"

var _ format.Streamer = Plugin{}

// Inspect implements format.Streamer for the file a proxy repo fetches: ingest computes its md5,
// sha1 and sha512 besides the sha256, and nothing else is read of its bytes. A deploy does not
// stream through it: its files go to the publication sessions (format.Sessions). A file whose
// ParamPath is not its path wraps format.ErrInvalid.
func (Plugin) Inspect(_ index.Repo, up format.StreamUpload) (format.Inspection, error) {
	if _, err := fetchedFile(up.Params, up.File.Name); err != nil {
		return format.Inspection{}, err
	}
	return format.Inspection{Checksums: []string{MD5, SHA1, SHA512}}, nil
}

// Variant returns the variant of a file a proxy repo fetched: its extension, and ":" and its
// classifier when it has one, in the order of Maven's coordinates: jar, jar:sources, pom,
// jar.asc, exe:linux-x86_64. No extension or classifier has a ":", so it names one file of the
// version.
func Variant(f File) string {
	if f.Classifier == "" {
		return f.Extension
	}
	return f.Extension + ":" + f.Classifier
}

// fetchedFile returns the file at the ParamPath of params, a release's or a build's, which must
// be called name.
func fetchedFile(params map[string]string, name string) (File, error) {
	p := params[ParamPath]
	if p == "" {
		return File{}, fmt.Errorf("%w: a fetched maven file comes with its path, the parameter %s", format.ErrInvalid, ParamPath)
	}
	f, err := ParsePath(p)
	if err != nil {
		return File{}, err
	}
	if f.Filename() != name {
		return File{}, fmt.Errorf("%w: the fetched file %q is not the file of its path %q", format.ErrInvalid, name, p)
	}
	return f, nil
}

// buildFetched is Build of the one file a proxy repo fetched (in.Origin set): a manifest of one
// file layer, described from the checksums and the size that ingest computed as it streamed;
// lajstrom.variant is the file's Variant, there is no lajstrom.maven.packaging, and the metadata
// layer has the one file, the packaging "" and an empty pom. The tag is profile.Tag(version,
// variant). Errors wrap format.ErrInvalid.
func buildFetched(repo index.Repo, in format.Upload) (*format.Built, error) {
	if len(in.Files) != 1 {
		return nil, fmt.Errorf("%w: a fetched maven file is one file, this upload %d", format.ErrInvalid, len(in.Files))
	}
	up := in.Files[0]
	f, err := fetchedFile(in.Params, up.Name)
	if err != nil {
		return nil, err
	}
	r := received{up: up, file: f, md: fileMetadata{Name: f.Filename(), Extension: f.Extension, Classifier: f.Classifier, Size: up.Size}}
	for _, a := range algorithms {
		v, ok := up.Checksums[a.name]
		if !ok || up.Size < 0 {
			return nil, fmt.Errorf("%w: the fetched file %s streamed without its %s or its size", format.ErrInvalid, up.Name, a.name)
		}
		*r.md.checksum(a.name) = v
	}
	if err := r.md.checkChecksums(); err != nil {
		return nil, err
	}
	c, variant := f.Coordinates, Variant(f)
	md, err := json.Marshal(metadata{GroupID: c.GroupID, ArtifactID: c.ArtifactID, Version: c.Version, Files: []fileMetadata{r.md},
		POM: pomEssentials{Dependencies: []Dependency{}}})
	if err != nil {
		return nil, fmt.Errorf("maven: metadata of %s: %w", f.Filename(), err)
	}
	layer := r.layer()
	attrs := map[string]string{profile.AnnotationMavenGroupID: c.GroupID, profile.AnnotationMavenArtifactID: c.ArtifactID}
	m, err := profile.Build(profile.Annotations{
		Format: Name, Repo: repo.Name, Name: c.Name(), Version: c.Version, Variant: variant,
		Created: time.Now().UTC().Truncate(time.Second), Attrs: attrs,
	}, []profile.Layer{layer}, &profile.Layer{Digest: digest.FromBytes(md), Size: int64(len(md))})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	return &format.Built{
		Manifest: m,
		Blobs:    []format.BlobSource{bytesBlob(profile.EmptyConfig, ocispec.DescriptorEmptyJSON.Data), bytesBlob(m.Layers[1], md)},
		Entry: format.Entry{Name: c.Name(), Version: c.Version, Variant: variant, Attrs: attrs, Metadata: md,
			Files: []format.FileEntry{{Name: layer.Title, MediaType: layer.MediaType, Digest: layer.Digest, Size: layer.Size, Attrs: f.annotations()}}},
		Tag:      profile.Tag(c.Version, variant),
		RepoPath: profile.RepositoryPath("", Name, repo.Name, packagePath(c.GroupID, c.ArtifactID)),
	}, nil
}

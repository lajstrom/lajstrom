package rpm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// maxMetadata is the largest metadata layer Build writes and Validate reads: 16 MiB. The file
// list and the changelog are its bulk.
const maxMetadata = 16 << 20

// maxIndexedMetadata is the largest metadata layer the entry gives the index for
// versions.metadata: 4 MiB, the bound of a metadata layer in the database (guarantees.md,
// INV-1). A larger one is left out of the row, and the renderer reads it from the registry.
const maxIndexedMetadata = 4 << 20

// indexed returns md for versions.metadata, nil when it is larger than maxIndexedMetadata.
func indexed(md []byte) json.RawMessage {
	if len(md) > maxIndexedMetadata {
		return nil
	}
	return md
}

// metadata is the content of the rpm metadata layer (docs/spec/manifest-profile.md §2): what the
// repodata of a version needs, read from the header, and the file name, size and sha256 of the
// file. Build writes it and Validate reads it; Validate refuses one that is not what the header
// of the file layer makes.
type metadata struct {
	Name          string      `json:"name"`
	Epoch         int64       `json:"epoch"`
	Version       string      `json:"version"`
	Release       string      `json:"release"`
	Arch          string      `json:"arch"`
	Summary       string      `json:"summary"`
	Description   string      `json:"description"`
	Packager      string      `json:"packager"`
	URL           string      `json:"url"`
	License       string      `json:"license"`
	Vendor        string      `json:"vendor"`
	Group         string      `json:"group"`
	BuildHost     string      `json:"buildhost"`
	SourceRPM     string      `json:"sourcerpm"`
	BuildTime     int64       `json:"buildtime"`
	InstalledSize int64       `json:"installedSize"`
	ArchiveSize   int64       `json:"archiveSize"`
	HeaderStart   int64       `json:"headerStart"`
	HeaderEnd     int64       `json:"headerEnd"`
	Provides      []relation  `json:"provides"`
	Requires      []relation  `json:"requires"`
	Conflicts     []relation  `json:"conflicts"`
	Obsoletes     []relation  `json:"obsoletes"`
	Recommends    []relation  `json:"recommends"`
	Suggests      []relation  `json:"suggests"`
	Supplements   []relation  `json:"supplements"`
	Enhances      []relation  `json:"enhances"`
	Files         []file      `json:"files"`
	Changelogs    []changelog `json:"changelogs"`
	Filename      string      `json:"filename"`
	Size          int64       `json:"size"`
	SHA256        string      `json:"sha256"`
}

// relation is an entry of a relation in the metadata layer.
type relation struct {
	Name    string `json:"name"`
	Flags   int64  `json:"flags"`
	Epoch   string `json:"epoch"`
	Version string `json:"version"`
	Release string `json:"release"`
	Pre     bool   `json:"pre"`
}

// file is a file in the metadata layer; Type is dir, ghost or absent.
type file struct {
	Path string `json:"path"`
	Type string `json:"type,omitempty"`
}

// changelog is a changelog entry in the metadata layer.
type changelog struct {
	Author string `json:"author"`
	Date   int64  `json:"date"`
	Text   string `json:"text"`
}

// relations returns the eight relations of md in the order of Package.Relations.
func (md *metadata) relations() [8]*[]relation {
	return [8]*[]relation{&md.Provides, &md.Requires, &md.Conflicts, &md.Obsoletes, &md.Recommends, &md.Suggests,
		&md.Supplements, &md.Enhances}
}

// metadataOf returns the metadata of pkg.
func metadataOf(pkg *Package) *metadata {
	md := &metadata{
		Name: pkg.Name, Epoch: pkg.Epoch, Version: pkg.Version, Release: pkg.Release, Arch: pkg.Arch,
		Summary: pkg.Summary, Description: pkg.Description, Packager: pkg.Packager, URL: pkg.URL, License: pkg.License,
		Vendor: pkg.Vendor, Group: pkg.Group, BuildHost: pkg.BuildHost, SourceRPM: pkg.SourceRPM, BuildTime: pkg.BuildTime,
		InstalledSize: pkg.InstalledSize, ArchiveSize: pkg.ArchiveSize, HeaderStart: pkg.HeaderStart, HeaderEnd: pkg.HeaderEnd,
		Files: make([]file, len(pkg.Files)), Changelogs: make([]changelog, len(pkg.Changelogs)),
		Filename: pkg.Filename, Size: pkg.Size, SHA256: pkg.SHA256,
	}
	for i, dst := range md.relations() {
		*dst = make([]relation, len(pkg.Relations[i]))
		for j, r := range pkg.Relations[i] {
			(*dst)[j] = relation(r)
		}
	}
	for i, f := range pkg.Files {
		md.Files[i] = file(f)
	}
	for i, c := range pkg.Changelogs {
		md.Changelogs[i] = changelog(c)
	}
	return md
}

// Metadata returns the metadata layer of a package that Parse read, as Build writes it
// (docs/spec/manifest-profile.md §2), compact JSON. One larger than 16 MiB wraps
// format.ErrInvalid: Validate would not read it. lajstrom admin rpm metadata prints it, for the
// metadata layer of an oras push.
func (pkg *Package) Metadata() ([]byte, error) {
	md, err := json.Marshal(metadataOf(pkg))
	if err != nil {
		return nil, fmt.Errorf("rpm: metadata of %s: %w", pkg.Filename, err)
	}
	if len(md) > maxMetadata {
		return nil, fmt.Errorf("%w: the metadata of %s has %d bytes, at most %d", format.ErrInvalid, pkg.Filename, len(md), maxMetadata)
	}
	return md, nil
}

// Build makes the manifest of one uploaded .rpm. in.Files holds exactly one file: one streamed
// into the registry (no Open, ADR 0014) is described by what its inspector read and by its size
// and sha256, and left out of Built.Blobs, for ingest commits its upload session; another is
// read once by Parse. The manifest has the file layer titled with the file name, the metadata
// layer, and the annotations name = NAME, version = [EPOCH:]VERSION-RELEASE and variant = the
// arch; the tag is profile.Tag(version, arch), 1.0-1.el9__x86_5f64. created is now, in whole
// seconds. Errors wrap format.ErrInvalid, except a failure to read the file.
func (Plugin) Build(_ context.Context, repo index.Repo, in format.Upload) (*format.Built, error) {
	if len(in.Files) != 1 {
		return nil, fmt.Errorf("%w: an rpm upload is one .rpm, got %d files", format.ErrInvalid, len(in.Files))
	}
	f := in.Files[0]
	pkg, err := parseFile(f)
	if err != nil {
		return nil, err
	}
	md, err := pkg.Metadata()
	if err != nil {
		return nil, err
	}
	d := digest.NewDigestFromEncoded(digest.SHA256, pkg.SHA256)
	version := pkg.EVR()
	layer := profile.Layer{MediaType: profile.MediaTypeRpmPackage, Digest: d, Size: pkg.Size, Title: pkg.Filename}
	m, err := profile.Build(profile.Annotations{
		Format: Name, Repo: repo.Name, Name: pkg.Name, Version: version, Variant: pkg.Arch,
		Created: time.Now().UTC().Truncate(time.Second), UploadedBy: in.Params[ParamUploadedBy],
	}, []profile.Layer{layer}, &profile.Layer{Digest: digest.FromBytes(md), Size: int64(len(md))})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	blobs := []format.BlobSource{bytesBlob(profile.EmptyConfig, ocispec.DescriptorEmptyJSON.Data)}
	if f.Open != nil {
		blobs = append(blobs, format.BlobSource{Descriptor: m.Layers[0], Open: func(context.Context) (io.ReadCloser, error) { return f.Open() }})
	}
	return &format.Built{
		Manifest: m,
		Blobs:    append(blobs, bytesBlob(m.Layers[1], md)),
		Entry: format.Entry{Name: pkg.Name, Version: version, Variant: pkg.Arch, Metadata: indexed(md),
			Files: []format.FileEntry{{Name: pkg.Filename, MediaType: layer.MediaType, Digest: d, Size: pkg.Size}}},
		Tag:      profile.Tag(version, pkg.Arch),
		RepoPath: profile.RepositoryPath("", Name, repo.Name, profile.EscapeComponent(pkg.Name)),
	}, nil
}

// parseFile returns what f says about itself: of a streamed file, streamedPackage; of another,
// what Parse reads from it. A size other than the one f declares wraps format.ErrInvalid.
func parseFile(f format.UploadedFile) (*Package, error) {
	if f.Open == nil {
		return streamedPackage(f)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("rpm: open %q: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	pkg, err := Parse(rc)
	switch {
	case err != nil:
		return nil, fmt.Errorf("rpm: %q: %w", f.Name, err)
	case f.Size >= 0 && pkg.Size != f.Size:
		return nil, fmt.Errorf("%w: %q has %d bytes, the upload says %d", format.ErrInvalid, f.Name, pkg.Size, f.Size)
	}
	return pkg, nil
}

// bytesBlob is a BlobSource of bytes in memory with the media type, digest and size of desc.
func bytesBlob(desc ocispec.Descriptor, b []byte) format.BlobSource {
	return format.BlobSource{
		Descriptor: ocispec.Descriptor{MediaType: desc.MediaType, Digest: desc.Digest, Size: desc.Size},
		Open:       func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil },
	}
}

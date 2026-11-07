package deb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

const (
	// maxMetadata is the largest metadata layer Build writes and Validate reads: 4 MiB, the
	// bound of the control member, since md5sums is the bulk of both.
	maxMetadata = 4 << 20
	// maxRepositoryLen is the longest OCI repository name registries accept.
	maxRepositoryLen = 255
)

// distName is the grammar of a suite and a component: lowercase letters, digits, ".", "+" and
// "-", the first a letter or a digit (jammy, jammy-updates, main, non-free-firmware).
var distName = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]*$`)

// metadata is the content of the deb metadata layer (docs/spec/manifest-profile.md §2): the
// control fields as written, md5sums ({} when the package has none), and the size, the
// checksums and the pool file name of the whole file. The fields are in the order of their
// keys, so that the JSON has its keys sorted.
type metadata struct {
	Control  map[string]string `json:"control"`
	Filename string            `json:"filename"`
	MD5      string            `json:"md5,omitempty"`
	MD5Sums  map[string]string `json:"md5sums"`
	SHA1     string            `json:"sha1,omitempty"`
	SHA256   string            `json:"sha256"`
	Size     int64             `json:"size"`
}

// Build makes the manifest of one uploaded .deb. in.Files holds exactly one file, read once by
// Parse for the control file and the checksums together; in.Params has ParamSuite and
// ParamComponent (distName each) and may have ParamArch, which must then be the Architecture of
// the control file, and ParamUploadedBy. The manifest has the file layer titled with the pool
// file name, the metadata layer, and the annotations name = Package, version = Version,
// variant = Architecture and lajstrom.deb.arch, .suite and .component; the tag is
// profile.Tag(Version, Architecture). created is now, in whole seconds, as for raw. Errors wrap
// format.ErrInvalid, except a failure to read the file.
func (Plugin) Build(_ context.Context, repo index.Repo, in format.Upload) (*format.Built, error) {
	suite, component, err := distParams(in.Params)
	if err != nil {
		return nil, err
	}
	if len(in.Files) != 1 {
		return nil, fmt.Errorf("%w: an apt upload is one .deb, got %d files", format.ErrInvalid, len(in.Files))
	}
	f := in.Files[0]
	pkg, err := parseFile(f)
	if err != nil {
		return nil, err
	}
	if arch := in.Params[ParamArch]; arch != "" && arch != pkg.Architecture {
		return nil, fmt.Errorf("%w: the upload is for architecture %q, the control file of %s says %q",
			format.ErrInvalid, arch, pkg.Filename, pkg.Architecture)
	}
	name := profile.RepositoryPath("", Name, repo.Name, profile.EscapeComponent(pkg.Name))
	if len(name) > maxRepositoryLen {
		return nil, fmt.Errorf("%w: package %q makes the OCI repository name %d bytes long, at most %d",
			format.ErrInvalid, pkg.Name, len(name), maxRepositoryLen)
	}
	md, err := metadataOf(pkg)
	if err != nil {
		return nil, err
	}
	d := digest.NewDigestFromEncoded(digest.SHA256, pkg.SHA256)
	attrs := map[string]string{
		profile.AnnotationDebArch: pkg.Architecture, profile.AnnotationDebSuite: suite, profile.AnnotationDebComponent: component,
	}
	file := profile.Layer{MediaType: profile.MediaTypeDebPackage, Digest: d, Size: pkg.Size, Title: pkg.Filename}
	m, err := profile.Build(profile.Annotations{
		Format: Name, Repo: repo.Name, Name: pkg.Name, Version: pkg.Version, Variant: pkg.Architecture,
		Created: time.Now().UTC().Truncate(time.Second), UploadedBy: in.Params[ParamUploadedBy], Attrs: attrs,
	}, []profile.Layer{file}, &profile.Layer{Digest: digest.FromBytes(md), Size: int64(len(md))})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	return &format.Built{
		Manifest: m,
		Blobs: []format.BlobSource{
			bytesBlob(profile.EmptyConfig, ocispec.DescriptorEmptyJSON.Data),
			{Descriptor: m.Layers[0], Open: func(context.Context) (io.ReadCloser, error) { return f.Open() }},
			bytesBlob(m.Layers[1], md),
		},
		Entry: format.Entry{Name: pkg.Name, Version: pkg.Version, Variant: pkg.Architecture, Attrs: attrs, Metadata: md,
			Files: []format.FileEntry{{Name: pkg.Filename, MediaType: file.MediaType, Digest: d, Size: pkg.Size}}},
		Tag:      profile.Tag(pkg.Version, pkg.Architecture),
		RepoPath: name,
	}, nil
}

// distParams returns the suite and the component of an upload, both required and distName.
func distParams(params map[string]string) (suite, component string, err error) {
	suite, component = params[ParamSuite], params[ParamComponent]
	for _, p := range []struct{ what, value string }{{"parameter " + ParamSuite, suite}, {"parameter " + ParamComponent, component}} {
		if err := checkDistName(p.what, p.value); err != nil {
			return "", "", err
		}
	}
	return suite, component, nil
}

// checkDistName checks a suite or a component, what names it in the error, which wraps
// format.ErrInvalid.
func checkDistName(what, s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%w: %s is missing", format.ErrInvalid, what)
	case !distName.MatchString(s):
		return fmt.Errorf("%w: %s %q is not lowercase letters, digits, \".\", \"+\" and \"-\", the first a letter or a digit",
			format.ErrInvalid, what, s)
	}
	return nil
}

// parseFile reads f once with Parse. A size other than the one f declares wraps
// format.ErrInvalid.
func parseFile(f format.UploadedFile) (*Package, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("deb: open %q: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	pkg, err := Parse(rc)
	switch {
	case err != nil:
		return nil, fmt.Errorf("deb: %q: %w", f.Name, err)
	case f.Size >= 0 && pkg.Size != f.Size:
		return nil, fmt.Errorf("%w: %q has %d bytes, the upload says %d", format.ErrInvalid, f.Name, pkg.Size, f.Size)
	}
	return pkg, nil
}

// metadataOf returns the metadata layer of pkg, compact JSON with its keys sorted. One larger
// than maxMetadata wraps format.ErrInvalid: Validate would not read it.
func metadataOf(pkg *Package) ([]byte, error) {
	sums := pkg.MD5Sums
	if sums == nil {
		sums = map[string]string{}
	}
	md, err := json.Marshal(metadata{Control: pkg.Control.Map(), Filename: pkg.Filename, MD5: pkg.MD5, MD5Sums: sums,
		SHA1: pkg.SHA1, SHA256: pkg.SHA256, Size: pkg.Size})
	if err != nil {
		return nil, fmt.Errorf("deb: metadata of %s: %w", pkg.Filename, err)
	}
	if len(md) > maxMetadata {
		return nil, fmt.Errorf("%w: the metadata of %s has %d bytes, at most %d", format.ErrInvalid, pkg.Filename, len(md), maxMetadata)
	}
	return md, nil
}

// bytesBlob is a BlobSource of bytes in memory with the media type, digest and size of desc.
func bytesBlob(desc ocispec.Descriptor, b []byte) format.BlobSource {
	return format.BlobSource{
		Descriptor: ocispec.Descriptor{MediaType: desc.MediaType, Digest: desc.Digest, Size: desc.Size},
		Open:       func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil },
	}
}

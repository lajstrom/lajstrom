package raw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

const (
	// maxPathLen is the longest path a raw repo stores, in bytes.
	maxPathLen = 4096
	// maxComponentLen is the longest escaped path component; a longer one is hashed
	// (docs/spec/manifest-profile.md §4.1, raw).
	maxComponentLen = 64
	// versionLen is the number of hex digits of the content sha256 that make the version.
	versionLen = 16
	// maxMetadata is the largest metadata layer Validate reads. A raw metadata layer holds a
	// path of at most maxPathLen bytes, a content type and two numbers.
	maxMetadata = 64 << 10
	// defaultContentType is the content type of a file uploaded without one.
	defaultContentType = "application/octet-stream"
)

// Upload parameters the routes pass to Build in format.Upload.Params.
const (
	// ParamPath is the path of the file, as the client sent it after /raw/<repo>/.
	ParamPath = "path"
	// ParamUploadedBy is the principal of the upload, for lajstrom.uploaded-by.
	ParamUploadedBy = "uploaded-by"
)

// metadata is the content of the raw metadata layer (docs/spec/manifest-profile.md §2).
type metadata struct {
	Path        string `json:"path"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// checkPath applies the path rules of a raw repo: not empty, at most maxPathLen bytes, valid
// UTF-8 (the path travels in JSON annotations, which cannot carry other bytes), no byte below
// 0x20 and no 0x7f, and no empty, "." or ".." component. Errors wrap format.ErrInvalid.
func checkPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%w: empty path", format.ErrInvalid)
	case len(p) > maxPathLen:
		return fmt.Errorf("%w: the path has %d bytes, at most %d", format.ErrInvalid, len(p), maxPathLen)
	case !utf8.ValidString(p):
		return fmt.Errorf("%w: path %q is not UTF-8", format.ErrInvalid, p)
	}
	for i := range len(p) {
		if c := p[i]; c < 0x20 || c == 0x7f {
			return fmt.Errorf("%w: path %q has the control byte %#02x at offset %d", format.ErrInvalid, p, c, i)
		}
	}
	for c := range strings.SplitSeq(p, "/") {
		switch c {
		case "":
			return fmt.Errorf("%w: path %q has an empty component", format.ErrInvalid, p)
		case ".", "..":
			return fmt.Errorf("%w: path %q has a %q component", format.ErrInvalid, p, c)
		}
	}
	return nil
}

// escapePath returns the package path of a raw path (docs/spec/manifest-profile.md §4.1): each
// component escaped with profile.EscapeComponentMax, which hashes as well a component whose
// escaped form is longer than maxComponentLen bytes, joined by profile.PackagePath. p must pass
// checkPath.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, c := range parts {
		parts[i] = profile.EscapeComponentMax(c, maxComponentLen)
	}
	return profile.PackagePath(parts...)
}

// EscapeName returns the package path of the raw path name: the path rules of checkPath, then
// escapePath. Errors wrap format.ErrInvalid. The length of the OCI repository name is the
// registry's to bound, with its instance prefix, so ingest checks it before a write
// (provider.Capabilities.CheckName): a 400 that names the limit.
func (Plugin) EscapeName(name string) (string, error) {
	if err := checkPath(name); err != nil {
		return "", err
	}
	return escapePath(name), nil
}

// repoPath returns the package path of p and the OCI repository raw/<repo>/<package path>
// without the instance prefix. A path that breaks the rules of checkPath wraps
// format.ErrInvalid.
func repoPath(repo index.Repo, p string) (pkgPath, name string, err error) {
	if pkgPath, err = (Plugin{}).EscapeName(p); err != nil {
		return "", "", err
	}
	return pkgPath, profile.RepositoryPath("", Name, repo.Name, pkgPath), nil
}

// Build makes the manifest of one uploaded file: one file layer titled with the path and the
// metadata layer {"path", "contentType", "size", "sha256"}. The path is in.Params[ParamPath],
// checked again here; in.Files must hold exactly one file, whose ContentType (or
// application/octet-stream) goes into the metadata. The version is the first 16 hex digits of
// the sha256 of the bytes, the layer digest. A file streamed into the registry (no Open, ADR
// 0014) is described by its sha256 checksum and its size and left out of Built.Blobs: ingest
// commits its upload session. A file with Open is read once. created is now, in whole seconds:
// the pipeline's clock does not reach Build, and the idempotent repeat of ingest ignores it.
// Errors wrap format.ErrInvalid, except a failure to read the file.
func (Plugin) Build(_ context.Context, repo index.Repo, in format.Upload) (*format.Built, error) {
	p := in.Params[ParamPath]
	_, name, err := repoPath(repo, p)
	if err != nil {
		return nil, err
	}
	if len(in.Files) != 1 {
		return nil, fmt.Errorf("%w: a raw upload is one file, got %d", format.ErrInvalid, len(in.Files))
	}
	f := in.Files[0]
	d, size, err := digestFile(f)
	if err != nil {
		return nil, err
	}
	blobs := []format.BlobSource{bytesBlob(profile.EmptyConfig, ocispec.DescriptorEmptyJSON.Data)}
	contentType := f.ContentType
	if contentType == "" {
		contentType = defaultContentType
	}
	md, err := json.Marshal(metadata{Path: p, ContentType: contentType, Size: size, SHA256: d.Encoded()})
	if err != nil {
		return nil, fmt.Errorf("raw: metadata of %q: %w", p, err)
	}
	version := d.Encoded()[:versionLen]
	attrs := map[string]string{profile.AnnotationRawPath: p}
	file := profile.Layer{MediaType: profile.MediaTypeRawFile, Digest: d, Size: size, Title: p}
	m, err := profile.Build(profile.Annotations{
		Format: Name, Repo: repo.Name, Name: p, Version: version,
		Created: time.Now().UTC().Truncate(time.Second), UploadedBy: in.Params[ParamUploadedBy], Attrs: attrs,
	}, []profile.Layer{file}, &profile.Layer{Digest: digest.FromBytes(md), Size: int64(len(md))})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	if f.Open != nil {
		blobs = append(blobs, format.BlobSource{Descriptor: m.Layers[0], Open: func(context.Context) (io.ReadCloser, error) { return f.Open() }})
	}
	return &format.Built{
		Manifest: m,
		Blobs:    append(blobs, bytesBlob(m.Layers[1], md)),
		Entry: format.Entry{Name: p, Version: version, Attrs: attrs, Metadata: md,
			Files: []format.FileEntry{{Name: p, MediaType: file.MediaType, Digest: d, Size: size}}},
		Tag:      profile.Tag(version, ""),
		RepoPath: name,
	}, nil
}

// digestFile returns the sha256 digest and the size of the bytes of f: of a streamed file (no
// Open), its sha256 checksum and its size; of another, what reading it once gives. A size other
// than the one f declares, and a streamed file without a sha256 or a size, wrap format.ErrInvalid.
func digestFile(f format.UploadedFile) (digest.Digest, int64, error) {
	if f.Open == nil {
		d := digest.NewDigestFromEncoded(digest.SHA256, f.Checksums["sha256"])
		if err := d.Validate(); err != nil || f.Size < 0 {
			return "", 0, fmt.Errorf("%w: %q has neither Open nor its sha256 and size", format.ErrInvalid, f.Name)
		}
		return d, f.Size, nil
	}
	rc, err := f.Open()
	if err != nil {
		return "", 0, fmt.Errorf("raw: open %q: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	h := digest.SHA256.Digester()
	n, err := io.Copy(h.Hash(), rc)
	switch {
	case err != nil:
		return "", 0, fmt.Errorf("raw: read %q: %w", f.Name, err)
	case f.Size >= 0 && n != f.Size:
		return "", 0, fmt.Errorf("%w: %q has %d bytes, the upload says %d", format.ErrInvalid, f.Name, n, f.Size)
	}
	return h.Digest(), n, nil
}

// bytesBlob is a BlobSource of bytes in memory with the media type, digest and size of desc.
func bytesBlob(desc ocispec.Descriptor, b []byte) format.BlobSource {
	return format.BlobSource{
		Descriptor: ocispec.Descriptor{MediaType: desc.MediaType, Digest: desc.Digest, Size: desc.Size},
		Open:       func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil },
	}
}

// Validate checks a raw manifest (an oras push through the facade, or the output of Build)
// beyond the profile: exactly one file layer, whose title is lajstrom.name and
// lajstrom.raw.path, a path that passes the rules and fits the OCI name limit, no variant, a
// sha256 layer digest whose first 16 hex digits are lajstrom.version, and a metadata layer, when
// there is one, that names the same path, the layer's size and its sha256 (contentType may be
// any string). It returns the entry with the metadata bytes, nil when the manifest has no
// metadata layer. Errors wrap format.ErrInvalid, except a failure to fetch the metadata layer.
//
// The rule that a raw path is unique without regard to case does not run here: Validate gets a
// BlobFetcher, not the index. On the facade path ingest refuses the second spelling under its
// claim on the package path, before the manifest is pushed (409, #244). The PUT route checks
// before the body is read.
func (Plugin) Validate(ctx context.Context, repo index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
	parsed, err := profile.Validate(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	a := parsed.Annotations
	if len(parsed.Files) != 1 {
		return nil, fmt.Errorf("%w: a raw manifest has one file layer, got %d", format.ErrInvalid, len(parsed.Files))
	}
	f := parsed.Files[0]
	if err := checkAnnotations(repo, a, f); err != nil {
		return nil, err
	}
	var md json.RawMessage
	if parsed.Metadata != nil {
		if md, err = readMetadata(ctx, fetch, *parsed.Metadata, a.Name, f); err != nil {
			return nil, err
		}
	}
	return &format.Entry{Name: a.Name, Version: a.Version, Attrs: a.Attrs, Metadata: md,
		Files: []format.FileEntry{{Name: f.Title, MediaType: f.MediaType, Digest: f.Digest, Size: f.Size, Attrs: f.Annotations}}}, nil
}

// checkAnnotations compares the annotations of a raw manifest with its file layer f.
func checkAnnotations(repo index.Repo, a profile.Annotations, f profile.Layer) error {
	if _, _, err := repoPath(repo, a.Name); err != nil {
		return err
	}
	switch rawPath := a.Attrs[profile.AnnotationRawPath]; {
	case a.Variant != "":
		return fmt.Errorf("%w: a raw manifest has no %s, got %q", format.ErrInvalid, profile.AnnotationVariant, a.Variant)
	case rawPath != a.Name:
		return fmt.Errorf("%w: %s %q is not %s %q", format.ErrInvalid, profile.AnnotationRawPath, rawPath, profile.AnnotationName, a.Name)
	case f.Title != a.Name:
		return fmt.Errorf("%w: the file layer is titled %q, not %s %q", format.ErrInvalid, f.Title, profile.AnnotationName, a.Name)
	case f.Digest.Algorithm() != digest.SHA256:
		return fmt.Errorf("%w: the file layer digest %s is not sha256, which the version is made of", format.ErrInvalid, f.Digest)
	case a.Version != f.Digest.Encoded()[:versionLen]:
		return fmt.Errorf("%w: %s %q is not the first %d hex digits of the file's sha256 %s",
			format.ErrInvalid, profile.AnnotationVersion, a.Version, versionLen, f.Digest.Encoded())
	}
	return nil
}

// readMetadata fetches the metadata layer l and checks that it names path p and the size and
// sha256 of the file layer f. It returns the bytes as they are in the registry.
func readMetadata(ctx context.Context, fetch format.BlobFetcher, l profile.Layer, p string, f profile.Layer) (json.RawMessage, error) {
	if l.Size > maxMetadata {
		return nil, fmt.Errorf("%w: the metadata layer has %d bytes, at most %d", format.ErrInvalid, l.Size, maxMetadata)
	}
	if fetch == nil {
		return nil, fmt.Errorf("raw: no blob fetcher for the metadata layer %s", l.Digest)
	}
	rc, err := fetch(ctx, l.Digest)
	if err != nil {
		return nil, fmt.Errorf("raw: fetch the metadata layer %s: %w", l.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, maxMetadata+1))
	if err != nil {
		return nil, fmt.Errorf("raw: read the metadata layer %s: %w", l.Digest, err)
	}
	var md struct {
		Path        *string `json:"path"`
		ContentType *string `json:"contentType"`
		Size        *int64  `json:"size"`
		SHA256      *string `json:"sha256"`
	}
	if err := json.Unmarshal(b, &md); err != nil {
		return nil, fmt.Errorf("%w: the metadata layer is not the raw metadata object: %w", format.ErrInvalid, err)
	}
	switch {
	case md.Path == nil || *md.Path != p:
		return nil, fmt.Errorf("%w: the metadata path is not %q", format.ErrInvalid, p)
	case md.Size == nil || *md.Size != f.Size:
		return nil, fmt.Errorf("%w: the metadata size is not the file layer's %d", format.ErrInvalid, f.Size)
	case md.SHA256 == nil || *md.SHA256 != f.Digest.Encoded():
		return nil, fmt.Errorf("%w: the metadata sha256 is not the file layer's %s", format.ErrInvalid, f.Digest.Encoded())
	}
	return b, nil
}

// contentTypeOf returns the contentType of a version's metadata (the metadata layer as ingest
// stored it), or application/octet-stream when there is none.
func contentTypeOf(md json.RawMessage) string {
	var m struct {
		ContentType string `json:"contentType"`
	}
	if len(md) == 0 || json.Unmarshal(md, &m) != nil || m.ContentType == "" {
		return defaultContentType
	}
	return m.ContentType
}

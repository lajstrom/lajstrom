package terraform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// Build makes the manifest of one published module archive. The package name, <name>/<system>,
// is in.Params[ParamName], the version in.Params[ParamVersion]. in.Files holds exactly one file:
// one streamed into the registry (no Open, ADR 0014) must have passed the inspector of Inspect
// and is described by its size and sha256 and left out of Built.Blobs, for ingest commits its
// upload session; another is read once by ReadArchive and hashed. The manifest has the file
// layer titled <name>-<system>-<version>.tar.gz, no metadata layer, and the annotations name and
// version; the tag is profile.Tag(version, ""). created is now, in whole seconds. Errors wrap
// format.ErrInvalid, except a failure to read the file.
func (Plugin) Build(_ context.Context, repo index.Repo, in format.Upload) (*format.Built, error) {
	if len(in.Files) != 1 {
		return nil, fmt.Errorf("%w: a terraform publish is one archive, got %d files", format.ErrInvalid, len(in.Files))
	}
	m, err := ParsePackageName(in.Params[ParamName])
	if err != nil {
		return nil, err
	}
	version := in.Params[ParamVersion]
	if err := CheckVersion(version); err != nil {
		return nil, err
	}
	f := in.Files[0]
	d, size, err := archiveDigest(f)
	if err != nil {
		return nil, err
	}
	layer := profile.Layer{MediaType: profile.MediaTypeTerraformModule, Digest: d, Size: size, Title: m.Filename(version)}
	man, err := profile.Build(profile.Annotations{
		Format: Name, Repo: repo.Name, Name: m.PackageName(), Version: version,
		Created: time.Now().UTC().Truncate(time.Second), UploadedBy: in.Params[ParamUploadedBy],
	}, []profile.Layer{layer}, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
	}
	blobs := []format.BlobSource{{
		Descriptor: ocispec.Descriptor{MediaType: profile.EmptyConfig.MediaType, Digest: profile.EmptyConfig.Digest, Size: profile.EmptyConfig.Size},
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(ocispec.DescriptorEmptyJSON.Data)), nil
		},
	}}
	if f.Open != nil {
		blobs = append(blobs, format.BlobSource{Descriptor: man.Layers[0], Open: func(context.Context) (io.ReadCloser, error) { return f.Open() }})
	}
	return &format.Built{
		Manifest: man,
		Blobs:    blobs,
		Entry: format.Entry{Name: m.PackageName(), Version: version,
			Files: []format.FileEntry{{Name: layer.Title, MediaType: layer.MediaType, Digest: d, Size: size}}},
		Tag:      profile.Tag(version, ""),
		RepoPath: profile.RepositoryPath("", Name, repo.Name, m.packagePath()),
	}, nil
}

// archiveDigest returns the sha256 digest and the size of the archive f: of a streamed file,
// checkStreamed and what ingest computed; of another, ReadArchive reads it once and the bytes
// are hashed as they pass. A size other than the one f declares wraps format.ErrInvalid.
func archiveDigest(f format.UploadedFile) (digest.Digest, int64, error) {
	if f.Open == nil {
		if err := checkStreamed(f); err != nil {
			return "", 0, err
		}
		return digest.NewDigestFromEncoded(digest.SHA256, f.Checksums["sha256"]), f.Size, nil
	}
	rc, err := f.Open()
	if err != nil {
		return "", 0, fmt.Errorf("terraform: open %q: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	c := &counter{r: io.TeeReader(rc, h)}
	if err := ReadArchive(c); err != nil {
		return "", 0, fmt.Errorf("terraform: %q: %w", f.Name, err)
	}
	if _, err := io.Copy(io.Discard, c); err != nil { // the bytes after the gzip stream, which ReadArchive refuses
		return "", 0, fmt.Errorf("terraform: read %q: %w", f.Name, err)
	}
	if f.Size >= 0 && c.n != f.Size {
		return "", 0, fmt.Errorf("%w: %q has %d bytes, the upload says %d", format.ErrInvalid, f.Name, c.n, f.Size)
	}
	return digest.NewDigestFromBytes(digest.SHA256, h.Sum(nil)), c.n, nil
}

// counter counts the bytes read through it.
type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

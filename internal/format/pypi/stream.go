package pypi

import (
	"fmt"
	"io"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

var _ format.Streamer = Plugin{}

// Inspect implements format.Streamer: the inspector of the distribution that up uploads, which
// finds its core metadata as the bytes stream (ADR 0014). A .tar.gz sdist streams; a wheel and a
// .zip sdist, whose directory is at their end, do not yet (ErrUnsupported). The project of the
// file name must be up.Name.
func (Plugin) Inspect(_ index.Repo, up format.StreamUpload) (format.Inspection, error) {
	d, err := describe(up.File.Name, "")
	if err != nil {
		return format.Inspection{}, err
	}
	if Normalize(d.name) != Normalize(up.Name) {
		return format.Inspection{}, fmt.Errorf("%w: %s is a file of the project %q, not %q", format.ErrInvalid, up.File.Name, d.name, up.Name)
	}
	if d.mediaType != profile.MediaTypePypiSdistTarGz {
		return format.Inspection{}, fmt.Errorf("pypi: %s does not stream yet: %w", up.File.Name, format.ErrUnsupported)
	}
	s, err := ParseSdistFilename(up.File.Name)
	if err != nil {
		return format.Inspection{}, err
	}
	return format.Inspection{Inspector: newSdistInspector(up.File.Name, s)}, nil
}

// streams reports whether the distribution d streams into the registry (Inspect);
// the others are spooled for the request and go through format.Publisher.Upload.
func streams(d dist) bool { return d.mediaType == profile.MediaTypePypiSdistTarGz }

// coreInspector is the inspector of a distribution that streams into the registry: it finds the
// core metadata inside it, which Build takes from it once it is closed.
type coreInspector struct {
	format.Inspector
	core []byte // METADATA or PKG-INFO, once the inspector is closed
}

// newSdistInspector reads the PKG-INFO of the .tar.gz sdist s, the file filename, as its bytes
// stream, as readArchive reads it from the registry: at most maxSdistScan bytes of it.
func newSdistInspector(filename string, s Sdist) *coreInspector {
	c := &coreInspector{}
	c.Inspector = format.ReadInspector(func(r io.Reader) error {
		b, err := ReadSdistPKGInfo(io.LimitReader(r, maxSdistScan), -1, s)
		if err != nil {
			return fmt.Errorf("%s: %w", filename, err)
		}
		c.core = b
		return nil
	})
	return c
}

// streamedUpload returns the sha256, the size and the core metadata of a distribution that
// streamed into the registry: what ingest computed, and what its inspector found, which
// Validate takes as checked (format.ErrStreamed).
func streamedUpload(f format.UploadedFile) (digest.Digest, int64, []byte, error) {
	c, ok := f.Inspector.(*coreInspector)
	sum := digest.NewDigestFromEncoded(digest.SHA256, f.Checksums["sha256"])
	if !ok || c.core == nil || sum.Validate() != nil {
		return "", 0, nil, fmt.Errorf("pypi: %s streamed without the pypi inspector, its core metadata or its sha256", f.Name)
	}
	return sum, f.Size, c.core, nil
}

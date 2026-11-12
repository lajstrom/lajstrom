package pypi

import (
	"errors"
	"fmt"
	"io"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

var _ format.Streamer = Plugin{}

// Inspect implements format.Streamer: the inspector of the distribution that up uploads, which
// finds its core metadata as the bytes stream (ADR 0014): that of a .tar.gz sdist reads it as
// the tar passes, that of a wheel or a .zip sdist holds the end of the zip and the entries
// that may be it (zipInspector). The project of the file name must be up.Name.
func (Plugin) Inspect(_ index.Repo, up format.StreamUpload) (format.Inspection, error) {
	d, err := describe(up.File.Name, "")
	if err != nil {
		return format.Inspection{}, err
	}
	if Normalize(d.name) != Normalize(up.Name) {
		return format.Inspection{}, fmt.Errorf("%w: %s is a file of the project %q, not %q", format.ErrInvalid, up.File.Name, d.name, up.Name)
	}
	if d.packagetype == PackagetypeWheel {
		return format.Inspection{Inspector: newZipInspector(up.File.Name, isWheelMetadata, ReadWheelMetadata)}, nil
	}
	s, err := ParseSdistFilename(up.File.Name)
	if err != nil {
		return format.Inspection{}, err
	}
	if s.Kind == SdistZip {
		want := s.Root() + "/PKG-INFO"
		return format.Inspection{Inspector: newZipInspector(up.File.Name, func(name []byte) bool { return string(name) == want },
			func(r io.ReaderAt, size int64) ([]byte, error) {
				return ReadSdistPKGInfo(io.NewSectionReader(r, 0, size), size, s)
			})}, nil
	}
	return format.Inspection{Inspector: newSdistInspector(up.File.Name, s)}, nil
}

// inspector is the inspector of a distribution as Build reads it.
type inspector interface {
	format.Inspector
	// coreMetadata returns the core metadata inside the distribution, once the inspector is
	// closed. What it needs of bytes it did not hold it reads from back, the file read back from
	// the registry (format.ErrReadBack); without back, that is format.ErrReadBack.
	coreMetadata(back io.ReaderAt) ([]byte, error)
}

// sdistInspector is the inspector of a .tar.gz sdist.
type sdistInspector struct {
	format.Inspector
	core []byte // PKG-INFO, once the inspector is closed
}

// newSdistInspector reads the PKG-INFO of the .tar.gz sdist s, the file filename, as its bytes
// stream, as readArchive reads it from the registry: at most maxSdistScan bytes of it.
func newSdistInspector(filename string, s Sdist) *sdistInspector {
	c := &sdistInspector{}
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

// coreMetadata implements inspector: a .tar.gz is read in one pass.
func (c *sdistInspector) coreMetadata(io.ReaderAt) ([]byte, error) {
	if c.core == nil {
		return nil, errors.New("pypi: the inspector of the sdist found no PKG-INFO")
	}
	return c.core, nil
}

// streamedUpload returns the sha256, the size and the core metadata of a distribution that
// streamed into the registry: what ingest computed, and what its inspector found, with the file
// that Open reads back from the registry when the inspector needs it. Validate takes it as
// checked (format.ErrStreamed), but after a read-back, whose file it reads from the registry.
func streamedUpload(f format.UploadedFile) (digest.Digest, int64, []byte, error) {
	insp, ok := f.Inspector.(inspector)
	sum := digest.NewDigestFromEncoded(digest.SHA256, f.Checksums["sha256"])
	if !ok || sum.Validate() != nil {
		return "", 0, nil, fmt.Errorf("pypi: %s streamed without the pypi inspector or its sha256", f.Name)
	}
	var back io.ReaderAt
	if f.Open != nil {
		rc, err := f.Open()
		if err != nil {
			return "", 0, nil, fmt.Errorf("pypi: read %s back: %w", f.Name, err)
		}
		defer func() { _ = rc.Close() }()
		if back, ok = rc.(io.ReaderAt); !ok {
			return "", 0, nil, fmt.Errorf("pypi: %s, read back, has no ReadAt", f.Name)
		}
	}
	core, err := insp.coreMetadata(back)
	return sum, f.Size, core, err
}

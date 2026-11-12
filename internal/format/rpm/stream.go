package rpm

import (
	"fmt"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// An upload streams into the registry (ADR 0014). The OCI repository of an rpm follows from the
// NAME in its header, so the upload route reads the head first; then the whole body, the head
// again at its start, passes the inspector on its way into the registry, which checks the file
// as Parse does.

var _ format.Streamer = Plugin{}

// Inspect implements format.Streamer: Build needs the sha256 of the file, which ingest computes
// anyway, and the head, which the inspector reads.
func (Plugin) Inspect(index.Repo, format.StreamUpload) (format.Inspection, error) {
	return format.Inspection{Inspector: &inspector{}}, nil
}

// streamedPackage returns what the inspector of the streamed file f read, with the size and the
// sha256 that ingest computed. A file that did not pass the rpm inspector to its end, or lacks
// a size or its sha256, wraps format.ErrInvalid.
func streamedPackage(f format.UploadedFile) (*Package, error) {
	in, ok := f.Inspector.(*inspector)
	if !ok || !in.done {
		return nil, fmt.Errorf("%w: %q was streamed without the inspector of the rpm plugin, or not to its end", format.ErrInvalid, f.Name)
	}
	pkg := *in.pkg
	pkg.Size, pkg.SHA256 = f.Size, f.Checksums["sha256"]
	if pkg.Size < 0 || pkg.SHA256 == "" {
		return nil, fmt.Errorf("%w: %q was streamed without its size and sha256", format.ErrInvalid, f.Name)
	}
	return &pkg, nil
}

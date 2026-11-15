package terraform

import (
	"fmt"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// A publish streams into the registry (ADR 0014): the module and the version are in its path,
// so the frontend knows the OCI repository before the first byte, and the archive passes the
// inspector on its way, which reads it as ReadArchive does.

var _ format.Streamer = Plugin{}

// Inspect implements format.Streamer: Build needs the sha256 of the archive, which ingest
// computes anyway, and that the archive passed ReadArchive, which the inspector runs.
func (Plugin) Inspect(index.Repo, format.StreamUpload) (format.Inspection, error) {
	return format.Inspection{Inspector: &inspector{Inspector: format.ReadInspector(ReadArchive)}}, nil
}

// inspector runs ReadArchive over the bytes of a publish and remembers whether it passed.
type inspector struct {
	format.Inspector
	passed bool
}

// Close implements format.Inspector.
func (in *inspector) Close() error {
	err := in.Inspector.Close()
	in.passed = err == nil
	return err
}

// checkStreamed checks that the file f streamed through the inspector of the plugin to its end
// and that it passed, and that ingest gave it a size and a sha256; errors wrap
// format.ErrInvalid.
func checkStreamed(f format.UploadedFile) error {
	in, ok := f.Inspector.(*inspector)
	switch {
	case !ok || !in.passed:
		return fmt.Errorf("%w: %q was streamed without the inspector of the terraform plugin, or not to its end", format.ErrInvalid, f.Name)
	case f.Size < 0 || f.Checksums["sha256"] == "":
		return fmt.Errorf("%w: %q was streamed without its size and sha256", format.ErrInvalid, f.Name)
	}
	return nil
}

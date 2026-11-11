package raw

import (
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// Name is the plugin name: the first URL segment and lajstrom.format.
const Name = "raw"

// Plugin is the raw format plugin. It is stateless; its routes get their services through
// format.Deps.
type Plugin struct{}

var _ format.Streamer = Plugin{}

func init() { format.Register(Plugin{}) }

// Name returns "raw".
func (Plugin) Name() string { return Name }

// ArtifactType returns application/vnd.lajstrom.raw.v1.
func (Plugin) ArtifactType() string { return profile.ArtifactType(Name) }

// Inspect implements format.Streamer: Build needs nothing of a raw file's bytes but their
// sha256, which ingest always computes, so there is no inspector and no other checksum.
func (Plugin) Inspect(index.Repo, format.StreamUpload) (format.Inspection, error) {
	return format.Inspection{}, nil
}

package rpm

import (
	"fmt"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

const (
	// Name is the plugin name: the repo format, the first URL segment and lajstrom.format.
	Name = "rpm"
	// Type is the artifact type of the manifests.
	Type = "rpm"
	// ParamUploadedBy is the principal of an upload, for lajstrom.uploaded-by.
	ParamUploadedBy = "uploaded-by"
)

// Plugin is the rpm format plugin. It is stateless: Build, Validate and EscapeName need no
// services.
type Plugin struct{}

var _ format.Plugin = Plugin{}

func init() { format.Register(Plugin{}) }

// Name returns "rpm".
func (Plugin) Name() string { return Name }

// ArtifactType returns application/vnd.lajstrom.rpm.v1.
func (Plugin) ArtifactType() string { return profile.ArtifactType(Type) }

// EscapeName returns the package path of a package name, profile.EscapeComponent of it
// (docs/spec/manifest-profile.md §4.1): libstdc++ → libstdc_2b_2b. A name outside the grammar of
// an rpm NAME wraps format.ErrInvalid.
func (Plugin) EscapeName(name string) (string, error) {
	if !nameGrammar.MatchString(name) {
		return "", fmt.Errorf("%w: %q is not an rpm package name", format.ErrInvalid, name)
	}
	return profile.EscapeComponent(name), nil
}

package deb

import (
	"context"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

const (
	// Name is the plugin name: the repo format, the first URL segment and lajstrom.format.
	Name = "apt"
	// Type is the artifact type of the manifests: the apt protocol stores deb packages
	// (docs/spec/manifest-profile.md §1).
	Type = "deb"
)

// Upload parameters the routes pass to Build in format.Upload.Params.
const (
	// ParamSuite is the apt suite of the upload (jammy, stable); required.
	ParamSuite = "suite"
	// ParamComponent is the apt component of the upload (main, contrib); required.
	ParamComponent = "component"
	// ParamArch is the architecture the client says the package has; optional, and when given
	// it must be the Architecture of the control file.
	ParamArch = "arch"
	// ParamUploadedBy is the principal of the upload, for lajstrom.uploaded-by.
	ParamUploadedBy = "uploaded-by"
)

// Plugin is the apt format plugin. It is stateless; its routes get their services through
// format.Deps.
type Plugin struct{}

var _ format.Plugin = Plugin{}

func init() { format.Register(Plugin{}) }

// Name returns "apt".
func (Plugin) Name() string { return Name }

// ArtifactType returns application/vnd.lajstrom.deb.v1.
func (Plugin) ArtifactType() string { return profile.ArtifactType(Type) }

// EscapeName returns the package path of a package name: profile.EscapeComponent of the name
// (docs/spec/manifest-profile.md §4.1), libstdc++6 → libstdc_2b_2b6. A name that is not a
// Debian package name (Debian Policy §5.6.7) wraps format.ErrInvalid.
func (Plugin) EscapeName(name string) (string, error) {
	if err := checkName("name", name); err != nil {
		return "", err
	}
	return profile.EscapeComponent(name), nil
}

// Routes mounts nothing yet: the dists/ routes come with the index rendering (#40), the pool/
// and upload routes with #42. Until then a package reaches an apt repo by oras push through the
// facade (#25), whose hook runs Validate.
func (Plugin) Routes(format.Router, format.Deps) {}

// RenderIndex renders nothing yet: Packages and Release are #40.
func (Plugin) RenderIndex(_ context.Context, _ index.Repo, scope format.IndexScope, _ index.Reader) ([]format.Document, error) {
	return nil, fmt.Errorf("%w: apt index %q: Packages and Release come with #40", format.ErrUnsupported, scope)
}

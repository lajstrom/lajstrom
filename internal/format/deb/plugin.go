package deb

import (
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
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

// Plugin is the apt format plugin. The value format.Register holds has no services: Build,
// Validate and EscapeName need none. Routes makes the value its handlers render with, which
// keeps the providers of format.Deps, so that RenderIndex can read from the registry a metadata
// layer the index left out (versions.metadata is nil). A Plugin is comparable; it holds a
// pointer, not the services themselves.
type Plugin struct {
	svc *services
}

// services are what RenderIndex needs beyond the index.
type services struct {
	// providers resolves repo.Registry, for the metadata layers the index does not hold.
	providers provider.Set
	// now is the clock of the Date of a Release and of the SignedAt of its signatures; nil is
	// time.Now.
	now func() time.Time
}

// clock returns the time for the Date of a Release and the SignedAt of its signatures.
func (p Plugin) clock() time.Time {
	if p.svc != nil && p.svc.now != nil {
		return p.svc.now()
	}
	return time.Now()
}

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

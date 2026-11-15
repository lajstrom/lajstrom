package terraform

import (
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

const (
	// Name is the plugin name: the repo format, the first URL segment and lajstrom.format.
	Name = "terraform"
	// Type is the artifact type of the manifests.
	Type = "terraform"
	// ParamName is the package name of a publish, <name>/<system>, from its path.
	ParamName = "name"
	// ParamVersion is the version of a publish, from its path.
	ParamVersion = "version"
	// ParamUploadedBy is the principal of a publish, for lajstrom.uploaded-by.
	ParamUploadedBy = "uploaded-by"
)

// Plugin is the terraform format plugin. It is stateless; its routes get their services
// through format.Deps.
type Plugin struct{}

var _ format.Plugin = Plugin{}

func init() { format.Register(Plugin{}) }

// Name returns "terraform".
func (Plugin) Name() string { return Name }

// ArtifactType returns application/vnd.lajstrom.terraform.v1.
func (Plugin) ArtifactType() string { return profile.ArtifactType(Type) }

// EscapeName returns the package path of a package name, <name>/<system>
// (docs/spec/manifest-profile.md §4.1): PackagePath(EscapeComponent(name),
// EscapeComponent(system)), My_Network/aws → my_5fnetwork/aws. A name outside the grammar of a
// module address wraps format.ErrInvalid.
func (Plugin) EscapeName(name string) (string, error) {
	m, err := ParsePackageName(name)
	if err != nil {
		return "", err
	}
	return m.packagePath(), nil
}

// packagePath returns the package path of the module.
func (m Module) packagePath() string {
	return profile.PackagePath(profile.EscapeComponent(m.Name), profile.EscapeComponent(m.System))
}

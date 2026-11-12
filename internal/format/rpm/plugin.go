package rpm

import (
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

const (
	// Name is the plugin name: the repo format, the first URL segment and lajstrom.format.
	Name = "rpm"
	// Type is the artifact type of the manifests.
	Type = "rpm"
	// ParamUploadedBy is the principal of an upload, for lajstrom.uploaded-by.
	ParamUploadedBy = "uploaded-by"
)

// Plugin is the rpm format plugin. The value format.Register holds has no services: Build,
// Validate and EscapeName need none. Routes makes the value its handlers render with, which
// keeps the providers of format.Deps, so that RenderIndex can read from the registry a metadata
// layer the index left out. A Plugin is comparable; it holds a pointer, not the services.
type Plugin struct {
	svc *services
}

// services are what the handlers and RenderIndex need beyond the index.
type services struct {
	// providers resolves repo.Registry, for the metadata layers the index does not hold.
	providers provider.Set
	// now is the clock of the revision of a repomd.xml and of the SignedAt of its signature;
	// nil is time.Now.
	now func() time.Time
}

// clock returns the time for a render and for a signature.
func (p Plugin) clock() time.Time {
	if p.svc != nil && p.svc.now != nil {
		return p.svc.now()
	}
	return time.Now()
}

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

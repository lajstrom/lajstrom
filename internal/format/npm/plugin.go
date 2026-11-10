package npm

import (
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// Name is the plugin name: the repo format, the first URL segment, lajstrom.format and the type
// of the artifactType.
const Name = "npm"

// The parameters of an Upload that Build reads.
const (
	// ParamUploadedBy is the principal of the upload, for lajstrom.uploaded-by; optional.
	ParamUploadedBy = "uploaded-by"
	// ParamDocument is the version document of a publish, the JSON object npm sends in the
	// versions of its body; without it, the tarball's package.json is the document.
	ParamDocument = "document"
	// ParamReadme is the readme of the body's top level, which yarn sends; the document's own
	// readme comes first.
	ParamReadme = "readme"
)

// maxMetadata is the largest metadata layer Build writes and Validate reads (docs/spec/
// manifest-profile.md §2, npm).
const maxMetadata = 2 << 20

// readmePlaceholder is the readme npm sends for a package without one; it is not kept.
const readmePlaceholder = "ERROR: No README data found!"

// Plugin is the npm format plugin: Build and Validate of a version (#304), the publish route
// (#305), the packuments and the tarballs (#306). It registers itself with format.Register when
// the package is imported, without an ExternalURL; serve mounts the routes of the plugin
// WithExternalURL returns.
type Plugin struct {
	// ExternalURL is server.externalURL, the base of the absolute dist.tarball of a packument.
	// Empty, the routes render a packument for each request with the base of its host, and do
	// not keep it (the dev stack without server.externalURL).
	ExternalURL string
	// Accounts checks the password of npm login and makes and revokes its tokens; nil, npm
	// login is 501 and npm logout 404.
	Accounts Accounts
}

// WithExternalURL returns the plugin with ExternalURL u: serve gives every plugin that has the
// method server.externalURL.
func (p Plugin) WithExternalURL(u string) format.Plugin {
	p.ExternalURL = u
	return p
}

var _ format.Plugin = Plugin{}

func init() { format.Register(Plugin{}) }

// Name returns "npm".
func (Plugin) Name() string { return Name }

// ArtifactType returns application/vnd.lajstrom.npm.v1.
func (Plugin) ArtifactType() string { return profile.ArtifactType(Name) }

// EscapeName returns the package path of a package name (docs/spec/manifest-profile.md §4.1):
// lodash → lodash, @acme/widgets → acme/widgets, JSONStream → jsonstream. A name CheckName
// refuses wraps format.ErrInvalid.
func (Plugin) EscapeName(name string) (string, error) {
	if err := CheckName(name); err != nil {
		return "", err
	}
	return packagePath(name), nil
}

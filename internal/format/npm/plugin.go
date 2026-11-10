package npm

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

// Plugin is the npm format plugin. Build makes the manifest of a version (#304); Validate, the
// routes and the registration come next.
type Plugin struct{}

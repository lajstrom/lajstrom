package maven

import (
	"fmt"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// Name is the plugin name: the repo format, the first URL segment, lajstrom.format and the type
// of the artifactType.
const Name = "maven"

// ParamUploadedBy is the upload parameter with the principal of the upload, for
// lajstrom.uploaded-by; optional.
const ParamUploadedBy = "uploaded-by"

const (
	// maxMetadata is the largest metadata layer Build writes and Validate reads: a file entry
	// is a few hundred bytes, a dependency less.
	maxMetadata = 4 << 20
	// maxRepositoryLen is the longest OCI repository name registries accept.
	maxRepositoryLen = 255
)

// Plugin is the maven format plugin: paths, POMs, Build and Validate (#52), deploy through
// publication sessions (#54), resolve and maven-metadata.xml (#55). It registers itself with
// format.Register when the package is imported; cmd/lajstrom compiles it in.
type Plugin struct{}

func init() { format.Register(Plugin{}) }

var _ format.Plugin = Plugin{}

// Name returns "maven".
func (Plugin) Name() string { return Name }

// ArtifactType returns application/vnd.lajstrom.maven.v1.
func (Plugin) ArtifactType() string { return profile.ArtifactType(Name) }

// EscapeName returns the package path of a package name groupId:artifactId
// (docs/spec/manifest-profile.md §4.1): profile.PackagePath of profile.EscapeComponent of the groupId
// lowercased and of the artifactId; com.example:Lib-Core → com.example/lib-core. A name
// that is not groupId:artifactId wraps format.ErrInvalid.
func (Plugin) EscapeName(name string) (string, error) {
	g, a, ok := strings.Cut(name, ":")
	if !ok {
		return "", fmt.Errorf("%w: %q is not groupId:artifactId", format.ErrInvalid, name)
	}
	if err := checkGroupArtifact(g, a); err != nil {
		return "", fmt.Errorf("%q: %w", name, err)
	}
	return packagePath(g, a), nil
}

// packagePath returns the package path of well-formed coordinates.
func packagePath(groupID, artifactID string) string {
	return profile.PackagePath(profile.EscapeComponent(strings.ToLower(groupID)), profile.EscapeComponent(artifactID))
}

// Routes mounts the maven frontend below /maven/: resolve (resolve.go) and deploy (deploy.go).
func (Plugin) Routes(r format.Router, deps format.Deps) {
	resolveRoutes(r, deps)
	deployRoutes(r, deps)
}

// metadata is the content of the maven metadata layer (docs/spec/manifest-profile.md §2): the
// coordinates and packaging, one entry per file layer, in manifest order, and the essentials of
// the POM. The fields are in the order of their keys, so that the JSON has its keys sorted.
type metadata struct {
	ArtifactID string         `json:"artifactId"`
	Files      []fileMetadata `json:"files"`
	GroupID    string         `json:"groupId"`
	Packaging  string         `json:"packaging"`
	POM        pomEssentials  `json:"pom"`
	Version    string         `json:"version"`
}

// fileMetadata is a file of the metadata layer: its name, what the name says, its size and the
// four checksums of the whole file, which the .md5, .sha1, .sha256 and .sha512 files serve.
// Classifier is "" when the file has none.
type fileMetadata struct {
	Classifier string `json:"classifier"`
	Extension  string `json:"extension"`
	MD5        string `json:"md5"`
	Name       string `json:"name"`
	SHA1       string `json:"sha1"`
	SHA256     string `json:"sha256"`
	SHA512     string `json:"sha512"`
	Size       int64  `json:"size"`
}

// checksum returns the checksum of algorithm.
func (f *fileMetadata) checksum(algorithm string) *string {
	switch algorithm {
	case MD5:
		return &f.MD5
	case SHA1:
		return &f.SHA1
	case SHA256:
		return &f.SHA256
	default:
		return &f.SHA512
	}
}

// checkChecksums checks that each of the four checksums is lowercase hex of its length.
func (f *fileMetadata) checkChecksums() error {
	for _, a := range algorithms {
		if v := *f.checksum(a.name); !isHex(v, a.digits) {
			return fmt.Errorf("%w: the %s of %s is %q, not %d lowercase hex digits", format.ErrInvalid, a.name, f.Name, v, a.digits)
		}
	}
	return nil
}

// pomEssentials is what the metadata layer keeps of the POM: the parent, absent when none, and
// the dependencies, [] when none.
type pomEssentials struct {
	Dependencies []Dependency `json:"dependencies"`
	Parent       *Coordinates `json:"parent,omitempty"`
}

// essentials returns the pomEssentials of a parsed POM.
func (p *POM) essentials() pomEssentials {
	deps := p.Dependencies
	if deps == nil {
		deps = []Dependency{}
	}
	return pomEssentials{Dependencies: deps, Parent: p.Parent}
}

// isHex reports whether s is n lowercase hex digits.
func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Package version holds the build information of the binary. The release build sets the
// variables at link time:
//
//	go build -ldflags "-X github.com/lajstrom/lajstrom/internal/version.Version=v0.1.0 \
//	  -X github.com/lajstrom/lajstrom/internal/version.Commit=28bf6ca \
//	  -X github.com/lajstrom/lajstrom/internal/version.Date=2026-11-06T12:00:00Z" ./cmd/lajstrom
//
// make build does this from git describe, git rev-parse and the current time.
package version

// Build information, set with -ldflags -X. A plain go build leaves the defaults.
var (
	// Version is the release version (git describe), "dev" for an unversioned build.
	Version = "dev"
	// Commit is the short commit hash the binary was built from; empty when unknown.
	Commit = ""
	// Date is the build time in RFC 3339, UTC; empty when unknown.
	Date = ""
)

// Info is the build information as /-/version and lajstrom version --json report it.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// Get returns the build information of the running binary.
func Get() Info {
	return Info{Version: Version, Commit: Commit, Date: Date}
}

package npm

import (
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestDownloadPrefix: the tarball's path, a scoped name's too.
func TestDownloadPrefix(t *testing.T) {
	repo := index.Repo{Format: Name, Name: "npmjs"}
	for name, want := range map[string]string{"left-pad": "/npm/npmjs/left-pad/-/left-pad-1.3.0.tgz",
		"@acme/ui": "/npm/npmjs/@acme/ui/-/ui-1.3.0.tgz"} {
		if got, ok := (Plugin{}).DownloadPrefix(repo, name, "1.3.0"); !ok || got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

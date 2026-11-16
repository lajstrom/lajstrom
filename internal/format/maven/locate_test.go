package maven

import (
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestDownloadPrefix: the version's directory; a name that is not groupId:artifactId has none.
func TestDownloadPrefix(t *testing.T) {
	repo := index.Repo{Format: Name, Name: "releases"}
	if got, ok := (Plugin{}).DownloadPrefix(repo, "com.acme:lib-core", "1.0"); !ok || got != "/maven/releases/com/acme/lib-core/1.0/" {
		t.Errorf("%q %v", got, ok)
	}
	if _, ok := (Plugin{}).DownloadPrefix(repo, "nope", "1.0"); ok {
		t.Error("a name without a group has a prefix")
	}
}

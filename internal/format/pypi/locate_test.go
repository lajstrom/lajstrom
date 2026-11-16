package pypi

import (
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestDownloadPrefix: the files of the version, below its normalized name.
func TestDownloadPrefix(t *testing.T) {
	got, ok := Plugin{}.DownloadPrefix(index.Repo{Format: Name, Name: "proxy-pypi"}, "Requests_Toolbelt", "1.0.0")
	if !ok || got != "/pypi/proxy-pypi/files/requests-toolbelt/1.0.0/" {
		t.Errorf("%q %v", got, ok)
	}
}

package pypi

import (
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

var _ format.Locator = Plugin{}

// DownloadPrefix is /pypi/<repo>/files/<name>/<version>/, where every file of the version is
// served (format.Locator).
func (Plugin) DownloadPrefix(repo index.Repo, name, version string) (string, bool) {
	return "/" + Name + "/" + repo.Name + "/files/" + Normalize(name) + "/" + version + "/", true
}

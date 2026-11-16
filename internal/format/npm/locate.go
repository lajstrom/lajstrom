package npm

import (
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

var _ format.Locator = Plugin{}

// DownloadPrefix is the path of the tarball of the version, /npm/<repo>/<name>/-/<file>
// (format.Locator).
func (Plugin) DownloadPrefix(repo index.Repo, name, version string) (string, bool) {
	return "/" + Name + "/" + repo.Name + "/" + name + "/-/" + Filename(name, version), true
}

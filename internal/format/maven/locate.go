package maven

import (
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

var _ format.Locator = Plugin{}

// DownloadPrefix is the directory of the version of groupId:artifactId,
// /maven/<repo>/<group path>/<artifactId>/<version>/ (format.Locator).
func (Plugin) DownloadPrefix(repo index.Repo, name, version string) (string, bool) {
	group, artifact, ok := strings.Cut(name, ":")
	if !ok || group == "" || artifact == "" {
		return "", false
	}
	return "/" + Name + "/" + repo.Name + "/" + strings.ReplaceAll(group, ".", "/") + "/" + artifact + "/" + version + "/", true
}

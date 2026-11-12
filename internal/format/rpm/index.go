package rpm

import (
	"context"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// RenderIndex renders the repodata of the repo. Until the renderer (#64, the next pull request
// of the stack) it renders nothing: every scope is index.ErrNotFound.
func (Plugin) RenderIndex(_ context.Context, repo index.Repo, scope format.IndexScope, _ index.Reader) ([]format.Document, error) {
	return nil, fmt.Errorf("%w: rpm/%s has no index %q yet", index.ErrNotFound, repo.Name, scope)
}

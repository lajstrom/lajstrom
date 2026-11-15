package terraform

import (
	"context"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// RenderIndex renders nothing yet: the versions document comes with the routes.
func (Plugin) RenderIndex(_ context.Context, repo index.Repo, scope format.IndexScope, _ index.Reader) ([]format.Document, error) {
	return nil, fmt.Errorf("%w: terraform/%s renders no document %q", index.ErrNotFound, repo.Name, scope)
}

package harbor

import (
	"context"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// ListRepositories is not supported yet: the listing of Harbor's REST API comes with the next
// change of #73. Reindex reads the OCI repositories the index records instead, as for type
// generic.
func (p *Provider) ListRepositories(_ context.Context, prefix string) ([]string, error) {
	return nil, fmt.Errorf("harbor: list repositories %s: %w", prefix, provider.ErrUnsupported)
}

// DeleteRepository is not supported yet, as for type generic: the REST API's repository delete
// comes with the next change of #73.
func (p *Provider) DeleteRepository(_ context.Context, name string) error {
	return fmt.Errorf("harbor: delete repository %s: %w", name, provider.ErrUnsupported)
}

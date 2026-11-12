package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// UserGroups returns the groups of the user u that count at now, by name: those of its
// identity's latest OIDC login, until that login's lifetime ends (docs/spec/rbac.md §8). A user
// without an identity, a local user, has none. whoami answers them; RBAC reads the same rows
// into its snapshot.
func UserGroups(ctx context.Context, r index.Reader, u *index.User, now time.Time) ([]string, error) {
	rows, err := r.ListIdentityGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: the groups of %s: %w", UserPrincipal(u), err)
	}
	groups := []string{}
	for _, g := range rows {
		if g.UserID == u.ID && g.ExpiresAt != nil && now.Before(*g.ExpiresAt) {
			groups = append(groups, g.Name)
		}
	}
	return groups, nil
}

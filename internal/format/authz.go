package format

import (
	"context"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Authorize reports whether the request's principal (auth.PrincipalFrom(ctx)) may perform a on
// repo, through deps.Auth: nil when allowed; ErrUnauthenticated when denied and the principal is
// anonymous; ErrForbidden when denied otherwise; the authorizer's own error, unchanged, when it
// cannot decide (WriteError answers 500). A nil deps.Auth allows everything: the dev stack before
// RBAC (#45).
func Authorize(ctx context.Context, deps Deps, a auth.Action, repo index.Repo) error {
	if deps.Auth == nil {
		return nil
	}
	p := auth.PrincipalFrom(ctx)
	ok, err := deps.Auth.Allowed(ctx, p, a, repo.Format, repo.Name)
	switch {
	case err != nil:
		return err
	case ok:
		return nil
	case p.Kind == auth.KindAnonymous:
		return fmt.Errorf("%w: %s on %s/%s", ErrUnauthenticated, a, repo.Format, repo.Name)
	default:
		return fmt.Errorf("%w: %s on %s/%s", ErrForbidden, a, repo.Format, repo.Name)
	}
}

package format

import (
	"context"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Authorize reports whether the request's principal (auth.PrincipalFrom(ctx)) may perform a on
// repo, through deps.Auth: nil when allowed. A denial must not reveal whether the repo exists
// (docs/spec/rbac.md §3): the anonymous principal gets ErrUnauthenticated (401 with a Basic
// challenge, so that the client sends credentials); a principal that can read the repo but
// lacks a gets ErrForbidden (403); one that cannot even read it gets index.ErrNotFound (404),
// the answer a repo that does not exist gives. The authorizer's own error is returned unchanged
// when it cannot decide (WriteError answers 500). A nil deps.Auth allows everything: the dev
// stack before RBAC (#45).
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
	}
	if a != auth.ActionRead {
		canRead, err := deps.Auth.Allowed(ctx, p, auth.ActionRead, repo.Format, repo.Name)
		switch {
		case err != nil:
			return err
		case canRead:
			return fmt.Errorf("%w: %s on %s/%s", ErrForbidden, a, repo.Format, repo.Name)
		}
	}
	return fmt.Errorf("%w: repository %s/%s", index.ErrNotFound, repo.Format, repo.Name)
}

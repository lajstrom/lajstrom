package format

import (
	"context"
	"errors"
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
// when it cannot decide (WriteError answers 500). serve sets deps.Auth to auth.RBAC; a nil
// deps.Auth allows everything the credential allows, for the tests.
//
// The credential of the request decides first (limit), with or without an Authorizer.
func Authorize(ctx context.Context, deps Deps, a auth.Action, repo index.Repo) error {
	if err := limit(ctx, a, repo); err != nil {
		return err
	}
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

// limit applies the part of docs/spec/rbac.md §3 that depends on the request's credential and
// not on the bindings (auth.Credential.Limit, steps 2 and 3): a personal access token's scopes
// narrow its user, a deploy token reaches its own repo only and never delete or manage. A
// deploy token on another repo is index.ErrNotFound (404, no access at all), an action outside
// the scopes ErrForbidden (403). It holds whether or not an Authorizer exists, so that the
// scopes count whatever the Authorizer; auth.RBAC adds the bindings behind deps.Auth, after it.
func limit(ctx context.Context, a auth.Action, repo index.Repo) error {
	err := auth.CredentialFrom(ctx).Limit(a, repo)
	if errors.Is(err, auth.ErrScope) {
		return fmt.Errorf("%w: %s on %s/%s: %w", ErrForbidden, a, repo.Format, repo.Name, err)
	}
	return err
}

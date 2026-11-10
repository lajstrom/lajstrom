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
// The credential of the request decides first (limit), with or without an Authorizer; when it
// refuses a user's action, the bindings still decide between 403 and 404 (refusedByCredential).
//
// Through a group (ViaFrom: WriteTarget found that the principal may read the group), the 404
// of a principal that cannot read repo, the group's member, is ErrForbidden naming the member:
// the group's resource shows its members to its readers (docs/spec/rbac.md §7).
func Authorize(ctx context.Context, deps Deps, a auth.Action, repo index.Repo) error {
	err := authorize(ctx, deps, a, repo)
	if group, ok := ViaFrom(ctx); ok && errors.Is(err, index.ErrNotFound) {
		return fmt.Errorf("%w: %s on %s/%s, the hosted member of the group %s/%s", ErrForbidden, a, repo.Format, repo.Name,
			group.Format, group.Name)
	}
	return err
}

// authorize is Authorize without the answer of a request through a group.
func authorize(ctx context.Context, deps Deps, a auth.Action, repo index.Repo) error {
	if err := limit(ctx, a, repo); err != nil {
		return refusedByCredential(ctx, deps, repo, err)
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

// refusedByCredential answers the refusal err of the credential (limit): a personal access token
// without the scope of the action is ErrForbidden, which tells that the repo exists, so a user
// gets it only when its bindings let it read the repo, and otherwise the index.ErrNotFound of a
// repo that does not exist (docs/spec/rbac.md §3). A deploy token is bound to its repo and
// knows it: its refusals stay as they are, and so does any refusal without an Authorizer.
func refusedByCredential(ctx context.Context, deps Deps, repo index.Repo, err error) error {
	p := auth.PrincipalFrom(ctx)
	if !errors.Is(err, ErrForbidden) || deps.Auth == nil || p.Kind != auth.KindUser {
		return err
	}
	bindings := auth.WithCredential(ctx, auth.Credential{}) // the user's own reach, not the token's
	canRead, aerr := deps.Auth.Allowed(bindings, p, auth.ActionRead, repo.Format, repo.Name)
	switch {
	case aerr != nil:
		return aerr
	case canRead:
		return err
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

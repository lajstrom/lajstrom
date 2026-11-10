package format

import (
	"context"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// viaKey is the context key of the group a request came through.
type viaKey struct{}

// WithVia returns a copy of ctx that marks a request that came through group (ADR 0013,
// docs/spec/proxy-and-group.md §8): ingest adds details.via, <format>/<group>, to the audit rows
// of the uploads, deletes and journal entries made with it, the proxy core to its proxy.fetch
// and proxy.reject rows (docs/spec/rbac.md §7), and Authorize answers a principal that cannot
// read the group's member 403 naming it.
func WithVia(ctx context.Context, group index.Repo) context.Context {
	return context.WithValue(ctx, viaKey{}, group)
}

// ViaFrom returns the group that ctx came through, set by WithVia; ok is false when it came
// through none.
func ViaFrom(ctx context.Context) (group index.Repo, ok bool) {
	group, ok = ctx.Value(viaKey{}).(index.Repo)
	return group, ok
}

// WriteTarget returns the repo that takes a write route of a format sent to repo, and the
// context to serve it with (docs/spec/proxy-and-group.md §8.4): a hosted repo and ctx as they
// are; for a group, once the principal may read the group (Authorize), its hosted member and
// WithVia(ctx, repo); for a proxy, or a group without a hosted member, an error wrapping
// ErrMethodNotAllowed once the principal may read it, and the denial of Authorize before that.
// The handler then authorizes the route's own action on the repo returned, with the context
// returned, and serves the write there.
func WriteTarget(ctx context.Context, deps Deps, repo index.Repo) (context.Context, index.Repo, error) {
	if repo.Kind != index.KindProxy && repo.Kind != index.KindGroup {
		return ctx, repo, nil
	}
	if err := Authorize(ctx, deps, auth.ActionRead, repo); err != nil {
		return ctx, repo, err
	}
	id := repo.Format + "/" + repo.Name
	if repo.Kind == index.KindProxy {
		return ctx, repo, fmt.Errorf("%w: %s is a proxy of an upstream and takes no writes", ErrMethodNotAllowed, id)
	}
	members, err := deps.Index.ListGroupMembers(ctx, repo.ID)
	if err != nil {
		return ctx, repo, fmt.Errorf("format: the members of %s: %w", id, err)
	}
	for _, m := range members {
		if m.Kind == index.KindHosted {
			return WithVia(ctx, repo), m, nil
		}
	}
	return ctx, repo, fmt.Errorf("%w: the group %s has no hosted member to take writes", ErrMethodNotAllowed, id)
}

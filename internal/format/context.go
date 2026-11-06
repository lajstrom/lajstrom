package format

import (
	"context"

	"github.com/lajstrom/lajstrom/internal/index"
)

// repoKey is the context key of the repo the repo middleware resolved.
type repoKey struct{}

// WithRepo returns a copy of ctx that carries repo. The repo middleware of NewRouter calls it;
// a plugin's handler tests call it to skip the lookup.
func WithRepo(ctx context.Context, repo index.Repo) context.Context {
	return context.WithValue(ctx, repoKey{}, repo)
}

// RepoFrom returns the repo of the request ctx belongs to, as the repo middleware resolved it
// from the {repo} wildcard. ok is false when there is none: the pattern has no {repo}.
func RepoFrom(ctx context.Context) (repo index.Repo, ok bool) {
	repo, ok = ctx.Value(repoKey{}).(index.Repo)
	return repo, ok
}

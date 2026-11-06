package auth

import "context"

// Action is what a principal wants to do on a repo (docs/spec/rbac.md §2).
type Action string

// Actions.
const (
	// ActionRead is GET and HEAD on a frontend, pull on /v2, listing a repo.
	ActionRead Action = "read"
	// ActionWrite is PUT and POST on a frontend, push on /v2: creating versions.
	ActionWrite Action = "write"
	// ActionDelete is deleting versions and manifests.
	ActionDelete Action = "delete"
	// ActionManage is changing the repo (policy, anonymous read, deploy tokens) and reindexing it.
	ActionManage Action = "manage"
)

// Authorizer decides whether a principal may perform an action on the repo <format>/<repo>.
// Implementations follow the evaluation order of docs/spec/rbac.md §3: admin binding, deploy
// token, PAT scopes, user and group bindings, anonymous read, deny. A missing repo is a deny,
// not an error; errors are reserved for failures to evaluate (the database is down).
type Authorizer interface {
	// Allowed reports whether p may perform a on format/repo.
	Allowed(ctx context.Context, p Principal, a Action, format, repo string) (bool, error)
}

package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/index"
)

// ErrScope means the credential of a request does not allow an action on a repo it may read:
// a personal access token whose scopes leave the action out, a deploy token without the scope,
// and delete or manage, which neither ever gets. format.Authorize answers it 403
// (format.ErrForbidden); token.Reduce drops the action.
var ErrScope = errors.New("auth: outside the credential's scopes")

// CredentialKind is how the principal of a request authenticated.
type CredentialKind string

// Credential kinds. The zero value is a request without a credential: anonymous, or a
// principal that something other than the authentication middleware put in the context (the
// facade's bearer, a test).
const (
	// CredPassword is a local user's password.
	CredPassword CredentialKind = "password"
	// CredPAT is a personal access token: the principal is its user.
	CredPAT CredentialKind = "pat"
	// CredDeploy is a deploy token: the principal is token:<id>.
	CredDeploy CredentialKind = "deploy"
)

// Credential is how the principal of a request authenticated, beside the principal in the
// context (WithCredential, CredentialFrom). The Authenticator returns it with the principal;
// the authentication middleware of internal/server stores both.
type Credential struct {
	// Kind is the kind of the credential; empty for a request without one.
	Kind CredentialKind
	// TokenID is the id of the token of a CredPAT or a CredDeploy.
	TokenID int64
	// UserID is the id of the user of a CredPAT.
	UserID int64
	// RepoID is the id of the repo of a CredDeploy.
	RepoID int64
	// Repo is <format>/<name> of the repo of a CredDeploy: token.Reduce and the management API
	// know a repo by its name only.
	Repo string
	// Scopes are the scopes of the token, read and/or write. An empty list on a CredPAT means
	// the user's full rights.
	Scopes []string
}

// Limit is the part of the evaluation of docs/spec/rbac.md §3 that depends on the credential
// and not on the bindings (steps 2 and 3): nil when the credential leaves the action a on repo
// to the Authorizer.
//
//   - No credential and a password limit nothing.
//   - A personal access token without scopes limits nothing: it is its user. With scopes it
//     allows read when it has read and write when it has write, and never delete or manage: an
//     error wrapping ErrScope.
//   - A deploy token on any repo but its own is an error wrapping index.ErrNotFound: it has no
//     access there at all, and a denial must not tell whether the repo exists (404). On its own
//     repo it allows read and write per its scopes, and never delete or manage (ErrScope: it
//     can read the repo, so the denial is a 403).
//
// The repo is matched by <format>/<name>, and by id as well when repo.ID is set: the names of
// a repo never change, and a repo deleted takes its deploy tokens with it.
func (c Credential) Limit(a Action, repo index.Repo) error {
	switch c.Kind {
	case CredPAT:
		if len(c.Scopes) == 0 || c.has(a) {
			return nil
		}
		return fmt.Errorf("%w: the personal access token has %s", ErrScope, scopeList(c.Scopes))
	case CredDeploy:
		if c.Repo != repo.Format+"/"+repo.Name || (repo.ID != 0 && repo.ID != c.RepoID) {
			return fmt.Errorf("%w: repository %s/%s", index.ErrNotFound, repo.Format, repo.Name)
		}
		if c.has(a) {
			return nil
		}
		return fmt.Errorf("%w: the deploy token has %s", ErrScope, scopeList(c.Scopes))
	case CredPassword:
	}
	return nil
}

// has reports whether the scopes grant a: read and write only, each by its own scope.
func (c Credential) has(a Action) bool {
	return (a == ActionRead || a == ActionWrite) && slices.Contains(c.Scopes, string(a))
}

// scopeList is the scopes for a message: "read", "read and write", "no scope".
func scopeList(scopes []string) string {
	if len(scopes) == 0 {
		return "no scope"
	}
	return strings.Join(scopes, " and ")
}

// credentialKey is the context key of the request's credential.
type credentialKey struct{}

// WithCredential returns a copy of ctx that carries c, the credential of the request's
// principal. The authentication middleware stores it with the principal (WithPrincipal).
func WithCredential(ctx context.Context, c Credential) context.Context {
	return context.WithValue(ctx, credentialKey{}, c)
}

// CredentialFrom returns the credential WithCredential stored in ctx, or the zero Credential,
// which limits nothing, for a request without one.
func CredentialFrom(ctx context.Context) Credential {
	c, _ := ctx.Value(credentialKey{}).(Credential)
	return c
}

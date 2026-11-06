// Package auth owns authentication and authorization: principals, actions, the Authorizer,
// password hashing, personal access and deploy tokens, RBAC evaluation and bearer JWTs.
// See docs/spec/rbac.md and ADR 0007.
//
// Principal, Action and the Authorizer interface ship with issue #7, the request's principal in
// the context (WithPrincipal, PrincipalFrom) with #11; local users come with issue #43, tokens
// with #44, RBAC with #45, bearer tokens with #26.
package auth

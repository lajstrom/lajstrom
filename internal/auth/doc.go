// Package auth owns authentication and authorization: principals, actions, the Authorizer,
// password hashing, personal access and deploy tokens, RBAC evaluation and bearer JWTs.
// See docs/spec/rbac.md and ADR 0007.
//
// Principal, Action and the Authorizer interface ship with issue #7, the request's principal in
// the context (WithPrincipal, PrincipalFrom) with #11. Issue #43 brings local users: argon2id
// passwords (HashPassword, VerifyPassword), the Authenticator that turns HTTP Basic credentials
// into a principal, with its verification cache, and the Users service of lajstrom admin user
// (docs/auth.md). Issue #26 brings the bearer tokens of /v2 (auth/token, keys.Tokens). Issue
// #44 brings personal access tokens and deploy tokens: the Tokens service of lajstrom admin
// token and the tokens API, their secrets (PATPrefix, DeployPrefix, HashSecret), their paths
// in the Authenticator (Basic user:<PAT> and deploy:<token>, AuthenticateBearer), and the
// Credential of a request, whose Limit is the part of RBAC that depends on the credential
// (format.Authorize and token.Reduce apply it). RBAC comes with #45.
package auth

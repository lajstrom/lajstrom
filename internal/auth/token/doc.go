// Package token is the docker bearer flow of the OCI facade (docs/spec/rbac.md §4, ADR 0010):
// the JWTs that GET /v2/token issues and every other /v2 request carries, the scopes a client
// asks for, their reduction to what the principal may do, and the challenge of a 401.
//
// The Issuer signs with the active token key of internal/keys (Keys: keys.Tokens, or
// keys.EphemeralTokens without a master key): ES256 or EdDSA after the key, the key's
// fingerprint as kid, iss and aud the service, sub the principal's canonical form, exp 20
// minutes after iat, a random jti, and the access claims. The Verifier pins the algorithm of the
// key the kid names before it verifies, so a token can name neither none nor HMAC nor another
// key's algorithm, and keeps the keys in memory, reloaded at most once a second.
//
// internal/auth does not import this package; internal/facade and internal/server do.
package token

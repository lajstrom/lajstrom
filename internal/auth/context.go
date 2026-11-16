package auth

import "context"

// principalKey is the context key of the request's principal.
type principalKey struct{}

// WithPrincipal returns a copy of ctx that carries p, the principal of the request. The
// authentication middleware of internal/server stores it from HTTP Basic credentials on every
// route (issue #43); the facade stores it from the bearer token on /v2 (issue #26). A request
// without credentials carries none and is anonymous.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal WithPrincipal stored in ctx, or Anonymous when nothing
// was stored. A stored zero Principal, which is not valid, is Anonymous too.
func PrincipalFrom(ctx context.Context) Principal {
	if p, ok := ctx.Value(principalKey{}).(Principal); ok && p.Kind != "" {
		return p
	}
	return Anonymous
}

// originKey is the context key of the origin of a change.
type originKey struct{}

// WithOrigin returns a copy of ctx that carries o, the origin of the changes made under it, for
// the audit rows whose writers take no Origin of their own: lajstrom admin key and lajstrom
// reindex store the command line's (docs/spec/rbac.md §6).
func WithOrigin(ctx context.Context, o Origin) context.Context {
	return context.WithValue(ctx, originKey{}, o)
}

// OriginFrom returns the origin WithOrigin stored in ctx, the zero Origin when none was.
func OriginFrom(ctx context.Context) Origin {
	o, _ := ctx.Value(originKey{}).(Origin)
	return o
}

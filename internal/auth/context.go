package auth

import "context"

// principalKey is the context key of the request's principal.
type principalKey struct{}

// WithPrincipal returns a copy of ctx that carries p, the principal of the request. The
// authentication middleware stores it: HTTP Basic on the frontends (issue #43) and the bearer
// on /v2 (issue #26). Until then nothing stores one and every request is anonymous.
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

package auth

import "context"

// principalKey is the context key of the request's principal.
type principalKey struct{}

// WithPrincipal returns a copy of ctx that carries p, the principal of the request. The
// authentication middleware of internal/server stores it from HTTP Basic credentials on every
// route (issue #43); the facade will from the bearer on /v2 (issue #26). A request without
// credentials carries none and is anonymous.
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

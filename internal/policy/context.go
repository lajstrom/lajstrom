package policy

import "context"

// deciderKey is the context key of the Decider of a request.
type deciderKey struct{}

// NewContext returns a copy of ctx that carries d. The repo middleware of format.NewRouter calls
// it with format.Deps.Policy, so that format.CachedDocument and format.ServeBlob, which every
// frontend calls, decide without a parameter of their own.
func NewContext(ctx context.Context, d *Decider) context.Context {
	if d == nil {
		return ctx
	}
	return context.WithValue(ctx, deciderKey{}, d)
}

// FromContext returns the Decider of ctx, nil when it has none, which allows everything.
func FromContext(ctx context.Context) *Decider {
	d, _ := ctx.Value(deciderKey{}).(*Decider)
	return d
}

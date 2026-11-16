package index

import "context"

// WithCondition returns a copy of ctx under which a change of a repo, a user or a webhook is
// made only while cond accepts the row: its id and its revision as the change's transaction
// read them. cond returns nil, or the error the change fails with, and nothing changes. It is
// the client's If-Match (docs/api.md, Revisions).
//
// The services that change these rows call CheckCondition on the row they read, in a
// transaction begun with the context of ConditionContext, so that a change of the row committed
// after the read aborts the transaction, which runs again and asks cond on the row as the other
// change left it: of two changes under the same condition, one is made.
func WithCondition(ctx context.Context, cond func(id, revision int64) error) context.Context {
	return context.WithValue(ctx, conditionKey{}, cond)
}

// HasCondition reports whether ctx has a condition of WithCondition.
func HasCondition(ctx context.Context) bool {
	_, ok := ctx.Value(conditionKey{}).(func(int64, int64) error)
	return ok
}

// ConditionContext returns the context of Store.Tx for a change that checks the condition of
// ctx: ctx under WithRepeatableRead when it has a condition, ctx as it is otherwise, so that a
// change without one runs as before.
func ConditionContext(ctx context.Context) context.Context {
	if !HasCondition(ctx) {
		return ctx
	}
	return WithRepeatableRead(ctx)
}

// CheckCondition returns the error of the condition of ctx on the row id at revision: nil when
// ctx has none, or when the condition accepts the row.
func CheckCondition(ctx context.Context, id, revision int64) error {
	cond, ok := ctx.Value(conditionKey{}).(func(int64, int64) error)
	if !ok {
		return nil
	}
	return cond(id, revision)
}

// conditionKey is the context key of WithCondition.
type conditionKey struct{}

package index

import "context"

// WithRepeatableRead returns a copy of ctx with which Store.Tx runs fn in a transaction that
// does not lose a concurrent update. fn reads one snapshot of the database. When another
// transaction committed a change after that snapshot to a row that fn then writes, the database
// aborts fn's transaction rather than let fn's write undo the other one. Tx then rolls back
// and runs fn again in a new transaction, which reads the row as the other left it, up to
// four times in all; the error of the last attempt is returned, and it wraps ErrUnavailable.
// So fn may run more than once: it must start from what it reads on its Writer and must not
// act outside the transaction. An audit event written after the commit waits until Tx
// returns nil.
//
// On Postgres and YugabyteDB the transaction is REPEATABLE READ, and the aborts are a
// serialization failure (40001) and a deadlock (40P01). On SQLite every write transaction
// holds the database's write lock from its start, so the writes are serialized already and fn
// runs once. Without WithRepeatableRead, Tx runs at the database's default (read committed on
// Postgres) and runs fn once. The update of a repo uses it (#791).
func WithRepeatableRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, repeatableReadKey{}, true)
}

// IsRepeatableRead reports whether ctx comes from WithRepeatableRead. The implementations of
// Store.Tx read it.
func IsRepeatableRead(ctx context.Context) bool {
	on, _ := ctx.Value(repeatableReadKey{}).(bool)
	return on
}

// repeatableReadKey is the context key of WithRepeatableRead.
type repeatableReadKey struct{}

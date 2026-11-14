// Package sqlstore is the SQL implementation of index.Store that the SQLite and Postgres
// drivers share. The statements are written once, with ? placeholders; a Dialect rewrites the
// placeholders for Postgres, encodes timestamps (text on SQLite, timestamptz on Postgres) and
// recognises the driver's constraint errors. The drivers open the *sql.DB and embed *Store.
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

// Store implements index.Store over a *sql.DB. Every method outside Tx is a transaction of
// its own: single statements run on the pool, methods with several statements begin a
// transaction. Inside Tx every method runs on the transaction of Tx.
type Store struct {
	*conn
	db *sql.DB
}

var _ index.Store = (*Store)(nil)

// New returns a Store over db, which the Store owns: Close closes it. db must have the schema
// of the migrations of d.Name, or get it with Migrate.
func New(db *sql.DB, d Dialect) *Store {
	return &Store{conn: &conn{q: db, db: db, d: &d}, db: db}
}

// Migrate applies the embedded migrations that are not applied yet (index.ApplyMigrations).
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.ApplyMigrations(ctx, index.MigrateOptions{})
	return err
}

// ApplyMigrations is Migrate with options, and it reports what it found and applied.
func (s *Store) ApplyMigrations(ctx context.Context, opts index.MigrateOptions) (index.MigrationReport, error) {
	return index.ApplyMigrations(ctx, s.db, s.d.Name, opts)
}

// AppliedMigrations returns the versions recorded in schema_migrations, ascending.
func (s *Store) AppliedMigrations(ctx context.Context) ([]int, error) {
	return index.AppliedMigrations(ctx, s.db)
}

// TxAttempts is how many times in all Tx runs fn under index.WithRepeatableRead when the
// database aborts each of its transactions.
const TxAttempts = 4

// Tx runs fn in one transaction: it commits when fn returns nil and otherwise rolls back and
// returns fn's error unchanged. Under index.WithRepeatableRead, on an engine with
// Dialect.IsAborted, the transaction is repeatable read, and one that the database aborts, in
// fn or at the commit, is run again from the start, up to TxAttempts times in all.
func (s *Store) Tx(ctx context.Context, fn func(index.Writer) error) error {
	run := func(c *conn) error { return fn(c) }
	if !index.IsRepeatableRead(ctx) || s.d.IsAborted == nil {
		return s.begin(ctx, s.d.txOptions(), run) // once, unlike withTx: fn is the caller's
	}
	opts := &sql.TxOptions{Isolation: sql.LevelRepeatableRead}
	for attempt := 1; ; attempt++ {
		err := s.begin(ctx, opts, run)
		if err == nil || attempt == TxAttempts || !s.d.IsAborted(err) || ctx.Err() != nil {
			return err
		}
	}
}

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return s.d.wrap(err, "ping")
	}
	return nil
}

// Close closes the connection pool.
func (s *Store) Close() error { return s.db.Close() }

// queryer is what conn runs statements on: *sql.DB or *sql.Tx.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// conn implements index.Writer on a queryer: the pool (db is set) or a transaction (db is
// nil).
type conn struct {
	q  queryer
	db *sql.DB
	d  *Dialect
}

var _ index.Writer = (*conn)(nil)

// withTx runs fn in a transaction. On the pool it begins one, at the dialect's isolation
// (Dialect.Isolation), commits when fn returns nil and rolls back otherwise; inside a
// transaction it runs fn on it, without a savepoint: the outer transaction is what makes the
// writes atomic. On the pool, a transaction that the database aborts (Dialect.IsAborted), in
// fn or at the commit, runs again from the start, up to Dialect.Attempts times in all: fn is a
// store method's, which computes what it writes from what it reads in the transaction and sets
// the fields of its arguments by assignment, so that a second run starts over.
func (c *conn) withTx(ctx context.Context, fn func(*conn) error) error {
	if c.db == nil {
		return fn(c)
	}
	for attempt := 1; ; attempt++ {
		err := c.begin(ctx, c.d.txOptions(), fn)
		if err == nil || attempt >= c.d.Attempts || c.d.IsAborted == nil || !c.d.IsAborted(err) || ctx.Err() != nil {
			return err
		}
	}
}

// begin runs fn in a transaction that it begins on the pool with opts (nil: the database's
// defaults), commits when fn returns nil and rolls back otherwise.
func (c *conn) begin(ctx context.Context, opts *sql.TxOptions, fn func(*conn) error) error {
	tx, err := c.db.BeginTx(ctx, opts)
	if err != nil {
		return c.d.wrap(err, "begin")
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(&conn{q: tx, d: c.d}); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return c.d.wrap(err, "commit")
	}
	return nil
}

// now is the timestamp the store fills in: the current time, normalized.
func now() time.Time { return Normalize(time.Now()) }

// fail wraps err, the error of a statement about what (a table and the key of the row, e.g.
// "repos raw/scratch"): no row is index.ErrNotFound, a unique violation index.ErrConflict, a
// missing referenced row index.ErrNotFound. The driver's message is dropped from those, so
// it does not reach a client; any other error is wrapped unchanged, with
// index.ErrUnavailable when the database did not answer (Dialect.IsUnavailable).
func (c *conn) fail(err error, what string) error {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return notFound(what)
	case c.d.IsUnique(err):
		return fmt.Errorf("%w: %s exists", index.ErrConflict, what)
	case c.d.IsForeignKey(err):
		return fmt.Errorf("%w: %s refers to a row that does not exist", index.ErrNotFound, what)
	}
	return c.d.wrap(err, what)
}

// notFound returns an error wrapping index.ErrNotFound about what.
func notFound(what string) error { return fmt.Errorf("%w: %s", index.ErrNotFound, what) }

// exec runs a statement and returns the number of rows it changed.
func (c *conn) exec(ctx context.Context, what, query string, args ...any) (_ int, err error) {
	defer observe(time.Now(), &err)
	res, err := c.q.ExecContext(ctx, c.d.Rebind(query), args...)
	if err != nil {
		return 0, c.fail(err, what)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, c.fail(err, what)
	}
	return int(n), nil
}

// execOne runs a statement that changes the row of one id: no row changed is
// index.ErrNotFound.
func (c *conn) execOne(ctx context.Context, what, query string, args ...any) error {
	n, err := c.exec(ctx, what, query, args...)
	if err == nil && n == 0 {
		err = notFound(what)
	}
	return err
}

// scanRow runs a query that returns one row and scans it into dests; no row is
// index.ErrNotFound.
func (c *conn) scanRow(ctx context.Context, what, query string, args []any, dests ...any) (err error) {
	defer observe(time.Now(), &err)
	if err := c.q.QueryRowContext(ctx, c.d.Rebind(query), args...).Scan(dests...); err != nil {
		return c.fail(err, what)
	}
	return nil
}

// insertID runs an INSERT … ON CONFLICT DO NOTHING RETURNING id into *id and reports whether
// it inserted a row: false means a unique constraint holds another row.
func (c *conn) insertID(ctx context.Context, what, query string, args []any, id *int64) (_ bool, err error) {
	defer observe(time.Now(), &err)
	err = c.q.QueryRowContext(ctx, c.d.Rebind(query), args...).Scan(id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, c.fail(err, what)
	}
	return true, nil
}

// getOne runs a query that returns one row of T, scanned into dests(&v).
func getOne[T any](ctx context.Context, c *conn, what string, dests func(*T) []any, query string, args ...any) (*T, error) {
	var v T
	if err := c.scanRow(ctx, what, query, args, dests(&v)...); err != nil {
		return nil, err
	}
	return &v, nil
}

// getAll runs a query that returns rows of T, each scanned into dests(&v). No row is an
// empty slice.
func getAll[T any](ctx context.Context, c *conn, what string, dests func(*T) []any, query string, args ...any) (_ []T, err error) {
	defer observe(time.Now(), &err)
	rows, err := c.q.QueryContext(ctx, c.d.Rebind(query), args...)
	if err != nil {
		return nil, c.fail(err, what)
	}
	defer func() { _ = rows.Close() }()
	out := []T{}
	for rows.Next() {
		var v T
		if err := rows.Scan(dests(&v)...); err != nil {
			return nil, c.fail(err, what)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, c.fail(err, what)
	}
	return out, nil
}

// where collects the conditions and arguments of a query with optional filters.
type where struct {
	conds []string
	args  []any
}

// add appends a condition with its arguments.
func (w *where) add(cond string, args ...any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

// clause returns " WHERE c1 AND c2 …", or "" without conditions.
func (w *where) clause() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// qualify prefixes every column of a comma-separated list with alias.
func qualify(alias, cols string) string {
	parts := strings.Split(cols, ", ")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ", ")
}

// observe records a statement that began at start in lajstrom_db_query_duration_seconds, by the
// exported store method that ran it and whether *errp holds an error other than "no row".
func observe(start time.Time, errp *error) {
	result := "ok"
	if *errp != nil && !errors.Is(*errp, index.ErrNotFound) {
		result = "error"
	}
	metrics.DBDuration.WithLabelValues(storeMethod(), result).Observe(time.Since(start).Seconds())
}

// storeMethod returns the name of the innermost exported method of conn on the stack of the
// statement, "other" when there is none: a label of the DB metrics from a set the code fixes.
func storeMethod() string {
	var pcs [12]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs[:])])
	for {
		f, more := frames.Next()
		if _, name, ok := strings.Cut(f.Function, ".(*conn)."); ok && name != "" && name[0] >= 'A' && name[0] <= 'Z' &&
			!strings.Contains(name, ".") {
			return name
		}
		if !more {
			return "other"
		}
	}
}

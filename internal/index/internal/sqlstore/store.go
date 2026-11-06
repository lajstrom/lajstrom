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
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
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
	_, err := s.ApplyMigrations(ctx)
	return err
}

// ApplyMigrations is Migrate that also returns the versions it applied: empty when the schema
// was current.
func (s *Store) ApplyMigrations(ctx context.Context) ([]int, error) {
	return index.ApplyMigrations(ctx, s.db, s.d.Name)
}

// AppliedMigrations returns the versions recorded in schema_migrations, ascending.
func (s *Store) AppliedMigrations(ctx context.Context) ([]int, error) {
	return index.AppliedMigrations(ctx, s.db)
}

// Tx runs fn in one transaction: it commits when fn returns nil and otherwise rolls back and
// returns fn's error unchanged.
func (s *Store) Tx(ctx context.Context, fn func(index.Writer) error) error {
	return s.withTx(ctx, func(c *conn) error { return fn(c) })
}

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("index: ping: %w", err)
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

// withTx runs fn in a transaction. On the pool it begins one, commits when fn returns nil and
// rolls back otherwise; inside a transaction it runs fn on it, without a savepoint: the outer
// transaction is what makes the writes atomic.
func (c *conn) withTx(ctx context.Context, fn func(*conn) error) (err error) {
	if c.db == nil {
		return fn(c)
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("index: begin: %w", err)
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
		return fmt.Errorf("index: commit: %w", err)
	}
	return nil
}

// now is the timestamp the store fills in: the current time, normalized.
func now() time.Time { return Normalize(time.Now()) }

// fail wraps err, the error of a statement about what (a table and the key of the row, e.g.
// "repos raw/scratch"): no row is index.ErrNotFound, a unique violation index.ErrConflict, a
// missing referenced row index.ErrNotFound. The driver's message is dropped from those, so
// it does not reach a client; any other error is wrapped unchanged.
func (c *conn) fail(err error, what string) error {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return notFound(what)
	case c.d.IsUnique(err):
		return fmt.Errorf("%w: %s exists", index.ErrConflict, what)
	case c.d.IsForeignKey(err):
		return fmt.Errorf("%w: %s refers to a row that does not exist", index.ErrNotFound, what)
	}
	return fmt.Errorf("index: %s: %w", what, err)
}

// notFound returns an error wrapping index.ErrNotFound about what.
func notFound(what string) error { return fmt.Errorf("%w: %s", index.ErrNotFound, what) }

// exec runs a statement and returns the number of rows it changed.
func (c *conn) exec(ctx context.Context, what, query string, args ...any) (int, error) {
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
func (c *conn) scanRow(ctx context.Context, what, query string, args []any, dests ...any) error {
	if err := c.q.QueryRowContext(ctx, c.d.Rebind(query), args...).Scan(dests...); err != nil {
		return c.fail(err, what)
	}
	return nil
}

// insertID runs an INSERT … ON CONFLICT DO NOTHING RETURNING id into *id and reports whether
// it inserted a row: false means a unique constraint holds another row.
func (c *conn) insertID(ctx context.Context, what, query string, args []any, id *int64) (bool, error) {
	err := c.q.QueryRowContext(ctx, c.d.Rebind(query), args...).Scan(id)
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
func getAll[T any](ctx context.Context, c *conn, what string, dests func(*T) []any, query string, args ...any) ([]T, error) {
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

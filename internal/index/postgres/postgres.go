package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/internal/sqlstore"
)

// Store is the index on PostgreSQL. It implements index.Store; Ping checks that the database
// answers, ApplyMigrations and AppliedMigrations report on the schema, and Close closes the
// connection pool.
type Store struct {
	*sqlstore.Store
}

var _ index.Store = (*Store)(nil)

// poolParam is the DSN parameter that sizes the connection pool, as pgxpool spells it.
const poolParam = "pool_max_conns"

// SQLSTATE codes of the constraint violations the store maps to index errors, of a statement
// the database user has no right to run, and of CockroachDB's commit whose result is unknown.
const (
	uniqueViolation       = "23505"
	foreignKeyViolation   = "23503"
	insufficientPrivilege = "42501"
	ambiguousResult       = "40003" // statement_completion_unknown
)

// unavailableStates are the SQLSTATE codes of a server that ends or refuses connections, which
// the store maps to index.ErrUnavailable, beside the class 08 (connection exception): it shuts
// down or starts, it has no connection left, or a timeout of its own ended the session (#627).
var unavailableStates = map[string]bool{
	"57P01": true, // admin_shutdown: terminating connection due to administrator command
	"57P02": true, // crash_shutdown
	"57P03": true, // cannot_connect_now: the database system is starting up or shutting down
	"53300": true, // too_many_connections
	"25P03": true, // idle_in_transaction_session_timeout
	"57P05": true, // idle_session_timeout
	"25P04": true, // transaction_timeout (Postgres 17 and later)
}

// abortedStates are the SQLSTATE codes of a transaction that the database aborted and that may
// pass when run again, which the store maps to index.ErrUnavailable too: a deadlock it broke, or
// a conflict it could not serialize (on YugabyteDB also one it could not retry itself, #629).
var abortedStates = map[string]bool{
	"40001": true, // serialization_failure: a conflict, or a read that must restart
	"40P01": true, // deadlock_detected
}

var dialect = sqlstore.Dialect{
	Name:          index.DialectPostgres,
	Numbered:      true,
	IsUnique:      func(err error) bool { return sqlState(err) == uniqueViolation },
	IsForeignKey:  func(err error) bool { return sqlState(err) == foreignKeyViolation },
	IsAborted:     func(err error) bool { return abortedStates[sqlState(err)] },
	IsUnavailable: isUnavailable,
	XactLock:      "SELECT set_config('idle_in_transaction_session_timeout', '0', true), pg_advisory_xact_lock(?)",
	Clock:         "SELECT clock_timestamp()",
}

// yugabyte is the dialect on YugabyteDB (docs/deploy/databases.md#yugabytedb), which Open tells
// from Postgres by the server_version the server reports ("15.12-YB-2026.1.3.0-b0"): its
// UpsertPackage inserts under a lock (sqlstore.Dialect.PackageLock, #629), which Postgres does
// not need.
var yugabyte = func() sqlstore.Dialect {
	d := dialect
	d.PackageLock = "SELECT pg_advisory_xact_lock(?)"
	return d
}()

// cockroach is the dialect on CockroachDB (docs/deploy/databases.md#cockroachdb), which Open
// tells from Postgres by the crdb_version the server reports. It differs in four things: its
// migrations are CockroachDB's own (index.DialectCockroach); every transaction asks for read
// committed, which CockroachDB's transactions are not unless they ask (Open checks that the
// cluster lets them, checkReadCommitted); a store method's own transaction that CockroachDB
// aborts, as it does one of two that deadlock on the lock of a unique key (40001), runs again,
// up to sqlstore.TxAttempts times in all; and an ambiguous result (40003), a commit that may or
// may not have happened, is index.ErrUnavailable, never run again, as IsAborted leaves it.
var cockroach = func() sqlstore.Dialect {
	d := dialect
	d.Name = index.DialectCockroach
	d.Attempts = sqlstore.TxAttempts
	d.Isolation = sql.LevelReadCommitted
	d.IsUnavailable = func(err error) bool { return isUnavailable(err) || sqlState(err) == ambiguousResult }
	return d
}()

// Open connects to the PostgreSQL database of dsn, a URL or a keyword/value string as pgx
// parses it (docs/configuration.md): postgres://lajstrom@db:5432/lajstrom?sslmode=require.
// The pool holds pool_max_conns connections when the DSN sets it, else max(4, number of
// CPUs). Open checks that the database answers; it does not migrate (Migrate does). Errors
// never contain the DSN, which may carry a password.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		var pe *pgconn.ParseConfigError
		if errors.As(err, &pe) && pe.Unwrap() != nil {
			return nil, fmt.Errorf("index: postgres: database.dsn cannot be parsed: %w", pe.Unwrap())
		}
		return nil, errors.New("index: postgres: database.dsn cannot be parsed")
	}
	size, err := poolSize(cfg)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(size)
	db.SetMaxIdleConns(size)
	d, err := dialectOf(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("index: postgres: connect: %w", err)
	}
	return &Store{sqlstore.New(db, d)}, nil
}

// dialectOf pings db and returns its dialect, by the parameters the server reports when a
// connection starts: cockroach when it reports crdb_version, once checkReadCommitted passes;
// yugabyte when its server_version says YugabyteDB ("15.12-YB-2026.1.3.0-b0"); else dialect.
func dialectOf(ctx context.Context, db *sql.DB) (sqlstore.Dialect, error) {
	p, err := serverParams(ctx, db)
	if err != nil {
		return dialect, err
	}
	switch {
	case p.cockroach():
		if err := checkReadCommitted(ctx, db); err != nil {
			return dialect, err
		}
		return cockroach, nil
	case p.yugabyte():
		return yugabyte, nil
	}
	return dialect, nil
}

// params are the parameters of the server that tell the engine apart.
type params struct{ serverVersion, crdbVersion string }

// yugabyte reports whether the server is YugabyteDB.
func (p params) yugabyte() bool { return strings.Contains(p.serverVersion, "-YB-") }

// cockroach reports whether the server is CockroachDB.
func (p params) cockroach() bool { return p.crdbVersion != "" }

// serverParams pings db and returns the parameters that the server reported at the start of
// the connection. No statement runs but the ping.
func serverParams(ctx context.Context, db *sql.DB) (params, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return params{}, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.PingContext(ctx); err != nil {
		return params{}, err
	}
	var p params
	err = conn.Raw(func(c any) error {
		if c, ok := c.(*stdlib.Conn); ok {
			pc := c.Conn().PgConn()
			p = params{serverVersion: pc.ParameterStatus("server_version"), crdbVersion: pc.ParameterStatus("crdb_version")}
		}
		return nil
	})
	return p, err
}

// isYugabyte pings db and reports whether the server is YugabyteDB (serverParams).
func isYugabyte(ctx context.Context, db *sql.DB) (bool, error) {
	p, err := serverParams(ctx, db)
	return p.yugabyte(), err
}

// checkReadCommitted fails when a transaction on CockroachDB that asks for read committed
// runs at another level: without the cluster setting sql.txn.read_committed_isolation.enabled,
// CockroachDB runs it serializable, without an error, and the store relies on read committed
// (the index cache generations, the tag claims, the package row, docs/deploy/databases.md).
func checkReadCommitted(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: cockroach.Isolation})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var level string
	if err := tx.QueryRowContext(ctx, "SHOW transaction_isolation").Scan(&level); err != nil {
		return err
	}
	if level != "read committed" {
		return fmt.Errorf("CockroachDB runs a transaction that asks for read committed at %s: "+
			"SET CLUSTER SETTING sql.txn.read_committed_isolation.enabled = true (docs/deploy/databases.md#cockroachdb)", level)
	}
	return nil
}

// ApplyMigrations is index.ApplyMigrations on the store's database. A database user with data
// rights only runs it on a current schema, which it does not change; when a migration is
// pending, the error says that the schema's owner must migrate first (docs/operations.md,
// Upgrades).
func (s *Store) ApplyMigrations(ctx context.Context, opts index.MigrateOptions) (index.MigrationReport, error) {
	rep, err := s.Store.ApplyMigrations(ctx, opts)
	if sqlState(err) == insufficientPrivilege {
		err = fmt.Errorf("%w; this database user may not change the schema: run lajstrom migrate as its owner first", err)
	}
	return rep, err
}

// poolSize returns the pool size of cfg and removes pool_max_conns from the parameters that
// go to the server, which would reject it.
func poolSize(cfg *pgx.ConnConfig) (int, error) {
	v, ok := cfg.RuntimeParams[poolParam]
	if !ok {
		return max(4, runtime.NumCPU()), nil
	}
	delete(cfg.RuntimeParams, poolParam)
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("index: postgres: database.dsn: %s %q is not a positive integer", poolParam, v)
	}
	return n, nil
}

// isUnavailable reports whether err says that the database did not answer: a server error of
// unavailableStates, of abortedStates or of the class 08, also one that ended a connection
// attempt (the 57P03 of a server that starts); a connection that could not be made
// (*pgconn.ConnectError: refused, a host name that does not resolve, connect_timeout); one that
// broke (a network error, an unexpected EOF); database/sql's driver.ErrBadConn, when every
// attempt found its connection closed. Another server error, a wrong password included, is not:
// the server answered.
func isUnavailable(err error) bool {
	if state := sqlState(err); state != "" {
		return unavailableStates[state] || abortedStates[state] || strings.HasPrefix(state, "08")
	}
	var (
		ce *pgconn.ConnectError
		oe *net.OpError
	)
	return errors.As(err, &ce) || errors.As(err, &oe) || errors.Is(err, driver.ErrBadConn) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

// sqlState returns the SQLSTATE of a server error, or "".
func sqlState(err error) string {
	var e *pgconn.PgError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

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
	"time"

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

// SQLSTATE codes of the constraint violations the store maps to index errors, and of a
// statement the database user has no right to run.
const (
	uniqueViolation       = "23505"
	foreignKeyViolation   = "23503"
	insufficientPrivilege = "42501"
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

// Open connects to the PostgreSQL database of dsn, a URL or a keyword/value string as pgx
// parses it (docs/configuration.md): postgres://lajstrom@db:5432/lajstrom?sslmode=require.
// The pool holds pool_max_conns connections when the DSN sets it, else max(4, number of
// CPUs); one that the server ended while it idled is replaced before a statement uses it
// (shouldPing). Open checks that the database answers; it does not migrate (Migrate does).
// Errors never contain the DSN, which may carry a password.
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
	db := stdlib.OpenDB(*cfg, stdlib.OptionShouldPing(shouldPing))
	db.SetMaxOpenConns(size)
	db.SetMaxIdleConns(size)
	d, err := dialectOf(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("index: postgres: connect: %w", err)
	}
	return &Store{sqlstore.New(db, d)}, nil
}

// shouldPing is when database/sql pings a pooled connection before it hands it out again
// (stdlib.Conn.ResetSession): after a second of idling, pgx's default, and whenever the server
// wrote to it or closed it meanwhile (serverSent). A server that ends a session, on a restart
// (57P01), a pg_terminate_backend, a primary switched away or its idle_session_timeout
// (57P05), sends its FATAL message and closes the socket: the ping then fails and database/sql
// takes another connection, where the statement would have got that FATAL, and its request a
// 503 although the database answers (#1123). A connection with nothing to read costs no round
// trip.
func shouldPing(_ context.Context, p stdlib.ShouldPingParams) bool {
	return p.IdleDuration > time.Second || serverSent(p.Conn.PgConn().Conn())
}

// dialectOf pings db and returns its dialect: yugabyte on YugabyteDB (isYugabyte), else
// dialect.
func dialectOf(ctx context.Context, db *sql.DB) (sqlstore.Dialect, error) {
	yb, err := isYugabyte(ctx, db)
	if err != nil {
		return dialect, err
	}
	if yb {
		return yugabyte, nil
	}
	return dialect, nil
}

// isYugabyte pings db and reports whether the server_version that the server reported at the
// start of the connection says YugabyteDB. No statement runs but the ping.
func isYugabyte(ctx context.Context, db *sql.DB) (bool, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.PingContext(ctx); err != nil {
		return false, err
	}
	var version string
	if err := conn.Raw(func(c any) error {
		if c, ok := c.(*stdlib.Conn); ok {
			version = c.Conn().PgConn().ParameterStatus("server_version")
		}
		return nil
	}); err != nil {
		return false, err
	}
	return strings.Contains(version, "-YB-"), nil
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

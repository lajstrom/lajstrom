package index

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"time"
)

// migrationLockID is the key of the Postgres advisory lock that serialises migration runners:
// "lajstrom" in ASCII.
const migrationLockID int64 = 0x6c616a7374726f6d

// runner holds the statements of the migration runner that differ between dialects.
type runner struct {
	begin   string // starts the transaction; on SQLite it also takes the write lock
	lock    string // takes the migration lock inside the transaction; "" when begin does
	exists  string // reports whether schema_migrations exists
	create  string // creates schema_migrations when it does not exist
	check   string // counts the rows of one version
	record  string // records one version as applied now
	require string // sets the requires of one version (the column of 0014)

	// delay returns the pause before the attempt after the failed attempt n (from 1) of a
	// migration's transaction; nil is retryDelay. The tests pause less.
	delay func(n int) time.Duration
}

// migrationAttempts is how many times in all the runner runs the transaction of one migration
// that fails on a conflict with another schema change (retryable), #616.
const migrationAttempts = 5

// firstRetryDelay is the pause before the second attempt at a migration's transaction; it
// doubles before each attempt after it.
const firstRetryDelay = 100 * time.Millisecond

// The SQLSTATE codes of the conflicts the runner retries (retryable).
const (
	serializationFailure = "40001" // the database aborted the transaction
	duplicateObject      = "42710" // YugabyteDB: schema_migrations, from a stale catalog
)

var runners = map[string]runner{
	DialectSQLite: {
		// IMMEDIATE takes the write lock at once: a second runner waits for it (busy
		// timeout) instead of failing when both try to upgrade a read lock.
		begin:   "BEGIN IMMEDIATE",
		exists:  "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations')",
		create:  "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)",
		check:   "SELECT count(*) FROM schema_migrations WHERE version = ?",
		record:  "INSERT INTO schema_migrations (version, applied_at) VALUES (?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))",
		require: "UPDATE schema_migrations SET requires = ? WHERE version = ?",
	},
	DialectPostgres: {
		begin:   "BEGIN",
		lock:    "SELECT pg_advisory_xact_lock($1)",
		exists:  "SELECT to_regclass('schema_migrations') IS NOT NULL",
		create:  "CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL)",
		check:   "SELECT count(*) FROM schema_migrations WHERE version = $1",
		record:  "INSERT INTO schema_migrations (version, applied_at) VALUES ($1, now())",
		require: "UPDATE schema_migrations SET requires = $1 WHERE version = $2",
	},
}

// MigrateOptions are the options of ApplyMigrations.
type MigrateOptions struct {
	// Breaking lets the run apply a migration that requires a version above the schema's
	// version before the run: binaries that know only that version may be serving on it, and
	// would not run on the schema after it. Without it, ApplyMigrations applies nothing then
	// and returns ErrBreakingMigration.
	Breaking bool
	// Log is the logger of the retries of a migration's transaction; nil means slog.Default().
	Log *slog.Logger
}

// MigrationReport is what ApplyMigrations found and did.
type MigrationReport struct {
	// Applied are the versions the run applied, ascending; empty when none was pending.
	Applied []int
	// Version is the newest version schema_migrations records after the run; 0 for a
	// database that has none.
	Version int
	// Known is the newest version embedded in this binary. Version above it is a schema of a
	// newer binary that this one runs on: none of its migrations requires more than Known.
	Known int
}

// ApplyMigrations applies the embedded migrations of dialect that db has not applied yet, in
// version order (docs/spec/index-schema.md, Migrations and Upgrades).
//
// It first reads schema_migrations. When db has versions this binary does not know and one of
// them requires a version above the newest it knows, it returns ErrSchemaTooNew and applies
// nothing. When a pending migration requires a version above the newest db had before the
// run, it returns ErrBreakingMigration and applies nothing, unless opts.Breaking or db had no
// migration. When none is pending, it returns without a statement that changes the schema.
//
// Each migration runs in a transaction of its own that also records its version in
// schema_migrations (created when missing). The transaction first takes the migration lock,
// pg_advisory_xact_lock on Postgres and BEGIN IMMEDIATE on SQLite, and checks
// schema_migrations under it, so replicas that start at once apply each migration exactly
// once. A transaction that fails on a conflict with another schema change runs again from the
// start, up to migrationAttempts times in all, with a warning on opts.Log each time: one the
// database aborted (40001), and one whose create of schema_migrations finds the table that
// another migrator committed while this one waited for the lock (42710; YugabyteDB, #616). The
// version is counted again under the lock, so a retry applies it at most once. On error the
// report has the versions applied before the failing one.
func ApplyMigrations(ctx context.Context, db *sql.DB, dialect string, opts MigrateOptions) (MigrationReport, error) {
	ms, err := migrations(dialect)
	if err != nil {
		return MigrationReport{Applied: []int{}}, err
	}
	return runners[dialect].applyAll(ctx, db, ms, opts)
}

// applyAll is ApplyMigrations of the migrations ms, ascending.
func (r runner) applyAll(ctx context.Context, db *sql.DB, ms []migration, opts MigrateOptions) (MigrationReport, error) {
	rep := MigrationReport{Applied: []int{}}
	if len(ms) > 0 {
		rep.Known = ms[len(ms)-1].version
	}
	have, err := r.applied(ctx, db)
	if err != nil {
		return rep, err
	}
	if len(have) > 0 {
		rep.Version = have[len(have)-1]
	}
	if err := runsOn(ctx, db, rep); err != nil {
		return rep, err
	}
	pending := slices.DeleteFunc(slices.Clone(ms), func(m migration) bool { return slices.Contains(have, m.version) })
	for _, m := range pending {
		if m.requires > rep.Version && rep.Version > 0 && !opts.Breaking {
			return rep, fmt.Errorf("%w: migration %s needs a binary that knows migration %04d, and the schema is at version %04d",
				ErrBreakingMigration, m.name, m.requires, rep.Version)
		}
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	for _, m := range pending {
		ran, err := r.apply(ctx, db, m, log)
		if err != nil {
			return rep, fmt.Errorf("index: migration %s: %w", m.name, err)
		}
		if ran {
			rep.Applied = append(rep.Applied, m.version)
		}
		rep.Version = max(rep.Version, m.version)
	}
	return rep, nil
}

// applied returns the versions recorded in schema_migrations, ascending, and none when the
// table does not exist.
func (r runner) applied(ctx context.Context, db *sql.DB) ([]int, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, r.exists).Scan(&exists); err != nil {
		return nil, fmt.Errorf("index: migrations: %w", err)
	}
	if !exists {
		return nil, nil
	}
	return AppliedMigrations(ctx, db)
}

// runsOn returns ErrSchemaTooNew when the schema of rep has versions above the newest this
// binary knows and one of them requires more than that.
func runsOn(ctx context.Context, db *sql.DB, rep MigrationReport) error {
	if rep.Version <= rep.Known {
		return nil
	}
	var requires int
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(requires), 0) FROM schema_migrations").Scan(&requires); err != nil {
		return fmt.Errorf("index: migrations: %w", err)
	}
	if requires > rep.Known {
		return fmt.Errorf("%w: schema version %04d needs a binary that knows migration %04d, and this one knows up to %04d",
			ErrSchemaTooNew, rep.Version, requires, rep.Known)
	}
	return nil
}

// AppliedMigrations returns the versions recorded in schema_migrations, ascending. It fails
// when the table does not exist, that is before the first ApplyMigrations.
func AppliedMigrations(ctx context.Context, db *sql.DB) ([]int, error) {
	rows, err := db.QueryContext(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("index: applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	versions := []int{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("index: applied migrations: %w", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index: applied migrations: %w", err)
	}
	return versions, nil
}

// apply runs the transaction of m (attempt) and reports whether it ran m. An attempt that fails
// on a conflict with another schema change (retryable) is rolled back and the transaction runs
// again from its start, after a pause (runner.delay) and a warning on log, up to
// migrationAttempts times in all. That is correct where DDL is transactional (Postgres;
// YugabyteDB with ysql_yb_ddl_transaction_block_enabled, docs/deploy/databases.md): the failed
// attempt left nothing, and the next counts the version again under the migration lock, so a
// migrator that lost the race finds m applied and does not run it. Any other error is returned
// at once.
func (r runner) apply(ctx context.Context, db *sql.DB, m migration, log *slog.Logger) (bool, error) {
	for n := 1; ; n++ {
		ran, err := r.attempt(ctx, db, m)
		state, retry := retryable(err)
		switch {
		case err == nil:
			return ran, nil
		case !retry:
			return false, err
		case n == migrationAttempts:
			return false, fmt.Errorf("%d attempts: %w", n, err)
		}
		delay := r.retryDelay(n)
		log.WarnContext(ctx, "index: a migration conflicted with another schema change; it runs again from the start",
			"migration", m.name, "sqlstate", state, "attempt", n, "attempts", migrationAttempts, "delay", delay, "error", err)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return false, fmt.Errorf("%w; not run again: %w", err, context.Cause(ctx))
		case <-t.C:
		}
	}
}

// retryDelay is the pause after the failed attempt n (from 1) at a migration's transaction:
// firstRetryDelay doubled n-1 times, between half and one and a half of it at random, so that
// migrators that failed together do not try again together.
func (r runner) retryDelay(n int) time.Duration {
	if r.delay != nil {
		return r.delay(n)
	}
	d := firstRetryDelay << (n - 1)
	return d/2 + rand.N(d) //nolint:gosec // the jitter of a retry, not a security use
}

// createError is the error of the create of schema_migrations in an attempt.
type createError struct{ err error }

func (e *createError) Error() string { return e.err.Error() }
func (e *createError) Unwrap() error { return e.err }

// retryable returns the SQLSTATE of err, the error of one attempt at a migration's
// transaction, and whether a later attempt may pass: a transaction the database aborted
// (40001, from any statement, COMMIT included: on YugabyteDB DDL that conflicts with another
// schema change, also of another schema, yugabyte-db#21833), or a create of schema_migrations
// that finds the type of a table it does not see (42710: on YugabyteDB the catalog of a
// transaction that waited for the migration lock misses the table another migrator committed
// meanwhile). A 42710 of a migration file is that file's, and the next attempt would fail
// alike. The SQLSTATE is that of the driver's error type, by its method SQLState (pgconn's
// PgError), so this package imports no driver; SQLite's errors have none.
func retryable(err error) (string, bool) {
	var e interface{ SQLState() string }
	if !errors.As(err, &e) {
		return "", false
	}
	switch state := e.SQLState(); state {
	case serializationFailure:
		return state, true
	case duplicateObject:
		var ce *createError
		return state, errors.As(err, &ce)
	default:
		return state, false
	}
}

// attempt runs m in one transaction that holds the migration lock, unless schema_migrations
// records it already, and reports whether it ran m. The transaction is spelled out on one
// connection because database/sql cannot begin one with BEGIN IMMEDIATE.
func (r runner) attempt(ctx context.Context, db *sql.DB, m migration) (ran bool, err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, r.begin); err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			rollback(ctx, conn)
		}
	}()
	if r.lock != "" {
		if _, err := conn.ExecContext(ctx, r.lock, migrationLockID); err != nil {
			return false, err
		}
	}
	if _, err := conn.ExecContext(ctx, r.create); err != nil {
		return false, &createError{err}
	}
	var n int
	if err := conn.QueryRowContext(ctx, r.check, m.version).Scan(&n); err != nil {
		return false, err
	}
	if n == 0 {
		if _, err := conn.ExecContext(ctx, m.sql); err != nil {
			return false, err
		}
		if _, err := conn.ExecContext(ctx, r.record, m.version); err != nil {
			return false, err
		}
		if m.requires > 0 {
			if _, err := conn.ExecContext(ctx, r.require, m.requires, m.version); err != nil {
				return false, err
			}
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, err
	}
	return n == 0, nil
}

// rollback ends the transaction open on conn. When that fails, the connection is discarded,
// so that the pool never hands out a connection inside a transaction.
func rollback(ctx context.Context, conn *sql.Conn) {
	if _, err := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

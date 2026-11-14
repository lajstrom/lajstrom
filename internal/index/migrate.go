package index

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strings"
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

	// steps runs a migration one statement at a time, each in a transaction of its own, under
	// the migration lock held by a transaction on another connection (applySteps): CockroachDB,
	// whose DDL inside a transaction is not atomic.
	steps bool
}

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
	DialectCockroach: {
		begin:   "BEGIN",
		lock:    "SELECT pg_advisory_xact_lock($1)",
		exists:  "SELECT to_regclass('schema_migrations') IS NOT NULL",
		create:  "CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL)",
		check:   "SELECT count(*) FROM schema_migrations WHERE version = $1",
		record:  "INSERT INTO schema_migrations (version, applied_at) VALUES ($1, now())",
		require: "UPDATE schema_migrations SET requires = $1 WHERE version = $2",
		steps:   true,
	},
}

// MigrateOptions are the options of ApplyMigrations.
type MigrateOptions struct {
	// Breaking lets the run apply a migration that requires a version above the schema's
	// version before the run: binaries that know only that version may be serving on it, and
	// would not run on the schema after it. Without it, ApplyMigrations applies nothing then
	// and returns ErrBreakingMigration.
	Breaking bool
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
// once. On CockroachDB a migration is not one transaction (applySteps). On error the report
// has the versions applied before the failing one.
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
	for _, m := range pending {
		ran, err := r.apply(ctx, db, m)
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

// apply runs m in one transaction that holds the migration lock, unless schema_migrations
// records it already, and reports whether it ran m. The transaction is spelled out on one
// connection because database/sql cannot begin one with BEGIN IMMEDIATE.
func (r runner) apply(ctx context.Context, db *sql.DB, m migration) (ran bool, err error) {
	if r.steps {
		return r.applySteps(ctx, db, m)
	}
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
		return false, err
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

// stepAttempts is how many times in all applySteps runs a statement that CockroachDB aborts
// (40001), with a pause of stepRetryDelay times the attempt before each next one.
const (
	stepAttempts   = 5
	stepRetryDelay = 100 * time.Millisecond
)

// applySteps is apply on CockroachDB (index-schema.md, Migrations). There an UPDATE does not see
// a column added earlier in its transaction, and the commit of a transaction with DDL can fail
// after its other statements committed (XXA00), so a migration is not one transaction: each of
// its statements (statements) runs in a transaction of its own, and its version is recorded
// after the last one. The migration lock is held by a transaction on a second connection from
// before the check of schema_migrations until after the record, so that it spans those
// transactions without a session lock. A migration that stops half-way is not recorded, and
// the next run runs it again from its first statement: every statement of a cockroach file
// may find its work done (IF NOT EXISTS, ON CONFLICT DO NOTHING). A statement that CockroachDB
// aborts (40001) runs again, up to stepAttempts times in all. The COMMIT of the lock's
// transaction is checked: when it fails, the lock may have ended before the migration did.
func (r runner) applySteps(ctx context.Context, db *sql.DB, m migration) (ran bool, err error) {
	if n := db.Stats().MaxOpenConnections; n == 1 {
		return false, errors.New("on CockroachDB a migration needs two connections, and the pool has one: set pool_max_conns to 2 or more")
	}
	lock, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = lock.Close() }()
	if _, err := lock.ExecContext(ctx, r.begin); err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			rollback(ctx, lock)
		}
	}()
	if _, err := lock.ExecContext(ctx, r.lock, migrationLockID); err != nil {
		return false, fmt.Errorf("the migration lock: %w", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	exec := func(query string, args ...any) error {
		return retryAborted(ctx, func() error {
			_, err := conn.ExecContext(ctx, query, args...)
			return err
		})
	}
	if err := exec(r.create); err != nil {
		return false, err
	}
	var n int
	if err := conn.QueryRowContext(ctx, r.check, m.version).Scan(&n); err != nil {
		return false, err
	}
	if n == 0 {
		if err := r.runSteps(m, exec); err != nil {
			return false, err
		}
	}
	if _, err := lock.ExecContext(ctx, "COMMIT"); err != nil {
		return false, fmt.Errorf("the migration lock ended before the migration did: %w", err)
	}
	return n == 0, nil
}

// runSteps runs the statements of m one by one with exec, each in a transaction of its own,
// then records m's version and its requires (applySteps).
func (r runner) runSteps(m migration, exec func(query string, args ...any) error) error {
	stmts := statements(m.sql)
	for i, stmt := range stmts {
		if err := exec(stmt); err != nil {
			return fmt.Errorf("statement %d of %d, after which the migration stopped; the next run runs it again from the first: %w",
				i+1, len(stmts), err)
		}
	}
	if err := exec(r.record, m.version); err != nil {
		return err
	}
	if m.requires > 0 {
		return exec(r.require, m.requires, m.version)
	}
	return nil
}

// retryAborted runs fn, and again while it fails with SQLSTATE 40001 (the error's SQLState
// method, which pgconn's PgError has), up to stepAttempts times in all, as long as ctx lasts.
func retryAborted(ctx context.Context, fn func() error) error {
	for attempt := 1; ; attempt++ {
		err := fn()
		var e interface{ SQLState() string }
		if err == nil || attempt == stepAttempts || !errors.As(err, &e) || e.SQLState() != "40001" {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w; not run again: %w", err, context.Cause(ctx))
		case <-time.After(time.Duration(attempt) * stepRetryDelay):
		}
	}
}

// statements returns the statements of a migration file in order: its lines but the comment
// lines and the empty ones, cut after each line that ends with a semicolon. A cockroach file
// ends no line inside a statement with a semicolon and comments only on lines of their own
// (TestCockroachMigrations).
func statements(file string) []string {
	var out []string
	var b strings.Builder
	for line := range strings.Lines(file) {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		b.WriteString(line)
		if strings.HasSuffix(t, ";") {
			out = append(out, strings.TrimSpace(b.String()))
			b.Reset()
		}
	}
	if rest := strings.TrimSpace(b.String()); rest != "" {
		out = append(out, rest)
	}
	return out
}

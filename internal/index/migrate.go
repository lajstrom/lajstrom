package index

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
)

// migrationLockID is the key of the Postgres advisory lock that serialises migration runners:
// "lajstrom" in ASCII.
const migrationLockID int64 = 0x6c616a7374726f6d

// runner holds the statements of the migration runner that differ between dialects.
type runner struct {
	begin  string // starts the transaction; on SQLite it also takes the write lock
	lock   string // takes the migration lock inside the transaction; "" when begin does
	create string // creates schema_migrations when it does not exist
	check  string // counts the rows of one version
	record string // records one version as applied now
}

var runners = map[string]runner{
	DialectSQLite: {
		// IMMEDIATE takes the write lock at once: a second runner waits for it (busy
		// timeout) instead of failing when both try to upgrade a read lock.
		begin:  "BEGIN IMMEDIATE",
		create: "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)",
		check:  "SELECT count(*) FROM schema_migrations WHERE version = ?",
		record: "INSERT INTO schema_migrations (version, applied_at) VALUES (?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))",
	},
	DialectPostgres: {
		begin:  "BEGIN",
		lock:   "SELECT pg_advisory_xact_lock($1)",
		create: "CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL)",
		check:  "SELECT count(*) FROM schema_migrations WHERE version = $1",
		record: "INSERT INTO schema_migrations (version, applied_at) VALUES ($1, now())",
	},
}

// ApplyMigrations applies the embedded migrations of dialect that db has not applied yet, in
// version order, and returns the versions it applied: empty when the schema is current.
//
// Each migration runs in a transaction of its own that also records its version in
// schema_migrations (created when missing). The transaction first takes the migration lock,
// pg_advisory_xact_lock on Postgres and BEGIN IMMEDIATE on SQLite, and checks
// schema_migrations under it, so replicas that start at once apply each migration exactly
// once. On error it returns the versions applied before the failing one.
func ApplyMigrations(ctx context.Context, db *sql.DB, dialect string) (applied []int, err error) {
	ms, err := migrations(dialect)
	if err != nil {
		return nil, err
	}
	r := runners[dialect]
	applied = []int{}
	for _, m := range ms {
		ran, err := r.apply(ctx, db, m)
		if err != nil {
			return applied, fmt.Errorf("index: migration %s: %w", m.name, err)
		}
		if ran {
			applied = append(applied, m.version)
		}
	}
	return applied, nil
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

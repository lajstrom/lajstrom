package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/postgres"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// store is the index as the commands use it: index.Store, and what both engines add to it, the
// readiness check and the migration report.
type store interface {
	index.Store
	Ping(ctx context.Context) error
	ApplyMigrations(ctx context.Context, opts index.MigrateOptions) (index.MigrationReport, error)
	AppliedMigrations(ctx context.Context) ([]int, error)
}

// openStore opens the index database of database.driver and database.dsn. It does not migrate.
// Its errors never contain the DSN; on SQLite they start with the file (inFile).
func openStore(ctx context.Context, cfg config.Database) (store, error) {
	switch cfg.Driver {
	case index.DialectSQLite:
		s, err := sqlite.Open(ctx, cfg.DSN)
		if err != nil {
			return nil, inFile(cfg, err)
		}
		return s, nil
	case index.DialectPostgres:
		s, err := postgres.Open(ctx, cfg.DSN)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	return nil, fmt.Errorf("database.driver %q is not sqlite or postgres", cfg.Driver)
}

// openMigrated is openStore of the configuration for the commands that need the schema and do
// not migrate it, admin and reindex: on a database that lacks a migration of this binary it
// fails with the command that applies it, instead of a query's "no such table" (#575); on a
// host whose clock is too far off the database's it fails too (checkClock). A SQLite file that
// does not exist is reported, not created (#719): such a command run in another directory, or
// with another configuration, than the server's would make an empty database there, and
// migrate after it would migrate the wrong file.
func (a *app) openMigrated(ctx context.Context) (store, error) {
	if err := sqliteExists(a.cfg.Database); err != nil {
		return nil, err
	}
	st, err := openStore(ctx, a.cfg.Database)
	if err != nil {
		return nil, err
	}
	if err := checkSchema(ctx, st, a.cfg.Database.Driver); err != nil {
		return nil, errors.Join(inFile(a.cfg.Database, err), st.Close())
	}
	if err := checkClock(ctx, st, a.clock, a.log); err != nil {
		return nil, errors.Join(err, st.Close())
	}
	return st, nil
}

// sqliteExists fails when database.dsn names a SQLite file that does not exist, naming the
// absolute path; an in-memory database and postgres pass. sqlite.Open would create the file:
// modernc.org/sqlite drops the query of a file name that is not a file: URI, mode=rw with it, so
// a stat is what checks both forms. A file that cannot be stat'ed otherwise is left to the open.
func sqliteExists(d config.Database) error {
	file := d.File()
	if file == "" {
		return nil
	}
	if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no database at %s (database.dsn): run lajstrom migrate there, "+
			"or point --config/database.dsn at the server's", file)
	}
	return nil
}

// inFile prefixes err with the SQLite file of d and the key, when d names one: SQLite's own
// errors, "unable to open database file (14)", "attempt to write a readonly database (1544)",
// name neither. A relative DSN is resolved against the working directory, as SQLite does.
func inFile(d config.Database, err error) error {
	if file := d.File(); file != "" {
		return fmt.Errorf("%s (database.dsn): %w", file, err)
	}
	return err
}

// databaseFile is the attribute of serve's first line that names the SQLite file of d,
// absolute; nothing for postgres, whose DSN is never logged, or an in-memory database.
func databaseFile(d config.Database) slog.Attr {
	if file := d.File(); file != "" {
		return slog.String("database.file", file)
	}
	return slog.Attr{}
}

// checkClock compares the clock of this host with the database's (skewOf) before a command of
// the CLI works on the index: its claims, reindex runs and tokens are compared with times that
// the replicas wrote, so its clock must agree with theirs as a replica's does
// (docs/spec/guarantees.md, clocks agree; docs/operations.md, Clocks). Past clockWarn it logs a
// warning; past clockFail it refuses, before anything is written, with an error that names the
// skew. A database whose clock it cannot read is an error too.
func checkClock(ctx context.Context, st clockReader, now func() time.Time, log *slog.Logger) error {
	skew, db, err := skewOf(ctx, st, now)
	if err != nil {
		return fmt.Errorf("read the database's clock: %w", err)
	}
	switch abs := skew.Abs(); {
	case abs > clockFail:
		return fmt.Errorf("the clock of this host is %s the database's; the CLI and the replicas must agree within %ds: "+
			"run NTP on this host and the database's, then run the command again", offBy(skew), clockFail/time.Second)
	case abs > clockWarn:
		log.WarnContext(ctx, "the clock of this host is more than 10s off the database's; run NTP",
			"skew", skew.Round(time.Millisecond).String(), "database", db.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// migrateHint is what to do about a database without the schema of this binary.
const migrateHint = "run lajstrom migrate, or start lajstrom serve, which migrates it"

// schemaReader is the part of store that checkSchema reads.
type schemaReader interface {
	Ping(ctx context.Context) error
	AppliedMigrations(ctx context.Context) ([]int, error)
}

// checkSchema returns an error with migrateHint when st has no schema, or one older than the
// embedded migrations of driver. AppliedMigrations fails before the first migration, which
// makes schema_migrations: when it fails and the database answers, the database was never
// migrated. A schema newer than the binary is left to the queries, as serve leaves it.
func checkSchema(ctx context.Context, st schemaReader, driver string) error {
	have, err := st.AppliedMigrations(ctx)
	if err != nil {
		if perr := st.Ping(ctx); perr != nil {
			return perr
		}
		return fmt.Errorf("the index database is not migrated: %s (%w)", migrateHint, err)
	}
	known, err := index.MigrationVersions(driver)
	if err != nil {
		return err
	}
	switch {
	case len(have) == 0:
		return errors.New("the index database is not migrated: " + migrateHint)
	case len(known) > 0 && have[len(have)-1] < known[len(known)-1]:
		return fmt.Errorf("the index schema is at version %04d, older than this binary's %04d: %s",
			have[len(have)-1], known[len(known)-1], migrateHint)
	}
	return nil
}

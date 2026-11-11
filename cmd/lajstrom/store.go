package main

import (
	"context"
	"errors"
	"fmt"

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
// Its errors never contain the DSN.
func openStore(ctx context.Context, cfg config.Database) (store, error) {
	switch cfg.Driver {
	case index.DialectSQLite:
		s, err := sqlite.Open(ctx, cfg.DSN)
		if err != nil {
			return nil, err
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

// openMigrated is openStore for the commands that need the schema and do not migrate it, admin
// and reindex: on a database that lacks a migration of this binary it fails with the command
// that applies it, instead of a query's "no such table" (#575).
func openMigrated(ctx context.Context, cfg config.Database) (store, error) {
	st, err := openStore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := checkSchema(ctx, st, cfg.Driver); err != nil {
		return nil, errors.Join(err, st.Close())
	}
	return st, nil
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

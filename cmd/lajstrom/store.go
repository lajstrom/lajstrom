package main

import (
	"context"
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

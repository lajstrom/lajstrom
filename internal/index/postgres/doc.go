// Package postgres implements index.Store on PostgreSQL with pgx through database/sql
// (github.com/jackc/pgx/v5/stdlib). The SQL is the one both engines share
// (internal/index/internal/sqlstore); text columns are COLLATE "C", so that order and
// comparison match SQLite. The conformance suite internal/index/storetest runs when
// LAJSTROM_TEST_POSTGRES_DSN is set, each test in a schema of its own.
package postgres

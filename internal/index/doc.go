// Package index defines the index database: the Store interface, one model per table, sentinel
// errors, and the embedded migrations with their runner (ApplyMigrations). Only index
// implementations talk to the database. See docs/spec/index-schema.md and ADR 0006.
//
// Implementations: internal/index/sqlite and internal/index/postgres, which share one SQL
// implementation (internal/index/internal/sqlstore) and pass the conformance suite
// internal/index/storetest. Migrations are migrations/<dialect>/NNNN_<name>.sql, the same
// versions for both dialects. Reindex comes with issue #29, reconcile with issue #30.
package index

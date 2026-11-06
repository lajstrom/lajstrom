// Package index defines the index database: the Store interface, one model per table, sentinel
// errors, and (later) the embedded migrations and the reindex worker. Only index
// implementations talk to the database. See docs/spec/index-schema.md and ADR 0006.
//
// Implementations: internal/index/sqlite and internal/index/postgres with the conformance suite
// internal/index/storetest (issue #10); reindex in issue #29, reconcile in issue #30.
package index

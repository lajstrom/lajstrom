// Package sqlite implements index.Store on SQLite with modernc.org/sqlite (no cgo): Open adds
// the connection parameters (WAL, busy timeout, foreign keys, immediate transactions) to the
// DSN, and the SQL is the one both engines share (internal/index/internal/sqlstore). Timestamps
// are UTC RFC 3339 text with microseconds. The conformance suite internal/index/storetest
// runs on an in-memory database.
package sqlite

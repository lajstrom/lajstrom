package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	msqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/internal/sqlstore"
)

// Store is the index on SQLite. It implements index.Store; Ping checks that the database
// answers, ApplyMigrations and AppliedMigrations report on the schema, and Close closes the
// database.
type Store struct {
	*sqlstore.Store
}

var _ index.Store = (*Store)(nil)

// maxConns is the size of the connection pool of a database file: WAL lets readers run
// beside the one writer.
const maxConns = 4

// Parameters the store adds to every DSN (modernc.org/sqlite's validated shorthand keys).
// Each connection gets them when it opens.
var (
	// tunable are added when the DSN does not set them.
	tunable = [][2]string{
		{"_busy_timeout", "5000"}, // milliseconds a writer waits for the lock before SQLITE_BUSY
		{"_journal_mode", "WAL"},  // readers do not block the writer
		{"_synchronous", "NORMAL"},
	}
	// forced override the DSN: the cascades need foreign keys, and every transaction is a
	// write that takes the lock when it begins, so that two writers never deadlock on a lock
	// upgrade.
	forced = [][2]string{
		{"_foreign_keys", "1"},
		{"_txlock", "immediate"},
	}
)

var dialect = sqlstore.Dialect{
	Name:          index.DialectSQLite,
	TimeText:      true,
	IsUnique:      isUnique,
	IsForeignKey:  isForeignKey,
	IsUnavailable: isUnavailable,
	Clock:         "SELECT strftime('%Y-%m-%dT%H:%M:%fZ', 'now')", // the host's clock, in milliseconds
}

// Open opens the SQLite database of dsn, a file name or a file: URI (docs/configuration.md):
// file:/var/lib/lajstrom/lajstrom.db, or file:test?mode=memory&cache=shared in tests. It adds
// _busy_timeout=5000, _journal_mode=WAL and _synchronous=NORMAL unless the DSN sets them, and
// always _foreign_keys=1 and _txlock=immediate. An in-memory database lives in its one
// connection, so the pool has one connection then. Open checks that the database opens; it
// does not migrate (Migrate does).
func Open(ctx context.Context, dsn string) (*Store, error) {
	full, memory, err := withParams(dsn)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", full)
	if err != nil {
		return nil, fmt.Errorf("index: sqlite: %w", err)
	}
	if memory {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxOpenConns(maxConns)
		db.SetMaxIdleConns(maxConns)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("index: sqlite: open: %w", err)
	}
	return &Store{sqlstore.New(db, dialect)}, nil
}

// withParams returns dsn with the store's parameters, and whether it names an in-memory
// database.
func withParams(dsn string) (string, bool, error) {
	name, query, _ := strings.Cut(dsn, "?")
	q, err := url.ParseQuery(query)
	if err != nil {
		return "", false, fmt.Errorf("index: sqlite: the query of database.dsn: %w", err)
	}
	for _, p := range tunable {
		if !q.Has(p[0]) {
			q.Set(p[0], p[1])
		}
	}
	for _, p := range forced {
		q.Set(p[0], p[1])
	}
	q.Del("_fk") // the alias of _foreign_keys, which would win over it
	memory := name == ":memory:" || strings.HasPrefix(name, "file::memory:") || q.Get("mode") == "memory"
	return name + "?" + q.Encode(), memory, nil
}

// isUnique reports whether err is SQLITE_CONSTRAINT_UNIQUE or SQLITE_CONSTRAINT_PRIMARYKEY.
func isUnique(err error) bool {
	code, ok := errorCode(err)
	return ok && (code == sqlite3.SQLITE_CONSTRAINT_UNIQUE || code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
}

// isForeignKey reports whether err is SQLITE_CONSTRAINT_FOREIGNKEY.
func isForeignKey(err error) bool {
	code, ok := errorCode(err)
	return ok && code == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY
}

// isUnavailable reports whether err is SQLITE_BUSY, the lock that _busy_timeout waited for
// in vain, or SQLITE_CANTOPEN, a database file (or its journal) that cannot be opened, with
// any extended code: the database did not answer, and a later attempt may pass.
func isUnavailable(err error) bool {
	code, ok := errorCode(err)
	primary := code & 0xff
	return ok && (primary == sqlite3.SQLITE_BUSY || primary == sqlite3.SQLITE_CANTOPEN)
}

// errorCode returns the extended result code of a driver error.
func errorCode(err error) (int, bool) {
	var e *msqlite.Error
	if !errors.As(err, &e) {
		return 0, false
	}
	return e.Code(), true
}

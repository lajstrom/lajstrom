package sqlstore

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Dialect is what differs between the engines outside the migration files: placeholders, the
// encoding of timestamps, and how the driver reports constraint violations. The SQL of the
// store is written once, with ? placeholders.
type Dialect struct {
	// Name is index.DialectSQLite or index.DialectPostgres; it selects the migrations.
	Name string
	// Numbered rewrites the ? placeholders to $1, $2, … (Postgres).
	Numbered bool
	// TimeText stores timestamps as UTC RFC 3339 text with six fractional digits (SQLite)
	// instead of passing time.Time to the driver (Postgres).
	TimeText bool
	// IsUnique reports whether err is a violation of a unique constraint or primary key.
	IsUnique func(err error) bool
	// IsForeignKey reports whether err is a violation of a foreign key: the row refers to a
	// row that does not exist.
	IsForeignKey func(err error) bool
	// IsAborted reports whether err says that the database aborted a transaction that may
	// pass when run again, which Store.Tx then runs again under index.WithRepeatableRead, at
	// repeatable read (Postgres: a serialization failure or a deadlock, 40001 or 40P01). nil
	// when every writing transaction holds the database's write lock from its start (SQLite,
	// BEGIN IMMEDIATE): such a Tx runs as any other, once.
	IsAborted func(err error) bool
	// IsUnavailable reports whether err says that the database did not answer
	// (index.ErrUnavailable): a connection that failed or was ended, a database that is
	// locked or cannot be opened. nil means never.
	IsUnavailable func(err error) bool
	// XactLock is the statement that takes a lock for the rest of the transaction, with the
	// lock's key as its one parameter, and lifts the server's timeout of idle transactions for
	// the rest of it, in one round trip: the referrers lock idles while its function talks to
	// the registry (Postgres: set_config of idle_in_transaction_session_timeout, local, and
	// pg_advisory_xact_lock). Empty when every writing transaction holds the database's write
	// lock from its start (SQLite, BEGIN IMMEDIATE).
	XactLock string
	// PackageLock is the statement that takes a lock for the rest of the transaction, with the
	// lock's key as its one parameter, which UpsertPackage takes on a package path before it
	// inserts a package there, so that the writers of one new package insert one after the
	// other (YugabyteDB: pg_advisory_xact_lock, #629). Empty when the database resolves two
	// such inserts itself: Postgres with speculative insertion, SQLite with its write lock.
	PackageLock string
	// Clock is the query that returns the time of the database's clock, as a timestamp the
	// store decodes (Postgres: clock_timestamp(); SQLite: 'now' as text, the host's clock).
	Clock string
}

// wrap returns err, an error of the driver about what ("begin", "repos raw/scratch"), with
// the context of the store: wrapping index.ErrUnavailable when IsUnavailable says so.
func (d *Dialect) wrap(err error, what string) error {
	if d.IsUnavailable != nil && d.IsUnavailable(err) {
		return fmt.Errorf("%w: %s: %w", index.ErrUnavailable, what, err)
	}
	return fmt.Errorf("index: %s: %w", what, err)
}

// timeLayout is the text form of a timestamp on SQLite: fixed width, so that text order is
// time order, and exact, because timestamps are truncated to microseconds.
const timeLayout = "2006-01-02T15:04:05.000000Z"

// Rebind returns query with its ? placeholders in the dialect's syntax. The store's SQL has no
// ? inside literals.
func (d *Dialect) Rebind(query string) string {
	if !d.Numbered {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := range len(query) {
		if query[i] != '?' {
			b.WriteByte(query[i])
			continue
		}
		n++
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(n))
	}
	return b.String()
}

// EncodeTime returns the value that stores t: Normalize(t) as text on SQLite, as time.Time
// on Postgres.
func (d *Dialect) EncodeTime(t time.Time) any {
	t = Normalize(t)
	if d.TimeText {
		return t.Format(timeLayout)
	}
	return t
}

// encodeNullTime is EncodeTime for a nullable column: nil is NULL.
func (d *Dialect) encodeNullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return d.EncodeTime(*t)
}

// decodeTime returns the timestamp a driver read, in UTC: text from SQLite, time.Time from
// Postgres. It needs no dialect, so the scanners take none.
func decodeTime(v any) (time.Time, error) {
	switch v := v.(type) {
	case time.Time:
		return v.UTC(), nil
	case string:
		t, err := time.Parse(time.RFC3339Nano, v)
		return t.UTC(), err
	case []byte:
		t, err := time.Parse(time.RFC3339Nano, string(v))
		return t.UTC(), err
	}
	return time.Time{}, fmt.Errorf("timestamp of type %T", v)
}

// Normalize returns t in UTC, truncated to microseconds: what both engines store exactly.
func Normalize(t time.Time) time.Time { return t.Truncate(time.Microsecond).UTC() }

// stamp returns Normalize(*t), or now when *t is zero, and stores it in *t.
func stamp(t *time.Time, now time.Time) time.Time {
	if t.IsZero() {
		*t = now
	}
	*t = Normalize(*t)
	return *t
}

// stampNull is stamp for a nullable timestamp: nil stays nil.
func stampNull(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	n := Normalize(*t)
	return &n
}

// timeDest scans a NOT NULL timestamp.
type timeDest struct{ t *time.Time }

// Scan implements sql.Scanner.
func (s timeDest) Scan(src any) error {
	if src == nil {
		return errors.New("NULL in a timestamp column that is NOT NULL")
	}
	t, err := decodeTime(src)
	*s.t = t
	return err
}

// nullTimeDest scans a nullable timestamp: NULL is nil.
type nullTimeDest struct{ t **time.Time }

// Scan implements sql.Scanner.
func (s nullTimeDest) Scan(src any) error {
	if src == nil {
		*s.t = nil
		return nil
	}
	t, err := decodeTime(src)
	*s.t = &t
	return err
}

// rawDest scans a nullable json column into a json.RawMessage: NULL is nil.
type rawDest struct{ raw *json.RawMessage }

// Scan implements sql.Scanner.
func (s rawDest) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*s.raw = nil
	case string:
		*s.raw = json.RawMessage(v)
	case []byte:
		*s.raw = bytes.Clone(v)
	default:
		return fmt.Errorf("json of type %T", src)
	}
	return nil
}

// jsonDest scans a nullable json column into a Go value: NULL is the zero value.
type jsonDest[T any] struct{ v *T }

// Scan implements sql.Scanner.
func (s jsonDest[T]) Scan(src any) error {
	var raw json.RawMessage
	if err := (rawDest{&raw}).Scan(src); err != nil {
		return err
	}
	var v T
	if raw != nil {
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
	}
	*s.v = v
	return nil
}

// textDest scans a nullable text column into a string: NULL is "".
type textDest struct{ s *string }

// Scan implements sql.Scanner.
func (d textDest) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d.s = ""
	case string:
		*d.s = v
	case []byte:
		*d.s = string(v)
	default:
		return fmt.Errorf("text of type %T", src)
	}
	return nil
}

// blobDest scans a blob column: an empty blob is nil.
type blobDest struct{ b *[]byte }

// Scan implements sql.Scanner.
func (d blobDest) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d.b = nil
	case []byte:
		*d.b = nil
		if len(v) > 0 {
			*d.b = bytes.Clone(v)
		}
	case string:
		*d.b = nil
		if v != "" {
			*d.b = []byte(v)
		}
	default:
		return fmt.Errorf("blob of type %T", src)
	}
	return nil
}

// rawArg is the value of a nullable json column: empty is NULL.
func rawArg(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// attrsArg marshals attributes for a nullable json column: nil is NULL.
func attrsArg(m map[string]string) any {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(m) // a map of strings always marshals
	return string(b)
}

// scopesArg marshals token scopes for a nullable json column: nil is NULL.
func scopesArg(s []string) any {
	if s == nil {
		return nil
	}
	b, _ := json.Marshal(s) // a slice of strings always marshals
	return string(b)
}

// textArg is the value of a nullable text column: "" is NULL.
func textArg(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// blobArg is the value of a blob column, which is NOT NULL: nil is stored empty.
func blobArg(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// nullIDDest scans a nullable foreign key: NULL is nil.
type nullIDDest struct{ id **int64 }

// Scan implements sql.Scanner.
func (d nullIDDest) Scan(src any) error {
	var n sql.NullInt64
	if err := n.Scan(src); err != nil {
		return err
	}
	*d.id = nil
	if n.Valid {
		*d.id = &n.Int64
	}
	return nil
}

// idArg is the value of a nullable foreign key: nil is NULL.
func idArg(id *int64) any {
	if id == nil {
		return nil
	}
	return *id
}

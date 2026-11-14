package index

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMigrationRetry runs the Postgres runner on a fake database that fails chosen statements
// with a SQLSTATE (#616): a migration whose transaction the database aborted (40001), or whose
// create of schema_migrations found the table of another migrator (42710), runs again from the
// start, counts its version again under the lock and is applied once; every other error ends
// the run at once; the attempts are bounded and each retry is logged.
func TestMigrationRetry(t *testing.T) {
	ms := []migration{
		{version: 1, name: "postgres/0001_a.sql", sql: "CREATE TABLE a (id integer)"},
		{version: 2, name: "postgres/0002_b.sql", sql: "CREATE TABLE b (id integer)"},
		{version: 3, name: "postgres/0003_c.sql", sql: "CREATE TABLE c (id integer)"},
	}
	r := runners[DialectPostgres]
	create, record := r.create, r.record
	tests := []retryCase{
		{
			name:        "42710 at the create of schema_migrations",
			fail:        failOnce(create, 1, "42710"),
			wantApplied: []int{1, 2, 3}, wantRan: []int{1, 2, 3}, wantRetries: 1,
		},
		{
			name: "42710 at the create: another migrator applied the version meanwhile",
			fail: func(db *fakeMigrationDB, q string, call int) error {
				if q == create && call == 1 {
					db.commitByOther(1)
					return &fakePgError{code: "42710", msg: `type "schema_migrations" already exists`}
				}
				return nil
			},
			wantApplied: []int{2, 3}, wantRan: []int{2, 3}, wantRetries: 1,
		},
		{
			name:        "40001 at a migration file",
			fail:        failOnce(ms[1].sql, 1, "40001"),
			wantApplied: []int{1, 2, 3}, wantRan: []int{1, 2, 3}, wantRetries: 1,
		},
		{
			name:        "40001 at the commit, twice",
			fail:        failCalls("COMMIT", "40001", 2, 3),
			wantApplied: []int{1, 2, 3}, wantRan: []int{1, 2, 3}, wantRetries: 2,
		},
		{
			name:        "40001 at the record",
			fail:        failOnce(record, 3, "40001"),
			wantApplied: []int{1, 2, 3}, wantRan: []int{1, 2, 3}, wantRetries: 1,
		},
		{
			name:        "40001 on every attempt: bounded",
			fail:        failCalls(ms[0].sql, "40001", 1, 2, 3, 4, 5, 6),
			wantApplied: []int{}, wantErr: "40001", wantRetries: migrationAttempts - 1,
		},
		{
			name:        "42710 from a migration file is not retried",
			fail:        failOnce(ms[1].sql, 1, "42710"),
			wantApplied: []int{1}, wantRan: []int{1}, wantErr: "42710",
		},
		{
			name:        "42P07 is not retried",
			fail:        failOnce(ms[0].sql, 1, "42P07"),
			wantApplied: []int{}, wantErr: "42P07",
		},
		{
			name:        "23505 is not retried",
			fail:        failOnce(record, 1, "23505"),
			wantApplied: []int{}, wantErr: "23505",
		},
		{
			name: "an error without SQLSTATE is not retried",
			fail: func(_ *fakeMigrationDB, q string, call int) error {
				if q == create && call == 1 {
					return errors.New("database is locked")
				}
				return nil
			},
			wantApplied: []int{}, wantErr: "plain",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, r, ms) })
	}
}

// retryCase is a case of TestMigrationRetry.
type retryCase struct {
	name string
	// fail returns the error of the statement q, the call-th statement with that text in the run
	// (from 1), or nil; the database is passed for what another migrator does.
	fail        func(db *fakeMigrationDB, q string, call int) error
	wantApplied []int  // reported by the run
	wantRan     []int  // migration files whose transaction committed, each once
	wantErr     string // a SQLSTATE the error has, or "plain" for an error without one; "" none
	wantRetries int    // warnings logged
}

// run applies ms with the runner r on a fake database that fails as tt.fail says, and checks
// what the run reports, what the database has, and the retries.
func (tt retryCase) run(t *testing.T, r runner, ms []migration) {
	fake := &fakeMigrationDB{fail: tt.fail, calls: map[string]int{}, ran: map[int]int{}}
	db := sql.OpenDB(fake)
	t.Cleanup(func() { _ = db.Close() })
	log := &recordingHandler{}
	var delays []int
	r.delay = func(n int) time.Duration { delays = append(delays, n); return 0 }
	rep, err := r.applyAll(t.Context(), db, ms, MigrateOptions{Log: slog.New(log)})
	tt.checkErr(t, err)
	if !slices.Equal(rep.Applied, tt.wantApplied) {
		t.Errorf("Applied = %v, want %v", rep.Applied, tt.wantApplied)
	}
	if got := fake.committedFiles(); !slices.Equal(got, tt.wantRan) {
		t.Errorf("committed migration files %v, want %v", got, tt.wantRan)
	}
	if fake.recordedTwice {
		t.Error("a version was recorded twice")
	}
	if got := log.count("sqlstate"); got != tt.wantRetries || len(delays) != tt.wantRetries {
		t.Errorf("%d retries logged, %d paused, want %d", got, len(delays), tt.wantRetries)
	}
	if fake.open > 0 {
		t.Errorf("%d transactions left open", fake.open)
	}
}

// checkErr checks the error of the run: none, one without SQLSTATE, or one with the SQLSTATE
// wanted, which counts the attempts when they were all made.
func (tt retryCase) checkErr(t *testing.T, err error) {
	t.Helper()
	switch {
	case tt.wantErr == "" && err != nil:
		t.Fatalf("applyAll: %v", err)
	case tt.wantErr == "plain" && (err == nil || sqlStateOf(err) != ""):
		t.Fatalf("applyAll: %v, want the error without SQLSTATE", err)
	case tt.wantErr != "" && tt.wantErr != "plain" && sqlStateOf(err) != tt.wantErr:
		t.Fatalf("applyAll: %v, want SQLSTATE %s", err, tt.wantErr)
	case tt.wantRetries == migrationAttempts-1 && !strings.Contains(err.Error(), "5 attempts"):
		t.Errorf("the error after the last attempt %q does not count them", err)
	}
}

// TestMigrationRetryCancel checks that a retry waits no longer than its context: the error is
// the attempt's, with the context's cause.
func TestMigrationRetryCancel(t *testing.T) {
	ms := []migration{{version: 1, name: "postgres/0001_a.sql", sql: "CREATE TABLE a (id integer)"}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fake := &fakeMigrationDB{calls: map[string]int{}, ran: map[int]int{}, fail: func(_ *fakeMigrationDB, q string, _ int) error {
		if q == ms[0].sql {
			cancel()
			return &fakePgError{code: "40001", msg: "could not serialize access due to concurrent update"}
		}
		return nil
	}}
	db := sql.OpenDB(fake)
	t.Cleanup(func() { _ = db.Close() })
	r := runners[DialectPostgres]
	r.delay = func(int) time.Duration { return time.Hour }
	done := make(chan error, 1)
	go func() {
		_, err := r.applyAll(ctx, db, ms, MigrateOptions{Log: slog.New(&recordingHandler{})})
		done <- err
	}()
	select {
	case err := <-done:
		if sqlStateOf(err) != "40001" || !errors.Is(err, context.Canceled) {
			t.Errorf("applyAll: %v, want the 40001 and context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the retry did not stop with its context")
	}
	if n := fake.calls[ms[0].sql]; n != 1 {
		t.Errorf("the migration ran %d times, want 1", n)
	}
}

// TestRetryDelay checks the pauses between the attempts at a migration's transaction: about
// firstRetryDelay, then doubling, each between half and one and a half of its value.
func TestRetryDelay(t *testing.T) {
	var r runner
	for n := 1; n < migrationAttempts; n++ {
		base := firstRetryDelay << (n - 1)
		for range 50 {
			if d := r.retryDelay(n); d < base/2 || d >= base/2+base {
				t.Fatalf("retryDelay(%d) = %v, want [%v, %v)", n, d, base/2, base/2+base)
			}
		}
	}
}

// failOnce fails the call-th statement q of a run with code.
func failOnce(q string, call int, code string) func(*fakeMigrationDB, string, int) error {
	return failCalls(q, code, call)
}

// failCalls fails the statements q of a run whose number is among calls with code.
func failCalls(q, code string, calls ...int) func(*fakeMigrationDB, string, int) error {
	return func(_ *fakeMigrationDB, got string, call int) error {
		if got == q && slices.Contains(calls, call) {
			return &fakePgError{code: code, msg: "injected"}
		}
		return nil
	}
}

// sqlStateOf returns the SQLSTATE of err, or "".
func sqlStateOf(err error) string {
	var e *fakePgError
	if errors.As(err, &e) {
		return e.code
	}
	return ""
}

// fakePgError is an error of a server, with a SQLSTATE as pgconn.PgError reports it.
type fakePgError struct{ code, msg string }

func (e *fakePgError) Error() string    { return "ERROR: " + e.msg + " (SQLSTATE " + e.code + ")" }
func (e *fakePgError) SQLState() string { return e.code }

// fakeMigrationDB is a database that knows the statements of the Postgres runner and nothing
// else: schema_migrations, the versions committed in it, and which migration files committed.
// A transaction is the statements between BEGIN and COMMIT or ROLLBACK on one connection; after
// an error it is aborted, as on Postgres, and every statement but ROLLBACK fails.
type fakeMigrationDB struct {
	fail func(db *fakeMigrationDB, q string, call int) error

	mu            sync.Mutex
	calls         map[string]int // statements run, by text
	table         bool           // schema_migrations exists
	versions      []int          // committed in schema_migrations
	ran           map[int]int    // migration files committed (with their record), by version
	recordedTwice bool
	open          int // transactions begun and not ended
}

// commitByOther commits version v as another migrator would; fake.mu is held.
func (f *fakeMigrationDB) commitByOther(v int) {
	f.table = true
	f.versions = append(f.versions, v)
}

// committedFiles returns the versions whose migration file committed, once each, ascending;
// a file committed twice appears twice.
func (f *fakeMigrationDB) committedFiles() []int {
	var out []int
	for v, n := range f.ran {
		for range n {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

// Connect implements driver.Connector.
func (f *fakeMigrationDB) Connect(context.Context) (driver.Conn, error) { return &fakeConn{db: f}, nil }

// Driver implements driver.Connector.
func (f *fakeMigrationDB) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fake: use the connector")
}

// fakeConn is a connection of fakeMigrationDB with its transaction.
type fakeConn struct {
	db      *fakeMigrationDB
	inTx    bool
	aborted bool
	created bool  // the transaction created schema_migrations
	record  []int // versions the transaction recorded
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("fake: no prepare") }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("fake: BEGIN by statement") }

// ExecContext implements driver.ExecerContext.
func (c *fakeConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	_, err := c.run(q, args)
	return driver.RowsAffected(1), err
}

// QueryContext implements driver.QueryerContext.
func (c *fakeConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.run(q, args)
}

// run runs one statement of the runner.
func (c *fakeConn) run(q string, args []driver.NamedValue) (*fakeRows, error) {
	f := c.db
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[q]++
	if q == "ROLLBACK" {
		c.end(false)
		return nil, nil
	}
	if c.aborted {
		return nil, &fakePgError{code: "25P02", msg: "current transaction is aborted"}
	}
	if f.fail != nil {
		if err := f.fail(f, q, f.calls[q]); err != nil {
			if q == "COMMIT" {
				c.end(false) // the database ended it
			} else {
				c.aborted = c.inTx
			}
			return nil, err
		}
	}
	return c.answer(q, args)
}

// answer runs one statement of the runner that did not fail; f.mu is held.
func (c *fakeConn) answer(q string, args []driver.NamedValue) (*fakeRows, error) {
	f, r := c.db, runners[DialectPostgres]
	version := func() int { return int(args[0].Value.(int64)) }
	switch q {
	case r.exists:
		return &fakeRows{cols: []string{"exists"}, rows: [][]driver.Value{{f.table}}}, nil
	case "SELECT version FROM schema_migrations ORDER BY version":
		rows := &fakeRows{cols: []string{"version"}}
		for _, v := range slices.Sorted(slices.Values(f.versions)) {
			rows.rows = append(rows.rows, []driver.Value{int64(v)})
		}
		return rows, nil
	case "BEGIN":
		c.inTx, c.aborted, c.created, c.record = true, false, false, nil
		f.open++
	case "COMMIT":
		c.end(true)
	case r.lock, r.require:
	case r.create:
		c.created = !f.table
	case r.check:
		n := 0
		if slices.Contains(f.versions, version()) || slices.Contains(c.record, version()) {
			n = 1
		}
		return &fakeRows{cols: []string{"count"}, rows: [][]driver.Value{{int64(n)}}}, nil
	case r.record:
		c.record = append(c.record, version())
	default: // a migration file: it commits with the version recorded after it
	}
	return &fakeRows{}, nil
}

// end ends the transaction, committing it when commit; f.mu is held.
func (c *fakeConn) end(commit bool) {
	if !c.inTx {
		return
	}
	f := c.db
	f.open--
	if commit && !c.aborted {
		f.table = f.table || c.created
		for _, v := range c.record {
			if slices.Contains(f.versions, v) {
				f.recordedTwice = true
			}
			f.versions = append(f.versions, v)
			f.ran[v]++
		}
	}
	c.inTx, c.aborted = false, false
}

// fakeRows is the answer of a query.
type fakeRows struct {
	cols []string
	rows [][]driver.Value
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

// recordingHandler is a slog.Handler that keeps the attributes' keys of every record.
type recordingHandler struct {
	mu   sync.Mutex
	keys []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler            { return h }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	r.Attrs(func(a slog.Attr) bool { h.keys = append(h.keys, a.Key); return true })
	return nil
}

// count returns how many records had the attribute key.
func (h *recordingHandler) count(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, k := range h.keys {
		if k == key {
			n++
		}
	}
	return n
}

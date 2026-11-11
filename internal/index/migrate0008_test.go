package index

import (
	"context"
	"crypto/rand"
	"database/sql"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // the driver of the Postgres half
	_ "modernc.org/sqlite"             // the driver of the SQLite half
)

// applyUpTo applies the migrations of dialect up to version last.
func applyUpTo(ctx context.Context, t *testing.T, db *sql.DB, dialect string, last int) {
	t.Helper()
	ms, err := migrations(dialect)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.version > last {
			return
		}
		if _, err := runners[dialect].apply(ctx, db, m); err != nil {
			t.Fatalf("migration %s: %v", m.name, err)
		}
	}
}

// TestMigration0008 carries a database of version 7 over to the columns of the uid of #182:
// users get a uid, a UUID v7 whose time is their created_at, unique; bindings, sessions and the
// audit log are not touched (0009 changes their principals). On SQLite, and on Postgres with
// LAJSTROM_TEST_POSTGRES_DSN.
func TestMigration0008(t *testing.T) {
	t.Run(DialectSQLite, func(t *testing.T) {
		db, err := sql.Open("sqlite", "file:migrate0008?mode=memory&cache=shared&_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		testMigration0008(t, db, DialectSQLite, "?")
	})
	t.Run(DialectPostgres, func(t *testing.T) {
		dsn := os.Getenv("LAJSTROM_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set LAJSTROM_TEST_POSTGRES_DSN to run the Postgres half")
		}
		db := postgresSchema(t, dsn)
		testMigration0008(t, db, DialectPostgres, "$")
	})
}

func testMigration0008(t *testing.T, db *sql.DB, dialect, mark string) {
	ctx := t.Context()
	applyUpTo(ctx, t, db, dialect, 7)
	m := &migrationDB{t: t, db: db, mark: mark}
	seedVersion7(m)
	applyUpTo(ctx, t, db, dialect, 8)
	uids := m.uids()
	for name, uid := range uids {
		if !IsUID(uid) || uid[14] != '7' || !strings.ContainsRune("89ab", rune(uid[19])) {
			t.Errorf("the uid of %s is %q, not a UUID v7", name, uid)
		}
	}
	// alice was created at 2026-11-09T11:22:02.234Z: 0x01a1c00b347a milliseconds, give or take one.
	if a := uids["alice"]; len(a) < 13 || a[:12] != "01a1c00b-347" {
		t.Errorf("alice's uid %q does not begin with the time of her created_at, 01a1c00b-347…", a)
	}
	if uids["alice"] == uids["bob"] || uids["alice"] >= uids["bob"] {
		t.Errorf("uids %v: want two, alice's (the older) first", uids)
	}
	for q, want := range map[string]string{
		"SELECT principal FROM bindings ORDER BY id":   "user:alice user:bob user:gone group:dev",
		"SELECT principal FROM sessions":               "user:alice",
		"SELECT principal FROM audit ORDER BY id":      "user:alice token:3 anonymous",
		"SELECT principal_name FROM audit ORDER BY id": "  ",
	} {
		if got := strings.Join(m.strings(q), " "); got != want {
			t.Errorf("%s: %q, want %q as before 0008", q, got, want)
		}
	}
	if _, err := db.ExecContext(ctx, m.bind("INSERT INTO users (uid, name, kind, disabled, created_at) VALUES (?, 'carol', 'local', ?, ?)"),
		uids["alice"], false, "2026-11-09T12:00:00Z"); err == nil {
		t.Error("a second user with alice's uid was inserted")
	}
}

// TestMigration0009 carries the principals of bindings and sessions over to user:<uid>: those of
// users that exist change, those of users that do not and of groups stay; the audit log keeps its
// principals and gets their names. On SQLite, and on Postgres with LAJSTROM_TEST_POSTGRES_DSN.
func TestMigration0009(t *testing.T) {
	t.Run(DialectSQLite, func(t *testing.T) {
		db, err := sql.Open("sqlite", "file:migrate0009?mode=memory&cache=shared&_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		testMigration0009(t, db, DialectSQLite, "?")
	})
	t.Run(DialectPostgres, func(t *testing.T) {
		dsn := os.Getenv("LAJSTROM_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set LAJSTROM_TEST_POSTGRES_DSN to run the Postgres half")
		}
		testMigration0009(t, postgresSchema(t, dsn), DialectPostgres, "$")
	})
}

func testMigration0009(t *testing.T, db *sql.DB, dialect, mark string) {
	ctx := t.Context()
	applyUpTo(ctx, t, db, dialect, 7)
	m := &migrationDB{t: t, db: db, mark: mark}
	seedVersion7(m)
	applyUpTo(ctx, t, db, dialect, 9)
	uids := m.uids()
	for q, want := range map[string]string{
		"SELECT principal FROM bindings ORDER BY id":   "user:" + uids["alice"] + " user:" + uids["bob"] + " user:gone group:dev",
		"SELECT principal FROM sessions":               "user:" + uids["alice"],
		"SELECT principal FROM audit ORDER BY id":      "user:alice token:3 anonymous",
		"SELECT principal_name FROM audit ORDER BY id": "alice  ",
	} {
		if got := strings.Join(m.strings(q), " "); got != want {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}
}

// migrationDB is a database under migration, with the placeholders of its dialect.
type migrationDB struct {
	t    *testing.T
	db   *sql.DB
	mark string // ? for SQLite, $ for Postgres
}

// bind turns the ? placeholders of q into the dialect's.
func (m *migrationDB) bind(q string) string {
	if m.mark == "?" {
		return q
	}
	n := 0
	var b strings.Builder
	for _, c := range q {
		if c == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// exec runs q, failing the test on an error.
func (m *migrationDB) exec(q string, args ...any) {
	m.t.Helper()
	if _, err := m.db.ExecContext(m.t.Context(), m.bind(q), args...); err != nil {
		m.t.Fatalf("%s: %v", q, err)
	}
}

// strings returns the one text column of the rows of q, NULL as "".
func (m *migrationDB) strings(q string) []string {
	m.t.Helper()
	rows, err := m.db.QueryContext(m.t.Context(), q)
	if err != nil {
		m.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			m.t.Fatal(err)
		}
		out = append(out, s.String)
	}
	if err := rows.Err(); err != nil {
		m.t.Fatal(err)
	}
	return out
}

// uids returns the uid of each user by name.
func (m *migrationDB) uids() map[string]string {
	m.t.Helper()
	names := m.strings("SELECT name FROM users ORDER BY name")
	uids := m.strings("SELECT CAST(uid AS TEXT) FROM users ORDER BY name")
	out := map[string]string{}
	for i, n := range names {
		out[n] = uids[i]
	}
	return out
}

// seedVersion7 writes what a database of version 7 holds: users alice and bob, a repo, bindings
// of alice, bob, a user that does not exist and a group, a session of alice, audit events of
// alice, a deploy token and anonymous.
func seedVersion7(m *migrationDB) {
	for _, u := range []struct{ name, at string }{{"alice", "2026-11-09T11:22:02.234934Z"}, {"bob", "2026-11-09T12:00:00.000000Z"}} {
		m.exec("INSERT INTO users (name, password_hash, kind, disabled, created_at) VALUES (?, ?, 'local', ?, ?)", u.name, "x", false, u.at)
	}
	m.exec("INSERT INTO repos (format, name, registry, prefix, kind, anonymous_read, created_at, updated_at) VALUES ('raw', 'r', 'local', '', 'hosted', ?, ?, ?)",
		false, "2026-11-09T12:00:00Z", "2026-11-09T12:00:00Z")
	for _, b := range []string{"user:alice", "user:bob", "user:gone", "group:dev"} {
		m.exec("INSERT INTO bindings (principal, role, repo_mask, created_at) VALUES (?, 'reader', 'raw/*', ?)", b, "2026-11-09T12:00:00Z")
	}
	m.exec("INSERT INTO sessions (repo_id, key, principal, state, files, created_at, updated_at, expires_at) VALUES (1, 'g:a:1', 'user:alice', 'open', '{}', ?, ?, ?)",
		"2026-11-09T12:00:00Z", "2026-11-09T12:00:00Z", "2026-11-09T13:00:00Z")
	for _, a := range []string{"user:alice", "token:3", "anonymous"} {
		m.exec("INSERT INTO audit (at, principal, action, repo, subject, ip) VALUES (?, ?, 'push', 'raw/r', 'x', '127.0.0.1')", "2026-11-09T12:00:00Z", a)
	}
}

// postgresSchema opens the Postgres of dsn in a schema of its own, dropped when the test ends;
// with LAJSTROM_TEST_POSTGRES_ISOLATION=database, in a database of its own, as the tests
// behind a pooler in transaction mode need (internal/index/postgres, isolationEnv).
func postgresSchema(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	name := "lajstrom_test_" + strings.ToLower(rand.Text())
	param, kind, force := "search_path", "SCHEMA", " CASCADE"
	if os.Getenv("LAJSTROM_TEST_POSTGRES_ISOLATION") == "database" {
		param, kind, force = "dbname", "DATABASE", " WITH (FORCE)"
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "CREATE "+kind+" "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP "+kind+" "+name+force)
		_ = admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set(param, name)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

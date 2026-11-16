package index

import (
	"database/sql"
	"maps"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMigration0034 fills users.last_seen_at as docs/spec/index-schema.md (A user's last
// activity) says: a user with a password from the time of the migration, one without from the
// latest use of its tokens and login of its identities, or NULL; a deleted user stays NULL. Run
// twice, as a cockroach migration that stopped half-way is, it changes nothing more. On SQLite,
// and on Postgres or CockroachDB with LAJSTROM_TEST_POSTGRES_DSN.
func TestMigration0034(t *testing.T) {
	t.Run(DialectSQLite, func(t *testing.T) {
		db, err := sql.Open("sqlite", "file:migrate0034?mode=memory&cache=shared&_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		testMigration0034(t, db, DialectSQLite, "?")
	})
	t.Run(DialectPostgres, func(t *testing.T) {
		dsn := os.Getenv("LAJSTROM_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set LAJSTROM_TEST_POSTGRES_DSN to run the Postgres half")
		}
		db := postgresSchema(t, dsn)
		dialect := DialectPostgres
		if onCockroach(t, db) {
			dialect = DialectCockroach
		}
		testMigration0034(t, db, dialect, "$")
	})
}

func testMigration0034(t *testing.T, db *sql.DB, dialect, mark string) {
	ctx := t.Context()
	applyUpTo(ctx, t, db, dialect, 33)
	m := &migrationDB{t: t, db: db, mark: mark}
	seedVersion33(m)
	before := time.Now().Add(-time.Minute)
	applyUpTo(ctx, t, db, dialect, 34)
	after := time.Now().Add(time.Minute)
	got := lastSeen(m, dialect)
	for _, name := range []string{"pw", "pwtokens"} {
		at, err := time.Parse(time.RFC3339Nano, got[name])
		if err != nil || at.Before(before) || at.After(after) {
			t.Errorf("%s, with a password: last_seen_at %q, want the time of the migration", name, got[name])
		}
	}
	for name, want := range map[string]string{
		"svc":      "2026-11-04T10:00:00.000000Z", // its tokens' latest use
		"sso":      "2026-11-07T10:00:00.000000Z", // its login, after its token's use
		"ssotoken": "2026-11-08T10:00:00.000000Z", // its token's use, after its login
		"idle":     "",                            // neither: counts from created_at
		"~gone":    "",                            // deleted
	} {
		if got[name] != want {
			t.Errorf("%s: last_seen_at %q, want %q", name, got[name], want)
		}
	}
	if dialect == DialectCockroach {
		rerun0034(m, dialect)
		if again := lastSeen(m, dialect); !maps.Equal(again, got) {
			t.Errorf("0034 run again: %v, want %v", again, got)
		}
	}
}

// seedVersion33 writes users of every case of 0034 into a database of version 33: two with a
// password, one of them with a token; a service account with two tokens used; an OIDC user whose
// login is its latest use and one whose token is; a service account whose token was never used;
// a deleted user. Every user was created at 2026-11-01T10:00:00Z.
func seedVersion33(m *migrationDB) {
	const created = "2026-11-01T10:00:00.000000Z"
	for _, u := range []struct {
		name, kind        string
		password, deleted any
	}{
		{"pw", "local", "$argon2id$pw", nil},
		{"pwtokens", "local", "$argon2id$pwtokens", nil},
		{"svc", "service", nil, nil},
		{"sso", "oidc", nil, nil},
		{"ssotoken", "oidc", nil, nil},
		{"idle", "service", nil, nil},
		{"~gone", "local", "$argon2id$gone", "2026-11-05T10:00:00.000000Z"},
	} {
		m.exec("INSERT INTO users (uid, name, password_hash, kind, disabled, created_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			NewUID(time.Now()), u.name, u.password, u.kind, false, created, u.deleted)
	}
	id := func(name string) int64 {
		n, err := strconv.ParseInt(strings.Join(m.strings("SELECT CAST(id AS TEXT) FROM users WHERE name = '"+name+"'"), ""), 10, 64)
		if err != nil {
			m.t.Fatal(err)
		}
		return n
	}
	for _, tok := range []struct {
		user, hash string
		lastUsed   any
	}{
		{"pwtokens", "h0", "2026-11-03T10:00:00.000000Z"},
		{"svc", "h1", "2026-11-04T10:00:00.000000Z"},
		{"svc", "h2", "2026-11-02T10:00:00.000000Z"},
		{"sso", "h3", "2026-11-06T10:00:00.000000Z"},
		{"ssotoken", "h4", "2026-11-08T10:00:00.000000Z"},
		{"idle", "h5", nil},
	} {
		m.exec("INSERT INTO tokens (kind, user_id, name, hash, scopes, expires_at, last_used_at, created_at) VALUES ('pat', ?, 'n', ?, '[]', ?, ?, ?)",
			id(tok.user), tok.hash, "2027-11-01T10:00:00.000000Z", tok.lastUsed, created)
	}
	for _, i := range []struct{ user, subject, login string }{
		{"sso", "s1", "2026-11-07T10:00:00.000000Z"},
		{"ssotoken", "s2", "2026-11-05T10:00:00.000000Z"},
	} {
		m.exec("INSERT INTO identities (user_id, issuer, subject, created_at, login_at) VALUES (?, 'https://idp.example.com', ?, ?, ?)",
			id(i.user), i.subject, created, i.login)
	}
}

// lastSeen returns users.last_seen_at of each user by name, in the text form of SQLite's
// timestamps, "" for NULL.
func lastSeen(m *migrationDB, dialect string) map[string]string {
	names := m.strings("SELECT name FROM users ORDER BY name")
	q := "SELECT last_seen_at FROM users ORDER BY name"
	if dialect != DialectSQLite {
		q = `SELECT to_char(last_seen_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM users ORDER BY name`
	}
	at := m.strings(q)
	out := map[string]string{}
	for i, n := range names {
		out[n] = at[i]
	}
	return out
}

// rerun0034 runs the statements of 0034 again, as the cockroach runner applies a file again from
// its first statement after a stop.
func rerun0034(m *migrationDB, dialect string) {
	ms, err := migrations(dialect)
	if err != nil {
		m.t.Fatal(err)
	}
	for _, mig := range ms {
		if mig.version == 34 {
			for _, stmt := range statements(mig.sql) {
				m.exec(stmt)
			}
		}
	}
}

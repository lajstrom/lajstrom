package index

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestUpgrade runs the migrations of four releases against shared databases, as the replicas
// of a rolling update, a rollback and an upgrade that skips a release do (index-schema.md,
// Upgrades). Release 1 is this binary's; release 2 adds a table, release 3 stops using it with a
// migration that only records its version, and release 4 drops it, requiring release 3. On
// SQLite, and on Postgres with LAJSTROM_TEST_POSTGRES_DSN.
func TestUpgrade(t *testing.T) {
	t.Run(DialectSQLite, func(t *testing.T) {
		testUpgrade(t, DialectSQLite, func(t *testing.T, name string) *sql.DB {
			db, err := sql.Open("sqlite", "file:upgrade-"+name+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = db.Close() })
			return db
		})
	})
	t.Run(DialectPostgres, func(t *testing.T) {
		dsn := os.Getenv("LAJSTROM_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set LAJSTROM_TEST_POSTGRES_DSN to run the Postgres half")
		}
		testUpgrade(t, DialectPostgres, func(t *testing.T, _ string) *sql.DB { return postgresSchema(t, dsn) })
	})
}

func testUpgrade(t *testing.T, dialect string, open func(t *testing.T, name string) *sql.DB) {
	r1, err := migrations(dialect)
	if err != nil {
		t.Fatal(err)
	}
	k := r1[len(r1)-1].version
	next := func(prev []migration, body string) []migration {
		m := migration{version: prev[len(prev)-1].version + 1, sql: body}
		m.name = fmt.Sprintf("%s/%04d_upgrade.sql", dialect, m.version)
		if m.requires, err = migrationRequires(m); err != nil {
			t.Fatal(err)
		}
		return append(slices.Clip(prev), m)
	}
	r2 := next(r1, "-- release 2 writes upgrade_note.\nCREATE TABLE upgrade_note (id integer PRIMARY KEY, note text);\n")
	r3 := next(r2, "-- release 3 stops using upgrade_note.\n")
	r4 := next(r3, fmt.Sprintf("%s%04d\nDROP TABLE upgrade_note;\n", requiresDirective, k+2))
	if r4[len(r4)-1].requires != k+2 {
		t.Fatalf("release 4's migration requires %d, want %d", r4[len(r4)-1].requires, k+2)
	}
	all := make([]int, k)
	for i := range all {
		all[i] = r1[i].version
	}

	type step struct {
		name     string
		binary   []migration
		breaking bool
		applied  []int
		version  int
		err      error
	}
	for _, tt := range []struct {
		db    string
		steps []step
	}{
		{"rolling", []step{
			{"release 1 on an empty database", r1, false, all, k, nil},
			{"release 2 adds a table", r2, false, []int{k + 1}, k + 1, nil},
			{"release 1 runs on it: a replica not yet updated, or a rollback", r1, false, []int{}, k + 1, nil},
			{"release 3 records its version", r3, false, []int{k + 2}, k + 2, nil},
			{"release 4 drops the table release 3 does not use", r4, false, []int{k + 3}, k + 3, nil},
			{"release 3 runs on it", r3, false, []int{}, k + 3, nil},
			{"release 2 refuses it", r2, false, []int{}, k + 3, ErrSchemaTooNew},
			{"release 1 refuses it", r1, false, []int{}, k + 3, ErrSchemaTooNew},
		}},
		{"skip", []step{
			{"release 2 on an empty database", r2, false, append(slices.Clone(all), k+1), k + 1, nil},
			{"release 4 refuses to break release 2", r4, false, []int{}, k + 1, ErrBreakingMigration},
			{"release 2 still runs", r2, false, []int{}, k + 1, nil},
			{"release 4 with Breaking", r4, true, []int{k + 2, k + 3}, k + 3, nil},
			{"release 2 refuses it then", r2, false, []int{}, k + 3, ErrSchemaTooNew},
		}},
		{"empty", []step{
			{"release 4 on an empty database: nobody serves on it", r4, false, append(slices.Clone(all), k+1, k+2, k+3), k + 3, nil},
		}},
	} {
		t.Run(tt.db, func(t *testing.T) {
			db := open(t, tt.db)
			for _, s := range tt.steps {
				rep, err := runners[dialect].applyAll(t.Context(), db, s.binary, MigrateOptions{Breaking: s.breaking})
				if !errors.Is(err, s.err) {
					t.Fatalf("%s: error %v, want %v", s.name, err, s.err)
				}
				if !slices.Equal(rep.Applied, s.applied) || rep.Version != s.version {
					t.Errorf("%s: applied %v, version %d; want %v, %d", s.name, rep.Applied, rep.Version, s.applied, s.version)
				}
				if have, err := AppliedMigrations(t.Context(), db); err != nil || have[len(have)-1] != s.version {
					t.Errorf("%s: schema_migrations has %v, %v; want up to %d", s.name, have, err, s.version)
				}
				if errors.Is(err, ErrSchemaTooNew) && !strings.Contains(err.Error(), fmt.Sprintf("needs a binary that knows migration %04d", k+2)) {
					t.Errorf("%s: %v does not name the migration it needs", s.name, err)
				}
			}
		})
	}
}

func TestMigrationRequires(t *testing.T) {
	tests := []struct {
		sql  string
		want int // -1: error
	}{
		{"CREATE TABLE t (id integer);", 0},
		{"-- 0015: drops t.\n-- lajstrom:requires 0014\n\nDROP TABLE t;", 14},
		{"-- lajstrom:requires 15\nDROP TABLE t;", 15},
		{"DROP TABLE t;\n-- lajstrom:requires 0014", 0},
		{"-- lajstrom:requires 0000", -1},
		{"-- lajstrom:requires 0016", -1},
		{"-- lajstrom:requires next", -1},
	}
	for _, tt := range tests {
		got, err := migrationRequires(migration{version: 15, name: "sqlite/0015_t.sql", sql: tt.sql})
		if (err != nil) != (tt.want < 0) || (err == nil && got != tt.want) {
			t.Errorf("migrationRequires(%q) = %d, %v; want %d", tt.sql, got, err, tt.want)
		}
	}
	// Both dialects require the same.
	lite, err := migrations(DialectSQLite)
	if err != nil {
		t.Fatal(err)
	}
	pg, err := migrations(DialectPostgres)
	if err != nil {
		t.Fatal(err)
	}
	for i := range min(len(lite), len(pg)) {
		if lite[i].requires != pg[i].requires {
			t.Errorf("migration %04d requires %d on sqlite, %d on postgres", lite[i].version, lite[i].requires, pg[i].requires)
		}
	}
}

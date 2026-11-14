package index

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestStatements(t *testing.T) {
	for _, tt := range []struct {
		file string
		want []string
	}{
		{"", nil},
		{"-- a comment only; no statement.\n", nil},
		{"CREATE TABLE IF NOT EXISTS t (id integer);\n", []string{"CREATE TABLE IF NOT EXISTS t (id integer);"}},
		{"-- 0042: two; with a semicolon in the comment.\n\nCREATE TABLE IF NOT EXISTS t (\n    id integer\n);\n\n-- then:\nALTER TABLE t ADD COLUMN IF NOT EXISTS n text;",
			[]string{"CREATE TABLE IF NOT EXISTS t (\n    id integer\n);", "ALTER TABLE t ADD COLUMN IF NOT EXISTS n text;"}},
		{"DELETE FROM t", []string{"DELETE FROM t"}},
	} {
		if got := statements(tt.file); !slices.Equal(got, tt.want) {
			t.Errorf("statements(%q) = %q, want %q", tt.file, got, tt.want)
		}
	}
}

// cockroachRerunnable are the forms of a statement that finds its work done, which every
// statement of a cockroach migration must have, since a migration that stopped half-way runs
// again from its first statement (applySteps); idempotent are statements that are so by
// nature.
var (
	cockroachRerunnable = []*regexp.Regexp{
		regexp.MustCompile(`^CREATE TABLE IF NOT EXISTS `),
		regexp.MustCompile(`^CREATE (UNIQUE )?INDEX IF NOT EXISTS `),
		regexp.MustCompile(`^CREATE SEQUENCE IF NOT EXISTS `),
		regexp.MustCompile(`^ALTER TABLE \S+ (ADD COLUMN IF NOT EXISTS [^,]+(,\s+ADD COLUMN IF NOT EXISTS [^,]+)*|ALTER COLUMN \S+ SET (NOT NULL|DEFAULT .+)|DROP COLUMN IF EXISTS \S+);$`),
		regexp.MustCompile(`^DROP (TABLE|INDEX|SEQUENCE) IF EXISTS `),
		regexp.MustCompile(`(?s)^INSERT INTO .* ON CONFLICT .* DO (NOTHING|UPDATE SET .*);$`),
		regexp.MustCompile(`^(UPDATE|DELETE FROM) `), // they compute their rows from the rows they find
	}
	cockroachComment = regexp.MustCompile(`--`)
)

// rerunnable reports whether stmt has a form of cockroachRerunnable.
func rerunnable(stmt string) bool {
	return slices.ContainsFunc(cockroachRerunnable, func(re *regexp.Regexp) bool { return re.MatchString(stmt) })
}

// TestCockroachMigrations checks the rules of the cockroach migrations that the runner relies on
// (applySteps): statements ends each at its semicolon, and each may run again.
func TestCockroachMigrations(t *testing.T) {
	for _, stmt := range []string{
		"CREATE TABLE t (id integer);",
		"CREATE UNIQUE INDEX t_id ON t (id);",
		"ALTER TABLE t ADD COLUMN n text;",
		"ALTER TABLE t ADD COLUMN IF NOT EXISTS n text, ADD COLUMN m text;",
		"ALTER TABLE t RENAME COLUMN n TO m;",
		"INSERT INTO t (id) VALUES (1);",
		"DROP TABLE t;",
	} {
		if rerunnable(stmt) {
			t.Errorf("%q passes for a statement that may run again", stmt)
		}
	}
	ms, err := migrations(DialectCockroach)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		for _, stmt := range statements(m.sql) {
			if strings.Count(stmt, ";") != 1 || !strings.HasSuffix(stmt, ";") {
				t.Errorf("%s: %q is not one statement that ends at its line's semicolon", m.name, stmt)
			}
			if cockroachComment.MatchString(stmt) {
				t.Errorf("%s: %q has a comment after code; put it on a line of its own", m.name, stmt)
			}
			if strings.Contains(stmt, `COLLATE "C"`) {
				t.Errorf("%s: %q names the collation C, which CockroachDB does not have", m.name, stmt)
			}
			if !rerunnable(stmt) {
				t.Errorf("%s: %q may not run again after a migration stopped half-way: use IF NOT EXISTS, IF EXISTS or ON CONFLICT", m.name, stmt)
			}
		}
	}
}

// TestCockroachRerun applies the cockroach migrations to a new schema, then runs every statement
// of every one of them again, as the next run does after a migration that stopped half-way, and
// applies the newest migration again after its record is gone: nothing fails, and the version is
// recorded once. On CockroachDB, when LAJSTROM_TEST_POSTGRES_DSN names one.
func TestCockroachRerun(t *testing.T) {
	dsn := os.Getenv("LAJSTROM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set LAJSTROM_TEST_POSTGRES_DSN to a CockroachDB to run it")
	}
	ctx := t.Context()
	db := postgresSchema(t, dsn)
	if !onCockroach(t, db) {
		t.Skip("LAJSTROM_TEST_POSTGRES_DSN is not a CockroachDB")
	}
	ms, err := migrations(DialectCockroach)
	if err != nil {
		t.Fatal(err)
	}
	r := runners[DialectCockroach]
	if _, err := r.applyAll(ctx, db, ms, MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		for _, stmt := range statements(m.sql) {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Errorf("%s: %q again: %v", m.name, stmt, err)
			}
		}
	}
	last := ms[len(ms)-1].version
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = $1", last); err != nil {
		t.Fatal(err)
	}
	rep, err := r.applyAll(ctx, db, ms, MigrateOptions{})
	if err != nil || !slices.Equal(rep.Applied, []int{last}) {
		t.Fatalf("applyAll after the record of %04d was deleted: applied %v, %v; want it again", last, rep.Applied, err)
	}
	have, err := AppliedMigrations(ctx, db)
	if err != nil || len(have) != len(ms) {
		t.Errorf("schema_migrations has %v, %v; want each of the %d versions once", have, err, len(ms))
	}
}

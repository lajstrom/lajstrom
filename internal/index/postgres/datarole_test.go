package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/storetest"
)

// dataGrants are the grants of docs/operations.md (Upgrades, A database user for serve) to the
// role %[2]s on the schema %[1]s, run by the schema's owner: data rights on what the migrations
// made and, by default, on what later migrations of the owner make.
const dataGrants = `GRANT USAGE ON SCHEMA %[1]s TO %[2]s;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %[1]s TO %[2]s;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA %[1]s TO %[2]s;
ALTER DEFAULT PRIVILEGES IN SCHEMA %[1]s GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %[2]s;
ALTER DEFAULT PRIVILEGES IN SCHEMA %[1]s GRANT USAGE, SELECT ON SEQUENCES TO %[2]s;`

// TestDataRole runs the store as a database user with data rights only, on schemas that the
// test DSN's user owns and migrated (#487): the conformance suite passes, a table the owner
// makes later is writable, and a pending migration fails, applying nothing, with an error that
// says the owner must migrate.
func TestDataRole(t *testing.T) {
	dsn := testDSN(t)
	role, password := newRole(t, dsn)

	// owned migrates a new namespace as the owner, grants its schema to role and returns the
	// role's DSN for it and the owner's connection.
	owned := func(t *testing.T) (string, *sql.DB) {
		ns := newNamespace(t, dsn)
		openMigrated(t, ns.dsn(dsn))
		owner := openDB(t, ns.dsn(dsn))
		if _, err := owner.ExecContext(t.Context(), fmt.Sprintf(dataGrants, ns.schema, role)); err != nil {
			t.Fatal(err)
		}
		return ns.dsn(withUser(t, dsn, role, password)), owner
	}

	t.Run("conformance", func(t *testing.T) {
		storetest.Run(t, func(t *testing.T) index.Store {
			roleDSN, _ := owned(t)
			s, err := Open(t.Context(), roleDSN)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		})
	})

	t.Run("later table", func(t *testing.T) {
		roleDSN, owner := owned(t)
		if _, err := owner.ExecContext(t.Context(), "CREATE TABLE later (id bigserial PRIMARY KEY, note text)"); err != nil {
			t.Fatal(err)
		}
		if _, err := openDB(t, roleDSN).ExecContext(t.Context(), "INSERT INTO later (note) VALUES ('x')"); err != nil {
			t.Errorf("the role cannot write a table the owner made after the grants: %v", err)
		}
	})

	t.Run("pending", func(t *testing.T) {
		ctx := t.Context()
		roleDSN, owner := owned(t)
		s, err := Open(ctx, roleDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if rep, err := s.ApplyMigrations(ctx, index.MigrateOptions{}); err != nil || len(rep.Applied) != 0 {
			t.Fatalf("ApplyMigrations of a current schema = %v, %v; want none applied", rep.Applied, err)
		}
		// The newest migration becomes pending, as after an upgrade of the binary.
		if _, err := owner.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = (SELECT max(version) FROM schema_migrations)"); err != nil {
			t.Fatal(err)
		}
		rep, err := s.ApplyMigrations(ctx, index.MigrateOptions{})
		if err == nil || len(rep.Applied) != 0 {
			t.Fatalf("ApplyMigrations of a pending migration as the data role = %v, %v; want an error", rep.Applied, err)
		}
		if !strings.Contains(err.Error(), "this database user may not change the schema: run lajstrom migrate as its owner first") {
			t.Errorf("the error %q does not say that the owner must migrate", err)
		}
	})
}

// newRole creates a role that can log in, and no more, and drops it when the test ends.
func newRole(t *testing.T, dsn string) (role, password string) {
	t.Helper()
	role = "lajstrom_test_data_" + strings.ToLower(rand.Text())
	password = rand.Text()
	admin := openDB(t, dsn)
	if _, err := admin.ExecContext(t.Context(), fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", role, password)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP ROLE "+role); err != nil {
			t.Error(err)
		}
	})
	return role, password
}

// openDB opens dsn and closes it when the test ends.
func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// withUser returns dsn, a URL or a keyword/value string, with user and password replaced.
func withUser(t *testing.T, dsn, user, password string) string {
	t.Helper()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(user, password)
		return u.String()
	}
	return dsn + " user=" + user + " password=" + password
}

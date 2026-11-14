package postgres

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestCockroachReadCommitted checks, on CockroachDB, that the store's transactions are read
// committed, which CockroachDB's are not unless they ask: a Tx, an index.Writer method that
// begins its own transaction (the transaction of the session in a transaction, seen from
// another session), and the referrers lock. On another engine it skips.
func TestCockroachReadCommitted(t *testing.T) {
	ctx := t.Context()
	dsn := testDSN(t)
	admin := openDB(t, dsn)
	if !onCockroach(t, admin) {
		t.Skip("not CockroachDB")
	}
	app := "lajstrom_test_" + strings.ToLower(rand.Text())
	s := openMigrated(t, withParam(isolatedDSN(t, dsn), "application_name", app))
	level := func() string {
		var l string
		if err := admin.QueryRowContext(ctx, "SELECT isolation_level FROM crdb_internal.cluster_sessions"+
			" WHERE application_name = $1 AND kv_txn IS NOT NULL", app).Scan(&l); err != nil {
			t.Fatalf("the store's session in a transaction: %v", err)
		}
		return l
	}
	if err := s.Tx(ctx, func(w index.Writer) error {
		if _, err := w.ListRepos(ctx, ""); err != nil {
			return err
		}
		if l := level(); l != "READ COMMITTED" {
			t.Errorf("Tx runs at %s, want READ COMMITTED", l)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WithReferrersLock(ctx, "raw/"+app, digest.FromString("subject"), func(context.Context) error {
		if l := level(); l != "READ COMMITTED" {
			t.Errorf("WithReferrersLock runs at %s, want READ COMMITTED", l)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

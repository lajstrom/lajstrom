//go:build integration

package ingest_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index/postgres"
)

// TestIntegrationAuditIntent is TestAuditIntent on Postgres, with the fake registry. It needs
// LAJSTROM_TEST_POSTGRES_DSN, a database the test may migrate and write repos of random names
// to; it deletes them.
func TestIntegrationAuditIntent(t *testing.T) {
	dsn := os.Getenv("LAJSTROM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set LAJSTROM_TEST_POSTGRES_DSN to run against Postgres")
	}
	testAuditIntent(t, func(t *testing.T) *env {
		st, err := postgres.Open(t.Context(), dsn)
		must(t, err)
		t.Cleanup(func() { _ = st.Close() })
		run := make([]byte, 4)
		_, _ = rand.Read(run)
		e := newEnvOn(t, st, "audit-"+hex.EncodeToString(run))
		t.Cleanup(func() { _ = st.DeleteRepo(context.Background(), e.repo.ID) })
		return e
	})
}

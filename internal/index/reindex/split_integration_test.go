//go:build integration

package reindex_test

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index/postgres"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegrationSplit is TestRebuildSplit against the registry at LAJSTROM_TEST_REGISTRY, over
// SQLite and, when LAJSTROM_TEST_POSTGRES_DSN is set, over Postgres (as TestIntegrationFormats),
// with the names whose earlier form the catalog lists: Zot leaves out netlify/blobs. Each case
// has a raw repo with a random name; its manifests are deleted at the end where the registry
// deletes manifests.
func TestIntegrationSplit(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	prov, err := generic.New(config.Registry{
		Name: "local", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")},
	}, slog.New(slog.DiscardHandler))
	must(t, err)
	dsn := os.Getenv("LAJSTROM_TEST_POSTGRES_DSN")
	for _, db := range []string{"sqlite", "postgres"} {
		for _, p := range []string{"a--b.txt", ".gitignore", "k8s/manifests/app.yaml"} {
			t.Run(db+"/"+p, func(t *testing.T) {
				e := newEnv(t)
				if db == "postgres" {
					if dsn == "" {
						t.Skip("set LAJSTROM_TEST_POSTGRES_DSN to run on Postgres")
					}
					st, err := postgres.Open(t.Context(), dsn)
					must(t, err)
					t.Cleanup(func() { _ = st.Close() })
					must(t, st.Migrate(t.Context()))
					e.st = st
				}
				e.useFormat(t, raw.Plugin{}, provider.Map{"local": prov})
				t.Cleanup(func() {
					deleteAll(context.Background(), t, prov, e.repo.Prefix+"/")
					_ = e.st.DeleteRepo(context.Background(), e.repo.ID)
				})
				rebuildSplit(t, e, p)
			})
		}
	}
}

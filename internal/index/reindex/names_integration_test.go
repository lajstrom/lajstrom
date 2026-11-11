//go:build integration

package reindex_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegrationNames is #597 and #598 against the registry at LAJSTROM_TEST_REGISTRY (and
// LAJSTROM_TEST_REGISTRY_PREFIX, _USERNAME, _PASSWORD as TestIntegration reads them), on a raw
// repo with a random name: files whose OCI names ADR 0017 changes, published through ingest,
// netlify/blobs as npm's @netlify/blobs is stored, a file below a directory blobs/, the words
// of Zot's layout and the names Gitea refuses in the earlier form. The catalog lists every OCI
// repository, which Zot did not for a name that ends in blobs, .uploads or .sync; and a run
// from an empty index brings every package back, every row as ingest wrote it. The manifests
// are deleted at the end where the registry deletes manifests.
func TestIntegrationNames(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	prov, err := generic.New(config.Registry{
		Name: "local", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")},
	}, slog.New(slog.DiscardHandler))
	must(t, err)

	e := newEnv(t)
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	e.repo = e.newRepo(t, "raw", "names-"+hex.EncodeToString(suffix))
	e.repo.Prefix = ""
	must(t, e.st.UpdateRepo(ctx, &e.repo))
	plugins := func(name string) (format.Plugin, bool) { return raw.Plugin{}, name == raw.Name }
	log := slog.New(slog.DiscardHandler)
	e.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": prov}, Index: e.st, Plugins: plugins, Log: log})
	e.w = reindex.New(reindex.Options{Index: e.st, Providers: provider.Map{"local": prov}, Inspector: e.pipe, Log: log, Now: e.clock.now})
	base := "raw/" + e.repo.Name
	t.Cleanup(func() { deleteAll(context.Background(), t, prov, base+"/") })

	paths := []string{"netlify/blobs", "x", "x/blobs/sha256/0f", "x/index.json", "x/oci-layout", "cache/my.uploads", "a.sync",
		"myblobs/child", ".gitignore", "site/_config.yml", "a--b.txt"}
	want := make([]string, 0, len(paths))
	for _, p := range paths {
		want = append(want, e.publish(t, p, p+" in "+e.repo.Name).RepoPath)
	}
	listed, err := prov.ListRepositories(ctx, base+"/")
	must(t, err)
	for _, name := range want {
		if !slices.Contains(listed, name) {
			t.Errorf("the catalog leaves out %s: %q", name, listed)
		}
	}

	before := e.derived(t)
	must(t, e.st.ResetDerived(ctx, e.repo.ID))
	r := e.run(t, reindex.Request{Prune: true})
	if r.Added != len(paths) || len(r.Errors) != 0 || len(r.Foreign) != 0 {
		t.Fatalf("the run from an empty index: %+v", *r)
	}
	same(t, "the rows rebuilt", before, e.derived(t))
}

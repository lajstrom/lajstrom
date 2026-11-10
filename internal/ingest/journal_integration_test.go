//go:build integration

package ingest_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/postgres"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegrationJournalReplicas is the HA test of the journal (#307): changes of one dist-tag
// through two replicas at once, each with its own Postgres pool, provider and pipeline, over one
// database and the registry at LAJSTROM_TEST_REGISTRY. The journal lock is Postgres's advisory
// lock: every change gets its own entry, numbered from 1 without gaps, and the state is the
// change with the last number. It needs LAJSTROM_TEST_POSTGRES_DSN, a database or schema the
// test may migrate and write a repo to: CI's is fresh; on the dev stack, give the DSN
// search_path=<a schema of its own>.
func TestIntegrationJournalReplicas(t *testing.T) {
	addr, dsn := os.Getenv("LAJSTROM_TEST_REGISTRY"), os.Getenv("LAJSTROM_TEST_POSTGRES_DSN")
	if addr == "" || dsn == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY and LAJSTROM_TEST_POSTGRES_DSN to run against a registry and Postgres")
	}
	ctx := t.Context()
	run := make([]byte, 4)
	_, _ = rand.Read(run)
	name := "it-" + hex.EncodeToString(run)
	versions := []string{"1.0.0", "1.0.1", "1.0.2", "1.0.3", "1.0.4", "1.0.5"}
	var (
		pipes []*ingest.Pipeline
		st    *postgres.Store
		repo  index.Repo
		pkg   index.Package
	)
	for i := range 2 {
		s, err := postgres.Open(ctx, dsn)
		must(t, err)
		t.Cleanup(func() { _ = s.Close() })
		if i == 0 {
			st = s
			must(t, st.Migrate(ctx))
			repo = index.Repo{Format: "npm", Name: name, Registry: "it", Prefix: "npm/" + name, Kind: index.KindHosted}
			must(t, st.CreateRepo(ctx, &repo))
			t.Cleanup(func() { _ = st.DeleteRepo(context.Background(), repo.ID) })
			pkg = index.Package{RepoID: repo.ID, Name: "@acme/widgets", NameOCI: widgetsPath}
			must(t, st.UpsertPackage(ctx, &pkg))
			for _, v := range versions {
				must(t, st.UpsertVersion(ctx, &index.Version{PackageID: pkg.ID, Version: v, Tag: v, Digest: dg(name + v),
					ArtifactType: profile.ArtifactType("npm")}))
			}
		}
		prov, err := generic.New(config.Registry{Name: "it", Type: generic.Type, URL: "http://" + addr,
			Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
			Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"),
				Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")}}, slog.New(slog.DiscardHandler))
		must(t, err)
		pipes = append(pipes, ingest.New(ingest.Options{Providers: provider.Map{"it": prov}, Index: s, Log: slog.New(slog.DiscardHandler)}))
	}

	seqs, errs := make([]int64, len(versions)), make([]error, len(versions))
	var wg sync.WaitGroup
	for i, v := range versions {
		wg.Go(func() {
			s, err := pipes[i%2].Journal(ctx, repo, widgetsPath, []format.JournalOp{opAdd("latest", v)}, alice)
			if errs[i] = err; err == nil {
				seqs[i] = s[0]
			}
		})
	}
	wg.Wait()
	must(t, errors.Join(errs...))
	same(t, "seqs", []int64{1, 2, 3, 4, 5, 6}, slices.Sorted(slices.Values(seqs)))
	rows, err := st.ListJournal(ctx, pkg.ID)
	must(t, err)
	if len(rows) != len(versions) {
		t.Fatalf("%d rows, want %d", len(rows), len(versions))
	}
	state, err := st.ListJournalState(ctx, pkg.ID)
	must(t, err)
	same(t, "state", []index.JournalState{{PackageID: pkg.ID, Kind: index.JournalDistTag, Key: "latest",
		Value: versions[slices.Index(seqs, 6)], Seq: 6}}, state)
}

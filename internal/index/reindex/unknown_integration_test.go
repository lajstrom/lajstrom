//go:build integration

package reindex_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegrationRepositoryUnknown is TestRepositoryUnknown against the registry at
// LAJSTROM_TEST_REGISTRY: a version row whose OCI repository the registry never had, as after a
// delete of the repository in its console (#567). The registry's own answer decides what the
// run reports: Zot answers NAME_UNKNOWN to the manifest and its tag list, and the version is
// listed in RepositoryUnknown; distribution answers MANIFEST_UNKNOWN, and the version is pending,
// as prune counts it. Either way nothing is removed and nothing is an error; the delete of the
// API removes it, but for a pending version on a registry whose manifests Lajstrom does not
// delete (Nexus's docker format), where the delete is refused.
func TestIntegrationRepositoryUnknown(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx := t.Context()
	prov, err := generic.New(config.Registry{
		Name: "local", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")},
	}, slog.New(slog.DiscardHandler))
	must(t, err)
	e := newEnv(t)
	e.useFormat(t, raw.Plugin{}, provider.Map{"local": prov})
	t.Cleanup(func() {
		deleteAll(context.Background(), t, prov, e.repo.Prefix+"/")
		_ = e.st.DeleteRepo(context.Background(), e.repo.ID)
	})
	e.publish(t, "docs/kept.txt", "kept")
	const path, version = "docs/gone.txt", "0123456789abcdef"
	d := digest.FromString("never pushed " + e.repo.Name)
	must(t, e.st.Tx(ctx, func(w index.Writer) error {
		pkg := index.Package{RepoID: e.repo.ID, Name: path, NameOCI: path, CreatedAt: time.Now()}
		if err := w.UpsertPackage(ctx, &pkg); err != nil {
			return err
		}
		return w.UpsertVersion(ctx, &index.Version{PackageID: pkg.ID, Version: version, Tag: version, Digest: d,
			ArtifactType: "application/vnd.lajstrom.raw.v1", CreatedAt: time.Now(), IndexedAt: time.Now()})
	}))
	_, stat := prov.StatManifest(ctx, e.repo.Prefix+"/"+path, d)
	t.Logf("the manifest in a repository the registry never had: %v", stat)

	r := e.run(t, reindex.Request{Prune: true})
	if r.Removed != 0 || len(r.Errors) != 0 || r.Unchanged != 1 {
		t.Errorf("the run: %+v; want nothing removed and no error", *r)
	}
	switch {
	case errors.Is(stat, provider.ErrManifestUnknown):
		if r.Pending != 1 || len(r.RepositoryUnknown) != 0 {
			t.Errorf("MANIFEST_UNKNOWN: %+v; want the version pending", *r)
		}
	default:
		want := []reindex.RepositoryUnknown{{Repository: path, Package: path, Version: version, Tag: version, Digest: d,
			Delete: "DELETE /api/v1/repos/raw/" + e.repo.Name + "/packages/docs%2Fgone.txt/versions/" + version}}
		if r.Pending != 0 {
			t.Errorf("a 404 without MANIFEST_UNKNOWN: %+v; want the version not pending", *r)
		}
		same(t, "the versions of the repository the registry does not know", want, r.RepositoryUnknown)
	}

	_, err = e.pipe.Delete(ctx, e.repo, format.Deletion{Path: path, Reference: d.String(),
		Condition: &format.Condition{Version: version}}, alice)
	if caps, cerr := prov.Capabilities(ctx); cerr == nil && !caps.DeleteManifest && errors.Is(stat, provider.ErrManifestUnknown) {
		// A registry whose manifests Lajstrom does not delete (Nexus's docker format), and which
		// answers MANIFEST_UNKNOWN in a repository it never had: the version is pending, as any
		// manifest gone, and its delete is refused as every delete there, on purpose (#352,
		// #800). Only a repository the registry does not know is deleted from the index alone
		// (#692).
		if !errors.Is(err, provider.ErrUnsupported) {
			t.Errorf("the delete on a registry that does not delete manifests: %v, want ErrUnsupported", err)
		}
		return
	}
	must(t, err)
	if _, err := e.st.GetPackage(ctx, e.repo.ID, path); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the package after the delete: %v, want ErrNotFound", err)
	}
	if r := e.run(t, reindex.Request{Prune: true}); len(r.RepositoryUnknown) != 0 || r.Pending != 0 || len(r.Errors) != 0 {
		t.Errorf("the run after the delete: %+v", *r)
	}
}

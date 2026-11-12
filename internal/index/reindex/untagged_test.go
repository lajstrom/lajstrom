package reindex_test

import (
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
)

// TestUntaggedOCIRepository (#748): on an oci repo a run drops the oci_repos row of a repository
// whose manifests are all deleted, which the registry keeps with an empty tag list (Zot, and the
// fake), and keeps the row when a push lands between its tag list and the drop: it lists the
// tags again after the row went, records the repository and reads the pushed tag.
func TestUntaggedOCIRepository(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.repo = e.newRepo(t, "oci", "images")
	const name = "oci/images/app"
	image := func(tag string) ocispec.Manifest {
		return ocispec.Manifest{Versioned: specs2, MediaType: ocispec.MediaTypeImageManifest,
			Config: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.FromString(tag), Size: 1}}
	}
	v1 := e.pushManifest(t, name, "v1", image("v1"), ocispec.MediaTypeImageManifest)
	if r := e.run(t, reindex.Request{Prune: true}); len(r.Foreign) != 1 || len(r.Errors) != 0 {
		t.Errorf("the first run: %+v", *r)
	}
	same(t, "oci_repos after the first run", []string{name}, e.derived(t).OCIRepos)

	rr, err := e.fake.Repository(ctx, name)
	must(t, err)
	must(t, rr.Delete(ctx, v1))
	if r := e.run(t, reindex.Request{Prune: true}); len(r.Foreign) != 0 || len(r.Errors) != 0 {
		t.Errorf("the run after the delete: %+v", *r)
	}
	same(t, "oci_repos after the delete, the tag list empty", []string{}, e.derived(t).OCIRepos)

	must(t, e.st.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: e.repo.ID, Path: name})) // as an earlier run left it
	e.prov.mu.Lock()
	e.prov.afterTags = func() { // the facade's push: the manifest, then its row
		e.pushManifest(t, name, "v2", image("v2"), ocispec.MediaTypeImageManifest)
		must(t, e.st.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: e.repo.ID, Path: name}))
	}
	e.prov.mu.Unlock()
	if r := e.run(t, reindex.Request{Prune: true}); len(r.Foreign) != 1 || r.Foreign[0].Tag != "v2" || len(r.Errors) != 0 {
		t.Errorf("the run that a push races: %+v", *r)
	}
	same(t, "oci_repos after a push between the tag list and the drop", []string{name}, e.derived(t).OCIRepos)
}

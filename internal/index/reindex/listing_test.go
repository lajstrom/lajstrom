package reindex_test

import (
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestRebuildLaggingListing is INV-3 on a registry whose listings lag a write (#446). A run
// reads what the catalog and the tag lists name, so it misses what they leave out: a version
// published to a package the catalog lists, a manifest pushed straight to the registry beside
// it, and a package published to a repository the catalog does not list yet, whose tag the next
// tag list leaves out too. Past the grace age, no run removes a version it missed, for prune
// looks a version up by its tag and its digest, never by a listing; the run after the listings
// name the manifest pushed adds it.
func TestRebuildLaggingListing(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	lag := &providertest.Lag{Fake: e.fake}
	e.through(lag, e.st)
	e.publish(t, "docs/old.txt", "one")
	lag.Lists = 1
	two := e.publish(t, "docs/old.txt", "two")
	e.bypassTo(t, lag, e.repo, "docs/old.txt", "three")
	fresh := e.publish(t, "docs/fresh.txt", "fresh")
	e.clock.later(2 * time.Hour)

	// The first run: the catalog leaves out docs/fresh.txt, the tag list of docs/old.txt two and
	// three. The second: the tag list of docs/fresh.txt leaves out its tag. The third sees all.
	for i, want := range []reindex.RepoReport{
		{OCIRepositories: 1, Manifests: 1, Unchanged: 1},
		{OCIRepositories: 2, Manifests: 3, Unchanged: 2, Added: 1},
		{OCIRepositories: 2, Manifests: 4, Unchanged: 4},
	} {
		r := e.run(t, reindex.Request{Prune: true})
		if r.OCIRepositories != want.OCIRepositories || r.Manifests != want.Manifests || r.Unchanged != want.Unchanged ||
			r.Added != want.Added || r.Removed != 0 || r.Pending != 0 || len(r.Errors) != 0 {
			t.Errorf("run %d: %+v; want %+v", i+1, *r, want)
		}
		for _, p := range []*format.Published{two, fresh} {
			if _, err := e.st.GetVersionByDigest(ctx, e.repo.ID, p.Digest); err != nil {
				t.Errorf("run %d: the version %s:%s, which a listing left out: %v", i+1, p.RepoPath, p.Tag, err)
			}
		}
	}
	if lag.Misses() != 4 {
		t.Errorf("the listings missed %d writes, want 4", lag.Misses())
	}
}

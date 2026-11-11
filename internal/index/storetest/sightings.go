package storetest

import (
	"errors"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testSightings: the answers MANIFEST_UNKNOWN that a removal by reindex waits on (#446). The
// first answer is kept while later ones move LastAt; a key is a repo, a repository and a
// digest; ResetDerived leaves the rows, ClearSightings removes those not recorded again, and
// the repo's deletion all of its rows.
func testSightings(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "sightings")
	other := newRepo(t, s, "raw", "other")
	record := func(repoID int64, path, d string, sec int) index.Sighting {
		t.Helper()
		x := index.Sighting{RepoID: repoID, Path: path, Digest: dg(d), LastAt: at(sec)}
		must(t, s.RecordSighting(ctx, &x))
		return x
	}
	first := record(repo.ID, "raw/sightings/a", "a", 0)
	same(t, "a new sighting", index.Sighting{RepoID: repo.ID, Path: "raw/sightings/a", Digest: dg("a"), FirstAt: at(0), LastAt: at(0)}, first)
	again := record(repo.ID, "raw/sightings/a", "a", 700)
	same(t, "FirstAt of a sighting recorded again", at(0), again.FirstAt)
	for _, x := range []index.Sighting{
		record(repo.ID, "raw/sightings/a", "b", 700),
		record(repo.ID, "raw/sightings/b", "a", 700),
		record(other.ID, "raw/sightings/a", "a", 700),
	} {
		same(t, "FirstAt of another key", at(700), x.FirstAt)
	}
	unstamped := index.Sighting{RepoID: repo.ID, Path: "raw/sightings/c", Digest: dg("c")}
	must(t, s.RecordSighting(ctx, &unstamped))
	if unstamped.LastAt.IsZero() || !unstamped.FirstAt.Equal(unstamped.LastAt) {
		t.Errorf("a sighting without LastAt: %+v, want LastAt now and FirstAt the same", unstamped)
	}

	must(t, s.ResetDerived(ctx, 0))
	same(t, "FirstAt after ResetDerived", at(0), record(repo.ID, "raw/sightings/a", "a", 800).FirstAt)

	n, err := s.ClearSightings(ctx, repo.ID, at(750))
	must(t, err)
	same(t, "ClearSightings of those before 750 s", 2, n) // raw/sightings/a@b and b@a
	same(t, "FirstAt of a sighting kept", at(0), record(repo.ID, "raw/sightings/a", "a", 900).FirstAt)
	same(t, "FirstAt of a sighting cleared", at(900), record(repo.ID, "raw/sightings/b", "a", 900).FirstAt)

	errRollback := errors.New("roll back")
	err = s.Tx(ctx, func(w index.Writer) error {
		x := index.Sighting{RepoID: repo.ID, Path: "raw/sightings/d", Digest: dg("d"), LastAt: at(0)}
		must(t, w.RecordSighting(ctx, &x))
		return errRollback
	})
	wantErr(t, "a Tx that rolls back", err, errRollback)
	same(t, "FirstAt of a sighting rolled back", at(1000), record(repo.ID, "raw/sightings/d", "d", 1000).FirstAt)

	must(t, s.DeleteRepo(ctx, other.ID))
	gone := index.Sighting{RepoID: other.ID, Path: "raw/sightings/a", Digest: dg("a"), LastAt: at(1000)}
	wantErr(t, "RecordSighting in a repo deleted", s.RecordSighting(ctx, &gone), index.ErrNotFound)
	n, err = s.ClearSightings(ctx, other.ID, at(3600))
	must(t, err)
	same(t, "ClearSightings of a repo deleted", 0, n)
}

// testSightingRace: replicas that record one sighting at once all succeed, and all get the
// FirstAt of the one row.
func testSightingRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "sighting-race")
	const writers = 8
	firsts := make([]index.Sighting, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			firsts[i] = index.Sighting{RepoID: repo.ID, Path: "raw/sighting-race/a", Digest: dg("a"), LastAt: at(i)}
			errs[i] = s.RecordSighting(ctx, &firsts[i])
		})
	}
	wg.Wait()
	for i := range writers {
		must(t, errs[i])
		same(t, "FirstAt of each writer", firsts[0].FirstAt, firsts[i].FirstAt)
	}
}

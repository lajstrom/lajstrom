package storetest

import (
	"strconv"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testFetchClaims: the claim of a proxy fetch (ADR 0013). A fresh claim holds the artifact, a
// stale one is taken over with a new id, so that the old holder's heartbeat and release find
// nothing; the claims of another artifact or repo are apart, ResetDerived leaves them, the
// expiry and the repo's deletion remove them.
func testFetchClaims(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "npm", "npmjs")
	other := newRepo(t, s, "npm", "other")
	first := index.FetchClaim{RepoID: repo.ID, Package: "left-pad", Tag: "1.3.0", Principal: "user:a", Replica: "r1:1", ClaimedAt: at(0)}
	must(t, s.ClaimFetch(ctx, &first, at(-120)))
	if first.ID == 0 || !first.HeartbeatAt.Equal(at(0)) {
		t.Fatalf("ClaimFetch filled %+v, want an ID and HeartbeatAt = ClaimedAt", first)
	}
	got, err := s.GetFetchClaim(ctx, repo.ID, "left-pad", "1.3.0")
	must(t, err)
	same(t, "GetFetchClaim", first, *got)

	fresh := index.FetchClaim{RepoID: repo.ID, Package: "left-pad", Tag: "1.3.0", Principal: "user:b", Replica: "r2:1", ClaimedAt: at(10)}
	wantErr(t, "ClaimFetch of a fresh claim", s.ClaimFetch(ctx, &fresh, at(-110)), index.ErrConflict)
	must(t, s.HeartbeatFetchClaim(ctx, first.ID, at(30)))
	wantErr(t, "ClaimFetch after a beat", s.ClaimFetch(ctx, &fresh, at(29)), index.ErrConflict)
	for _, c := range []index.FetchClaim{
		{RepoID: repo.ID, Package: "left-pad", Tag: "1.2.0", Principal: "user:b", Replica: "r2:1", ClaimedAt: at(5)},
		{RepoID: other.ID, Package: "left-pad", Tag: "1.3.0", Principal: "user:b", Replica: "r2:1", ClaimedAt: at(5)},
	} {
		must(t, s.ClaimFetch(ctx, &c, at(-110)))
	}

	// Stale: its last beat is before staleBefore.
	taken := index.FetchClaim{RepoID: repo.ID, Package: "left-pad", Tag: "1.3.0", Principal: "user:c", Replica: "r3:1", ClaimedAt: at(200)}
	must(t, s.ClaimFetch(ctx, &taken, at(31)))
	if taken.ID == first.ID {
		t.Errorf("a claim taken over kept id %d", first.ID)
	}
	wantErr(t, "HeartbeatFetchClaim of a claim taken over", s.HeartbeatFetchClaim(ctx, first.ID, at(201)), index.ErrNotFound)
	wantErr(t, "ReleaseFetchClaim of a claim taken over", s.ReleaseFetchClaim(ctx, first.ID), index.ErrNotFound)
	got, err = s.GetFetchClaim(ctx, repo.ID, "left-pad", "1.3.0")
	must(t, err)
	same(t, "the claim taken over", taken, *got)

	must(t, s.ResetDerived(ctx, 0))
	must(t, s.ReleaseFetchClaim(ctx, taken.ID)) // ResetDerived left it
	_, err = s.GetFetchClaim(ctx, repo.ID, "left-pad", "1.3.0")
	wantErr(t, "GetFetchClaim after the release", err, index.ErrNotFound)

	n, err := s.ExpireFetchClaims(ctx, at(0))
	must(t, err)
	same(t, "ExpireFetchClaims before every beat", 0, n)
	must(t, s.DeleteRepo(ctx, other.ID))
	n, err = s.ExpireFetchClaims(ctx, at(3600))
	must(t, err)
	same(t, "ExpireFetchClaims after the repo of one went", 1, n)
}

// testFetchClaimRace: replicas that claim one artifact at once, one claim fresh and one stale
// before them, end with one holder each time; the others get ErrConflict.
func testFetchClaimRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "npm", "race")
	stale := index.FetchClaim{RepoID: repo.ID, Package: "tool", Tag: "2.0.0", Principal: "user:gone", Replica: "dead:1",
		ClaimedAt: at(-600)}
	must(t, s.ClaimFetch(ctx, &stale, at(-3600)))
	const replicas = 8
	for _, tag := range []string{"1.0.0", "2.0.0"} {
		errs := make([]error, replicas)
		claimIDs := make([]int64, replicas)
		var wg sync.WaitGroup
		for i := range replicas {
			wg.Go(func() {
				c := index.FetchClaim{RepoID: repo.ID, Package: "tool", Tag: tag, Principal: "user:w", Replica: "r" + strconv.Itoa(i),
					ClaimedAt: at(100)}
				errs[i] = s.ClaimFetch(ctx, &c, at(0))
				claimIDs[i] = c.ID
			})
		}
		wg.Wait()
		stored, err := s.GetFetchClaim(ctx, repo.ID, "tool", tag)
		must(t, err)
		winners := 0
		for i, err := range errs {
			if err != nil {
				wantErr(t, "tag "+tag+", replica "+strconv.Itoa(i), err, index.ErrConflict)
				continue
			}
			winners++
			if claimIDs[i] != stored.ID {
				t.Errorf("tag %s, replica %d: ID %d, want the stored claim %d", tag, i, claimIDs[i], stored.ID)
			}
		}
		if winners != 1 {
			t.Errorf("tag %s: %d replicas hold the claim, want 1", tag, winners)
		}
	}
}

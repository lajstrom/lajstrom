package storetest

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// wantTagConflict fails the test unless err is a TagConflictError for tag naming existing.
func wantTagConflict(t *testing.T, what string, err error, tag string, existing digest.Digest) {
	t.Helper()
	var tc *index.TagConflictError
	if !errors.As(err, &tc) || tc.Tag != tag || tc.Existing != existing || !errors.Is(err, index.ErrConflict) {
		t.Errorf("%s: %v, want a TagConflictError for %q naming %s", what, err, tag, existing)
	}
}

// testTagClaims checks the claim rule of ClaimTag, GetTagClaim, ReleaseTagClaim and
// ExpireTagClaims. Claims made before staleBefore are stale.
func testTagClaims(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "a")
	other := newRepo(t, s, "raw", "b")
	staleBefore := at(0)
	get := func(c index.TagClaim) index.TagClaim {
		t.Helper()
		got, err := s.GetTagClaim(ctx, c.RepoID, c.Package, c.Tag)
		must(t, err)
		return *got
	}

	first := index.TagClaim{RepoID: repo.ID, Package: "Tool", Tag: "1.0.0", Digest: dg("a"), Principal: "user:alice", ClaimedAt: at(10)}
	must(t, s.ClaimTag(ctx, &first, staleBefore))
	if first.ID == 0 {
		t.Fatalf("ClaimTag did not fill ID: %+v", first)
	}
	same(t, "GetTagClaim", first, get(first))

	shared := index.TagClaim{RepoID: repo.ID, Package: "Tool", Tag: "1.0.0", Digest: dg("a"), Principal: "user:bob", ClaimedAt: at(20)}
	must(t, s.ClaimTag(ctx, &shared, staleBefore))
	same(t, "ClaimTag with the same digest shares the claim", first, shared)

	fresh := index.TagClaim{RepoID: repo.ID, Package: "Tool", Tag: "1.0.0", Digest: dg("b"), Principal: "user:carol", ClaimedAt: at(30)}
	wantTagConflict(t, "ClaimTag of a fresh claim with another digest", s.ClaimTag(ctx, &fresh, staleBefore), "1.0.0", dg("a"))
	same(t, "claim after the conflict", first, get(first))

	// Another repo, another package (names are case-sensitive) and another tag are other keys.
	for _, c := range []index.TagClaim{
		{RepoID: other.ID, Package: "Tool", Tag: "1.0.0", Digest: dg("c"), Principal: "user:dave"},
		{RepoID: repo.ID, Package: "tool", Tag: "1.0.0", Digest: dg("c"), Principal: "user:dave", ClaimedAt: at(40)},
		{RepoID: repo.ID, Package: "Tool", Tag: "2.0.0", Digest: dg("c"), Principal: "user:dave", ClaimedAt: at(41)},
	} {
		before := time.Now().Truncate(time.Microsecond)
		zero := c.ClaimedAt.IsZero()
		must(t, s.ClaimTag(ctx, &c, staleBefore))
		if c.ID == first.ID || (zero && c.ClaimedAt.Before(before)) {
			t.Errorf("ClaimTag of %s %q %q: %+v, want a new claim (zero ClaimedAt: now)", c.Digest, c.Package, c.Tag, c)
		}
		same(t, "GetTagClaim of another key", c, get(c))
	}

	takeover := index.TagClaim{RepoID: repo.ID, Package: "Tool", Tag: "1.0.0", Digest: dg("b"), Principal: "user:carol", ClaimedAt: at(50)}
	must(t, s.ClaimTag(ctx, &takeover, at(11))) // first was claimed at(10): stale
	if takeover.ID != first.ID {
		t.Errorf("take-over of a stale claim: ID %d, want the row's %d", takeover.ID, first.ID)
	}
	same(t, "claim after the take-over", takeover, get(first))

	must(t, s.ReleaseTagClaim(ctx, takeover.ID))
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"GetTagClaim released", second(s.GetTagClaim(ctx, repo.ID, "Tool", "1.0.0"))},
		{"GetTagClaim missing", second(s.GetTagClaim(ctx, repo.ID, "Tool", "9.9.9"))},
		{"ReleaseTagClaim released", s.ReleaseTagClaim(ctx, takeover.ID)},
		{"ClaimTag of a missing repo", s.ClaimTag(ctx, &index.TagClaim{RepoID: 999999, Package: "p", Tag: "t", Digest: dg("x")}, staleBefore)},
	} {
		wantErr(t, tt.name, tt.err, index.ErrNotFound)
	}

	// Left: other/Tool (now), a/tool at(40), a/Tool:2.0.0 at(41).
	n, err := s.ExpireTagClaims(ctx, at(41))
	must(t, err)
	if n != 1 {
		t.Errorf("ExpireTagClaims(at(41)) = %d, want 1", n)
	}
	wantErr(t, "GetTagClaim expired", second(s.GetTagClaim(ctx, repo.ID, "tool", "1.0.0")), index.ErrNotFound)
	n, err = s.ExpireTagClaims(ctx, time.Now().Add(time.Hour))
	must(t, err)
	if n != 2 {
		t.Errorf("ExpireTagClaims(in an hour) = %d, want 2", n)
	}
}

// testTagClaimRace runs writers that claim one tag at once: with different digests exactly one
// wins and the others get the TagConflictError naming its digest; with the same digest all
// share one claim; on a stale claim exactly one of the writers takes it over.
func testTagClaimRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "race")
	stale := index.TagClaim{RepoID: repo.ID, Package: "tool", Tag: "3.0.0", Digest: dg("old"), Principal: "user:gone", ClaimedAt: at(-60)}
	must(t, s.ClaimTag(ctx, &stale, at(-3600)))
	const writers = 8
	for _, tt := range []struct {
		tag        string
		digest     func(i int) string
		wantWinner int // writers that get nil
	}{
		{"1.0.0", strconv.Itoa, 1},
		{"2.0.0", func(int) string { return "same" }, writers},
		{"3.0.0", strconv.Itoa, 1},
	} {
		errs := make([]error, writers)
		rowIDs := make([]int64, writers)
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				c := index.TagClaim{RepoID: repo.ID, Package: "tool", Tag: tt.tag, Digest: dg(tt.digest(i)), Principal: "user:w" + strconv.Itoa(i)}
				errs[i] = s.ClaimTag(ctx, &c, at(0))
				rowIDs[i] = c.ID
			})
		}
		wg.Wait()
		stored, err := s.GetTagClaim(ctx, repo.ID, "tool", tt.tag)
		must(t, err)
		winners := 0
		for i, err := range errs {
			if err != nil {
				wantTagConflict(t, "tag "+tt.tag+", writer "+strconv.Itoa(i), err, tt.tag, stored.Digest)
				continue
			}
			winners++
			if rowIDs[i] != stored.ID {
				t.Errorf("tag %s, writer %d: ID %d, want the stored row %d", tt.tag, i, rowIDs[i], stored.ID)
			}
		}
		if winners != tt.wantWinner {
			t.Errorf("tag %s: %d writers succeeded, want %d", tt.tag, winners, tt.wantWinner)
		}
	}
}

// testTagClaimsLifecycle checks that ResetDerived leaves tag claims, which are not derived, and
// that DeleteRepo takes the claims of the repo with it.
func testTagClaimsLifecycle(t *testing.T, s index.Store) {
	ctx := t.Context()
	a, b := populate(t, s, "a"), populate(t, s, "b")
	claims := []index.TagClaim{
		{RepoID: a.repo.ID, Package: "pkg", Tag: "2.0.0", Digest: dg("a2"), Principal: "user:alice", ClaimedAt: at(1)},
		{RepoID: b.repo.ID, Package: "pkg", Tag: "2.0.0", Digest: dg("b2"), Principal: "user:alice", ClaimedAt: at(2)},
	}
	for i := range claims {
		must(t, s.ClaimTag(ctx, &claims[i], at(0)))
	}
	must(t, s.ResetDerived(ctx, 0))
	for _, c := range claims {
		got, err := s.GetTagClaim(ctx, c.RepoID, c.Package, c.Tag)
		must(t, err)
		same(t, "claim after ResetDerived", c, *got)
	}
	must(t, s.DeleteRepo(ctx, a.repo.ID))
	wantErr(t, "claim of the deleted repo", second(s.GetTagClaim(ctx, a.repo.ID, "pkg", "2.0.0")), index.ErrNotFound)
	if _, err := s.GetTagClaim(ctx, b.repo.ID, "pkg", "2.0.0"); err != nil {
		t.Errorf("claim of the other repo after DeleteRepo: %v", err)
	}
}

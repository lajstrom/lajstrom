package storetest

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testConvertRepo: a proxy becomes a hosted repo (docs/spec/proxy-and-group.md §2.6): the kind
// and the upstream change and nothing else, AuthVersion moves, UpdateRepo still changes no
// kind; a fresh fetch claim refuses it and a stale one does not; a repo that is not a proxy is
// ErrConflict, no repo ErrNotFound; of two conversions at once one wins.
func testConvertRepo(t *testing.T, s index.Store) {
	ctx := t.Context()
	proxy := index.Repo{Format: "npm", Name: "legacy", Registry: "local", Prefix: "npm/legacy", Kind: index.KindProxy,
		AnonymousRead: true, Policy: json.RawMessage(`{"maxFileSize": 1048576}`),
		Upstream: json.RawMessage(`{"name": "old", "offline": true}`)}
	must(t, s.CreateRepo(ctx, &proxy))
	hosted := newRepo(t, s, "npm", "internal")
	claim := index.FetchClaim{RepoID: proxy.ID, Package: "left-pad", Tag: "1.3.0", Principal: "user:a", Replica: "r1:1",
		ClaimedAt: at(0)}
	must(t, s.ClaimFetch(ctx, &claim, at(-120)))

	wantErr(t, "ConvertRepo with a fresh claim", s.ConvertRepo(ctx, proxy.ID, at(-120)), index.ErrConflict)
	got, err := s.GetRepoByID(ctx, proxy.ID)
	must(t, err)
	same(t, "the repo after a refused conversion", proxy, *got)

	v0, err := s.AuthVersion(ctx)
	must(t, err)
	must(t, s.ConvertRepo(ctx, proxy.ID, at(1))) // the claim beat at 0: stale
	got, err = s.GetRepoByID(ctx, proxy.ID)
	must(t, err)
	want := proxy
	want.Kind, want.Upstream, want.UpdatedAt = index.KindHosted, nil, got.UpdatedAt
	same(t, "the converted repo", want, *got)
	if !got.UpdatedAt.After(proxy.UpdatedAt) {
		t.Errorf("UpdatedAt %s, want after %s", got.UpdatedAt, proxy.UpdatedAt)
	}
	if v1, err := s.AuthVersion(ctx); err != nil || v1 <= v0 {
		t.Errorf("AuthVersion %d after ConvertRepo (%v), want above %d", v1, err, v0)
	}

	wantErr(t, "ConvertRepo of the converted repo", s.ConvertRepo(ctx, proxy.ID, at(1)), index.ErrConflict)
	wantErr(t, "ConvertRepo of a hosted repo", s.ConvertRepo(ctx, hosted.ID, at(1)), index.ErrConflict)
	wantErr(t, "ConvertRepo of no repo", s.ConvertRepo(ctx, hosted.ID+1000, at(1)), index.ErrNotFound)
	back := *got
	back.Kind = index.KindProxy
	must(t, s.UpdateRepo(ctx, &back))
	same(t, "UpdateRepo of the kind", index.KindHosted, back.Kind)

	// Two conversions at once: one changes the row, the other is ErrConflict.
	race := index.Repo{Format: "npm", Name: "race", Registry: "local", Prefix: "npm/race", Kind: index.KindProxy,
		Upstream: json.RawMessage(`{"name": "old"}`)}
	must(t, s.CreateRepo(ctx, &race))
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() { errs[i] = s.ConvertRepo(ctx, race.ID, at(1)) })
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, index.ErrConflict):
			t.Errorf("a concurrent ConvertRepo: %v, want nil or ErrConflict", err)
		}
	}
	if won != 1 {
		t.Errorf("%d concurrent conversions succeeded, want 1 (%v)", won, errs)
	}
}

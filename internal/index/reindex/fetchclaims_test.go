package reindex_test

import (
	"errors"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
)

// TestStaleFetchClaims: a run deletes the claims of proxy fetches whose last beat is 2 minutes
// old, as it deletes the stale tag claims, and leaves a fresh one (docs/spec/index-schema.md,
// Proxy and group repos).
func TestStaleFetchClaims(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	now := time.Now()
	stale := index.FetchClaim{RepoID: e.repo.ID, Package: "a", Tag: "1", Principal: "anonymous", Replica: "gone:1",
		ClaimedAt: now.Add(-time.Hour), HeartbeatAt: now.Add(-3 * time.Minute)}
	fresh := index.FetchClaim{RepoID: e.repo.ID, Package: "b", Tag: "1", Principal: "anonymous", Replica: "alive:1",
		ClaimedAt: now.Add(-time.Hour), HeartbeatAt: now.Add(-time.Minute)}
	for _, c := range []*index.FetchClaim{&stale, &fresh} {
		must(t, e.st.ClaimFetch(ctx, c, now.Add(-2*time.Minute)))
	}
	e.run(t, reindex.Request{})
	if _, err := e.st.GetFetchClaim(ctx, e.repo.ID, "a", "1"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the stale claim after a run: %v, want ErrNotFound", err)
	}
	if _, err := e.st.GetFetchClaim(ctx, e.repo.ID, "b", "1"); err != nil {
		t.Errorf("the fresh claim after a run: %v", err)
	}
}

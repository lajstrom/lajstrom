package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// TestClaimTakenOver: when another replica takes a claim over, the heartbeat of the fetch that
// held it calls lost, which stops an npm fetch; an oci fetch has no lost, and goes on.
func TestClaimTakenOver(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:proxy-claim-taken-over?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := index.Repo{Format: "raw", Name: "up", Registry: "local", Prefix: "raw/up", Kind: index.KindProxy,
		Upstream: json.RawMessage(`{"name": "up"}`)}
	if err := st.CreateRepo(ctx, &repo); err != nil {
		t.Fatal(err)
	}
	const beat = 5 * time.Millisecond
	c := New(Options{Index: st, Replica: "r1:1", ClaimHeartbeat: beat, Log: slog.New(slog.DiscardHandler)})
	for _, stops := range []bool{true, false} {
		fctx, cancel := context.WithCancel(ctx)
		f := fetchClaim{repoID: repo.ID, pkg: "tool", tag: strconv.FormatBool(stops), repo: "raw/up", what: "tool",
			done: func(context.Context) (bool, error) { return false, nil }}
		wait := 20 * beat // long enough for several beats
		if stops {
			f.lost, wait = cancel, 5*time.Second
		}
		release, shared, err := c.claim(fctx, f)
		if err != nil || shared {
			t.Fatalf("claim: shared %v, %v", shared, err)
		}
		// Another replica finds the claim stale and takes it over: the holder's id is gone.
		other := index.FetchClaim{RepoID: repo.ID, Package: "tool", Tag: f.tag, Principal: "anonymous", Replica: "r2:1"}
		if err := st.ClaimFetch(ctx, &other, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-fctx.Done():
			if !stops {
				t.Error("a fetch without lost stopped when its claim was taken over")
			}
		case <-time.After(wait):
			if stops {
				t.Error("a fetch with lost went on after its claim was taken over")
			}
		}
		release() // finds the claim gone, and leaves the other replica's
		if got, err := st.GetFetchClaim(ctx, repo.ID, "tool", f.tag); err != nil || got.ID != other.ID {
			t.Errorf("after release: %+v, %v; want the other replica's claim %d", got, err, other.ID)
		}
		cancel()
	}
}

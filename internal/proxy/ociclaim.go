package proxy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// ociClaim takes the fetch claim of ref, a digest, at p's path (§5, step 6, with the key of
// §11.4): the claim's package is the path and its tag the reference. While another replica
// holds a fresh claim the request waits, asking done after WaitFirst and then twice as long each
// time, at most WaitLongest apart: shared is true when done reports that the other replica's
// fetch is there; a claim gone without it is taken, once; a claim that stopped beating is taken
// over. The claim taken beats every ClaimHeartbeat until release, which removes it; a claim
// that another replica took over ends ctx's work: release is then all that is left to call.
func (c *Core) ociClaim(ctx context.Context, p *ociProxied, ref string, done func(context.Context) (bool, error)) (release func(), shared bool, err error) {
	cl := index.FetchClaim{RepoID: p.repo.ID, Package: p.path, Tag: ref, Principal: auth.PrincipalFrom(ctx).String(), Replica: c.o.Replica}
	w := claimWait{next: c.o.WaitFirst}
	for {
		now := c.o.Now()
		cl.ClaimedAt, cl.HeartbeatAt = now, now
		err := c.o.Index.ClaimFetch(ctx, &cl, now.Add(-c.o.ClaimStale))
		if err == nil {
			break
		}
		if !errors.Is(err, index.ErrConflict) {
			return nil, false, err
		}
		if shared, err := c.ociWait(ctx, p, ref, done, &w); shared || err != nil {
			return nil, shared, err
		}
	}
	stop := c.ociHeartbeat(ctx, p, cl.ID)
	return func() {
		stop()
		if err := c.o.Index.ReleaseFetchClaim(context.WithoutCancel(ctx), cl.ID); err != nil && !errors.Is(err, index.ErrNotFound) {
			c.o.Log.WarnContext(ctx, "proxy: a fetch claim not released; it goes stale", "repo", p.id(), "path", p.path, "ref", ref,
				"error", err.Error())
		}
	}, false, nil
}

// claimWait is the state of a wait for another replica's claim: the next wait, and whether a
// claim gone without the fetch's result was taken once already.
type claimWait struct {
	next    time.Duration
	retried bool
}

// ociWait waits while another replica holds the claim of ref at p's path: shared when done
// reports its result, nil when the claim is gone (once) or stale and may be taken.
func (c *Core) ociWait(ctx context.Context, p *ociProxied, ref string, done func(context.Context) (bool, error), w *claimWait) (shared bool, err error) {
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(w.next):
		}
		w.next = min(2*w.next, c.o.WaitLongest)
		if ok, err := done(ctx); ok || err != nil {
			return ok, err
		}
		other, err := c.o.Index.GetFetchClaim(ctx, p.repo.ID, p.path, ref)
		switch {
		case errors.Is(err, index.ErrNotFound) && w.retried:
			return false, fmt.Errorf("%w: proxy: %s: the fetch of %s@%s by another replica ended without it", format.ErrBadGateway,
				p.id(), p.path, ref)
		case errors.Is(err, index.ErrNotFound):
			w.retried = true
			return false, nil
		case err != nil:
			return false, err
		case other.HeartbeatAt.Before(c.o.Now().Add(-c.o.ClaimStale)):
			return false, nil // stale: ClaimFetch takes it over
		}
	}
}

// ociHeartbeat beats the claim id every ClaimHeartbeat until stop is called. A claim that is
// gone, taken over by a replica that found it stale, is logged: the other replica fetches too,
// and the backend takes the second copy of the same bytes.
func (c *Core) ociHeartbeat(ctx context.Context, p *ociProxied, id int64) (stop func()) {
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(c.o.ClaimHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			err := c.o.Index.HeartbeatFetchClaim(ctx, id, c.o.Now())
			if errors.Is(err, index.ErrNotFound) {
				c.o.Log.WarnContext(ctx, "proxy: the fetch claim was taken over by another replica", "repo", p.id(), "claim", id)
				return
			}
			if err != nil && ctx.Err() == nil {
				c.o.Log.WarnContext(ctx, "proxy: a fetch claim did not beat", "repo", p.id(), "claim", id, "error", err.Error())
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

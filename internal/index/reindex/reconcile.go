package reindex

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// SourceReconcile is the source of the runs of the reconcile loop.
const SourceReconcile = "reconcile"

// Reconcile runs a reindex of every repo with prune every interval until ctx ends: writes that
// went past Lajstrom (a push straight to the backend, a delete in the registry's console)
// reach the index without an operator (#30; docs/operations.md, Reconcile). The first run comes
// after a random part of interval, so replicas that start together spread out; a run that
// overlaps a run elsewhere (another replica's loop, the CLI, the API) is skipped until the next
// tick, and the next tick is counted from the end of a run, so the loop of one replica never
// overlaps itself. An error of a run is logged and the loop goes on. A run writes no audit row
// at its start; it writes one row reconcile per repo whose index it changed.
func (w *Worker) Reconcile(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	wait := w.jitter(interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.after(wait):
		}
		w.reconcileOnce(ctx)
		wait = interval
	}
}

// reconcileOnce is one run of the loop.
func (w *Worker) reconcileOnce(ctx context.Context) {
	req := Request{Prune: true, Source: SourceReconcile, Principal: auth.Anonymous.String()}
	run, err := w.Start(ctx, req)
	var conflict *index.ReindexConflictError
	switch {
	case errors.As(err, &conflict):
		w.log.DebugContext(ctx, "reconcile: another reindex holds the lock; skipped until the next tick",
			"run", conflict.Running.ID, "replica", conflict.Running.Replica)
		return
	case err != nil:
		w.log.ErrorContext(ctx, "reconcile: the run did not start", "error", err.Error())
		return
	}
	report, err := w.Run(ctx, run, req)
	if err != nil {
		w.log.ErrorContext(ctx, "reconcile: the run failed", "run", run.ID, "error", err.Error())
	}
	if report == nil {
		return
	}
	for _, r := range report.Repos {
		if r.Added+r.Updated+r.Removed == 0 {
			continue
		}
		ev := index.AuditEvent{At: w.now(), Principal: req.Principal, Action: SourceReconcile, Repo: r.Repo,
			Subject: "run " + itoa(run.ID)}
		ev.Details, _ = json.Marshal(map[string]any{"run": run.ID, "added": r.Added, "updated": r.Updated,
			"removed": r.Removed, "drift": len(r.Drift), "split": len(r.Split), "repositoryUnknown": len(r.RepositoryUnknown),
			"repairedReferrers": len(r.RepairedReferrers), "errors": len(r.Errors)})
		w.writeAudit(context.WithoutCancel(ctx), ev)
	}
}

// randomPart returns a random duration in [0, d).
func randomPart(d time.Duration) time.Duration { return rand.N(d) } //nolint:gosec // the jitter of a schedule, not a secret

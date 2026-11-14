package webhook

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// The defaults of a Worker.
const (
	DefaultWorkers   = 4
	DefaultRetention = 168 * time.Hour
	pollInterval     = time.Second
	sweepInterval    = time.Minute
)

// Worker delivers the due deliveries of every webhook from the outbox, on one replica: it claims
// at most Workers of them at a time with the replica's id, sends them, and records each outcome
// under its claim (docs/spec/webhooks.md §4). Several replicas' workers share the outbox through
// the claims. It also deletes the deliveries older than Retention. Create it as a struct literal.
type Worker struct {
	// Index is the index database; required.
	Index index.Store
	// Deliverer sends the requests; required.
	Deliverer *Deliverer
	// Master opens the webhooks' secrets; required.
	Master *keys.Master
	// Replica is the id of the replica, which its claims carry; required.
	Replica string
	// Workers is the number of deliveries in flight; zero or less means DefaultWorkers.
	Workers int
	// MaxAttempts is the number of attempts before a delivery is failed; zero or less means
	// DefaultMaxAttempts.
	MaxAttempts int
	// Retention is the age past which a delivery is deleted; zero or less means
	// DefaultRetention.
	Retention time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
}

// Run delivers until ctx ends: it looks for due deliveries every second, at once again after a
// round that found some, and deletes the old ones every minute.
func (w *Worker) Run(ctx context.Context) {
	poll := time.NewTimer(0)
	defer poll.Stop()
	sweep := time.NewTicker(sweepInterval)
	defer sweep.Stop()
	w.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			w.sweep(ctx)
		case <-poll.C:
			n, err := w.RunOnce(ctx)
			if err != nil && ctx.Err() == nil {
				w.log().WarnContext(ctx, "webhook: claiming the due deliveries failed; trying again", "error", err.Error())
			}
			next := pollInterval
			if n > 0 {
				next = 0
			}
			poll.Reset(next)
		}
	}
}

// RunOnce claims at most Workers due deliveries, sends them at once, and returns when each has
// its outcome recorded, with how many it claimed.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	now := w.now()
	claims, err := w.Index.ClaimWebhookDeliveries(ctx, w.Replica, now, now.Add(-ClaimStale), w.workers())
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	for _, c := range claims {
		wg.Go(func() { w.deliver(ctx, c) })
	}
	wg.Wait()
	return len(claims), nil
}

// deliver makes one attempt of the claimed delivery c and records its outcome. When ctx ends
// during the attempt nothing is recorded: the claim goes stale and the row is sent again.
func (w *Worker) deliver(ctx context.Context, c index.WebhookClaim) {
	d, h := c.Delivery, c.Webhook
	var res Result
	key, err := Open(w.Master, h)
	if err != nil {
		res = Result{Error: "the secret does not open with this replica's master key"}
		w.log().ErrorContext(ctx, "webhook: a secret does not open; is the master key the same on every replica?",
			"webhook", h.ID, "error", err.Error())
	} else if res, err = w.Deliverer.Attempt(ctx, h.URL, key, d.EventID, d.Payload); err != nil {
		return
	}
	Settle(&d, res, w.now(), w.maxAttempts())
	switch err := w.Index.FinishWebhookDelivery(context.WithoutCancel(ctx), &d); {
	case errors.Is(err, index.ErrNotFound):
		w.log().InfoContext(ctx, "webhook: the claim of a delivery went stale, another replica sends it", "webhook", h.ID,
			"delivery", d.ID)
		return
	case err != nil:
		w.log().WarnContext(ctx, "webhook: the outcome of a delivery not recorded; it is sent again", "webhook", h.ID,
			"delivery", d.ID, "error", err.Error())
		return
	}
	if d.State == index.DeliveryFailed {
		w.log().WarnContext(ctx, "webhook: a delivery failed for good", "webhook", h.ID, "host", hostOf(h.URL),
			"delivery", d.ID, "event", d.EventID, "type", d.EventType, "attempts", d.Attempts, "status", d.LastStatus,
			"error", d.LastError)
	}
}

// Settle writes on d the outcome res of an attempt made at now: delivered on a 2xx; failed when
// the target was refused or the attempts reached maxAttempts; else pending, due after Backoff.
func Settle(d *index.WebhookDelivery, res Result, now time.Time, maxAttempts int) {
	d.Attempts++
	d.LastAt, d.LastStatus, d.LastError = &now, res.Status, res.Error
	switch {
	case res.Delivered:
		d.State, d.DeliveredAt = index.DeliveryDelivered, &now
	case res.Refused || d.Attempts >= maxAttempts:
		d.State = index.DeliveryFailed
	default:
		d.State, d.NextAt = index.DeliveryPending, now.Add(Backoff(d.Attempts))
	}
}

// sweep deletes the deliveries older than the retention; a failure is logged, and the next
// sweep tries again.
func (w *Worker) sweep(ctx context.Context) {
	retention := w.Retention
	if retention <= 0 {
		retention = DefaultRetention
	}
	n, err := w.Index.DeleteWebhookDeliveries(ctx, w.now().Add(-retention))
	switch {
	case err != nil && ctx.Err() == nil:
		w.log().WarnContext(ctx, "webhook: old deliveries not deleted", "error", err.Error())
	case n > 0:
		w.log().DebugContext(ctx, "webhook: old deliveries deleted", "deleted", n, "retention", retention.String())
	}
}

func (w *Worker) workers() int {
	if w.Workers <= 0 {
		return DefaultWorkers
	}
	return w.Workers
}

func (w *Worker) maxAttempts() int {
	if w.MaxAttempts <= 0 {
		return DefaultMaxAttempts
	}
	return w.MaxAttempts
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

// hostOf is the host of a webhook's URL, which the logs name instead of the URL: receivers put
// tokens in paths.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

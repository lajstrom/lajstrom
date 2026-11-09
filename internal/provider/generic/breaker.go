package generic

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// The circuit breaker of a registry: breakerThreshold requests in a row that fail open it, and
// it stays open for breakerCooldown. Variables so that tests can change them.
var (
	breakerThreshold = 5
	breakerCooldown  = 30 * time.Second
)

// The states of a breaker.
const (
	breakerClosed   = iota // requests go through
	breakerOpen            // requests fail at once, until the cooldown is over
	breakerHalfOpen        // one GET or HEAD goes through as a trial; the others fail at once
)

// breaker is the circuit breaker of one registry. A request fails when the registry answers
// with a 5xx other than 501, or does not answer; after breakerThreshold failures in a row the
// breaker opens, and for breakerCooldown every request fails at once with a
// *provider.UnavailableError that says how much of the cooldown is left, instead of waiting for
// timeouts and retries. Then the next GET or HEAD, the readiness ping
// of serve every 20 s or a client's, goes through as a trial: an answer closes the breaker, a
// failure opens it for another cooldown. A request with a body is never the trial: an upload
// would keep the breaker half-open for as long as it streams. A 4xx is an answer; a 429 and a
// request whose caller went away count neither way.
type breaker struct {
	registry string       // the label of the metric
	log      *slog.Logger // carries registry=<name>
	now      func() time.Time

	mu       sync.Mutex
	state    int
	failures int       // in a row, while closed
	opened   time.Time // when it last opened
	trial    bool      // a trial is in flight, while half-open
}

func newBreaker(registry string, log *slog.Logger) *breaker {
	metrics.ProviderCircuitOpen.WithLabelValues(registry).Set(0)
	return &breaker{registry: registry, log: log, now: time.Now}
}

// allow reports whether req may go to the registry, and whether it is the trial of a
// half-open breaker; done must be called with the outcome when allow returned nil.
func (b *breaker) allow(req *http.Request) (trial bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == breakerOpen && b.now().Sub(b.opened) >= breakerCooldown {
		b.state = breakerHalfOpen
	}
	switch b.state {
	case breakerClosed:
		return false, nil
	case breakerHalfOpen:
		if !b.trial && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
			b.trial = true
			return true, nil
		}
		return false, &provider.UnavailableError{} // a trial is under way: the answer is near
	}
	return false, &provider.UnavailableError{RetryAfter: max(breakerCooldown-b.now().Sub(b.opened), 0)}
}

// outcome classifies an attempt for the breaker: +1 the registry answered, -1 it failed, 0
// neither (a 429, the caller's context ended).
func outcome(ctx context.Context, resp *http.Response, err error) int {
	switch {
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		return 0
	case err != nil:
		return -1
	case resp.StatusCode == http.StatusTooManyRequests:
		return 0
	case resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented:
		return -1
	}
	return +1
}

// done records the outcome of a request that allow let through.
func (b *breaker) done(ctx context.Context, trial bool, result int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if trial {
		b.trial = false
	}
	switch {
	case result > 0:
		if b.state != breakerClosed {
			b.log.InfoContext(ctx, "registry answers again: circuit closed") // the logger names the registry
			metrics.ProviderCircuitOpen.WithLabelValues(b.registry).Set(0)
		}
		b.state, b.failures = breakerClosed, 0
	case result < 0 && b.state == breakerClosed:
		b.failures++
		if b.failures >= breakerThreshold {
			b.open(ctx)
		}
	case result < 0 && trial:
		b.open(ctx)
	}
}

// open opens the breaker for a cooldown; b.mu is held.
func (b *breaker) open(ctx context.Context) {
	if b.state == breakerClosed {
		b.log.WarnContext(ctx, "registry fails: circuit open, its requests fail at once for a while",
			"failures", b.failures, "cooldown", breakerCooldown)
		metrics.ProviderCircuitOpen.WithLabelValues(b.registry).Set(1)
	}
	b.state, b.opened, b.failures = breakerOpen, b.now(), 0
}

// breakerTransport sends a request through the registry's breaker: a request it refuses
// fails with a *provider.UnavailableError, without reaching the registry. It sits above the retries, so a
// request is one outcome however often it was tried.
type breakerTransport struct {
	next http.RoundTripper
	b    *breaker
}

// RoundTrip implements http.RoundTripper.
func (t *breakerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	trial, err := t.b.allow(req)
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	resp, err := t.next.RoundTrip(req)
	t.b.done(req.Context(), trial, outcome(req.Context(), resp, err))
	return resp, err
}

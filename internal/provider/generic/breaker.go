package generic

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
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
// would keep the breaker half-open for as long as it streams. A 4xx is an answer; a 429, a
// request whose caller went away and one whose body failed to read count neither way.
type breaker struct {
	registry string       // the label of the metric
	log      *slog.Logger // carries registry=<name>
	now      func() time.Time

	mu       sync.Mutex
	state    int
	failures int       // in a row, while closed
	opened   time.Time // when it last opened
	trial    bool      // a trial is in flight, while half-open
	// waitUntil is when the wait ends that the registry last asked for (askedToWait).
	waitUntil time.Time
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
// neither (a 429, the caller's context ended, or bodyFailed: a read of the request's body failed
// and no answer came, the caller's doing).
func outcome(ctx context.Context, resp *http.Response, err error, bodyFailed bool) int {
	switch {
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		return 0
	case err != nil && bodyFailed:
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
// request is one outcome however often it was tried. It watches the reads of the request's
// body: the facade streams a client's upload into the registry, and a client that stalls past
// server.stallTimeout, breaks off or sends a broken chunked encoding fails the request to the
// registry as a registry that does not answer would (#638). A timeout or an EOF that it counts
// as the registry's failure comes back as a *noAnswerError.
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
	var body *watchedBody
	if req.Body != nil && req.Body != http.NoBody {
		body = &watchedBody{ReadCloser: req.Body}
		watched := *req // a RoundTripper must not change the request it was given
		watched.Body = body
		req = &watched
	}
	resp, err := t.next.RoundTrip(req)
	result := outcome(req.Context(), resp, err, body.failed())
	t.b.done(req.Context(), trial, result)
	if result < 0 && err != nil && quiet(err) {
		err = &noAnswerError{err: err}
	}
	if resp != nil {
		if after, ok := retryAfter(resp); ok {
			t.b.askedToWait(after)
		}
	}
	return resp, err
}

// quiet reports whether err, the failure of a request, is a timeout or an EOF: the registry did
// not answer in time, or closed the connection without an answer. Both also end what is not
// the registry's doing (a caller's deadline, a client's body that stalls or breaks off, #643),
// so only breakerTransport, which tells these apart (outcome), marks the registry's.
func quiet(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout() || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// noAnswerError is the error of a request that timed out or met an EOF (quiet), and that
// breakerTransport counted as the registry's failure: neither the caller's context nor the
// request's body was the cause. translateAs makes it provider.ErrUnavailable (#670). Its
// message is the error's.
type noAnswerError struct{ err error }

func (e *noAnswerError) Error() string { return e.err.Error() }

func (e *noAnswerError) Unwrap() error { return e.err }

// Timeout reports whether the error is a timeout: the *url.Error that net/http puts around it
// asks, and a timed-out blob push is tried again on that (transientPush).
func (e *noAnswerError) Timeout() bool {
	var ne net.Error
	return errors.As(e.err, &ne) && ne.Timeout()
}

// detail says how the registry did not answer, for the message of the provider.ErrUnavailable.
func (e *noAnswerError) detail() string {
	if e.Timeout() {
		return "the registry did not answer in time"
	}
	return "the connection closed without an answer"
}

// watchedBody is the body of a request through breakerTransport: it records a read that failed
// with an error other than io.EOF. The transport reads it in a goroutine of its own, hence the
// atomic.
type watchedBody struct {
	io.ReadCloser
	broke atomic.Bool // a read failed
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.broke.Store(true)
	}
	return n, err
}

// failed reports whether a read of the body failed; false for a request without one.
func (b *watchedBody) failed() bool { return b != nil && b.broke.Load() }

// askedToWait records that the registry asked for a wait of d, with the Retry-After of a 429 or
// 503: it throttles Lajstrom's credential, so the wait is the registry's, not one request's.
func (b *breaker) askedToWait(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if until := b.now().Add(d); until.After(b.waitUntil) {
		b.waitUntil = until
	}
}

// waitLeft returns what is left of the wait the registry last asked for, zero when none.
func (b *breaker) waitLeft() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(b.waitUntil.Sub(b.now()), 0)
}

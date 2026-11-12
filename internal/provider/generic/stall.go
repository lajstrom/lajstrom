package generic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// stallTransport bounds every wait for the next bytes of an upstream's answer by timeout,
// upstreams[].timeout (docs/spec/proxy-and-group.md §2.1). The transport below bounds the wait
// for the headers; a body whose bytes stop coming while its connection stays open would hold
// the request for as long as the upstream keeps the connection (#883). Each request runs on a
// context of its own, which a read of the body that waits longer than timeout cancels; the
// read then fails with a *stallError, a timeout. Only the time spent in a read counts: a caller
// that is slow to read, a backend that takes its time with the bytes, is never cut.
type stallTransport struct {
	next    http.RoundTripper
	timeout time.Duration
}

// RoundTrip implements http.RoundTripper.
func (t *stallTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	resp, err := t.next.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return resp, err
	}
	resp.Body = &stallBody{rc: resp.Body, timeout: t.timeout, cancel: cancel}
	return resp, nil
}

// stallBody is a body of stallTransport: a read that waits longer than timeout cancels the
// request and fails with a *stallError. Close releases the request.
type stallBody struct {
	rc      io.ReadCloser
	timeout time.Duration
	cancel  context.CancelFunc
	timer   *time.Timer
	stalled atomic.Bool
}

// Read reads the body, with timeout as the bound on the read.
func (b *stallBody) Read(p []byte) (int, error) {
	if b.timer == nil {
		b.timer = time.AfterFunc(b.timeout, func() {
			b.stalled.Store(true)
			b.cancel()
		})
	} else {
		b.timer.Reset(b.timeout)
	}
	n, err := b.rc.Read(p)
	b.timer.Stop()
	if err != nil && !errors.Is(err, io.EOF) && b.stalled.Load() {
		return n, &stallError{timeout: b.timeout}
	}
	return n, err
}

// Close stops the bound and releases the request.
func (b *stallBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}
	err := b.rc.Close()
	b.cancel()
	return err
}

// stallError is a read of an upstream's body that got no byte within upstreams[].timeout: a
// net.Error whose Timeout is true, as the wait for the headers is. It does not wrap the read's
// own error, the cancel of the request's context, which is not the caller's.
type stallError struct{ timeout time.Duration }

func (e *stallError) Error() string {
	return fmt.Sprintf("the upstream sent no byte of the body for %s", e.timeout)
}

// Timeout reports true: the bound of upstreams[].timeout passed.
func (e *stallError) Timeout() bool { return true }

// Temporary reports false; net.Error asks.
func (e *stallError) Temporary() bool { return false }

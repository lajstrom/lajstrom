package generic

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"

	"oras.land/oras-go/v2/registry/remote/errcode"
)

// retryBase is the wait before the first retry; each further retry doubles it, within
// retryWait.
const retryBase = 250 * time.Millisecond

// retryTransport sends a request again when it is idempotent and failed in a way that another
// attempt may fix (transient), at most retries times, waiting backoff between attempts. The
// caller's context ends a wait. A request whose body cannot be replayed (no GetBody) is sent
// once. Each retry is logged at debug level with the method, the path and the cause: no
// header, query or body, so credentials and upload state stay out of the log.
type retryTransport struct {
	next    http.RoundTripper
	retries int
	log     *slog.Logger
}

// RoundTrip implements http.RoundTripper.
func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.retries <= 0 || !idempotent(req) {
		return t.next.RoundTrip(req)
	}
	ctx := req.Context()
	for attempt := 0; ; attempt++ {
		resp, err := t.next.RoundTrip(req)
		if attempt >= t.retries || !transient(ctx, resp, err) {
			return resp, err
		}
		wait, ok := backoff(attempt, resp)
		if !ok {
			return resp, err
		}
		next := req
		if req.Body != nil && req.Body != http.NoBody {
			if req.GetBody == nil {
				return resp, err
			}
			body, gerr := req.GetBody()
			if gerr != nil {
				return resp, err
			}
			next = req.Clone(ctx)
			next.Body = body
		}
		cause := "error"
		if err == nil {
			cause = strconv.Itoa(resp.StatusCode)
			discard(resp)
		}
		t.log.DebugContext(ctx, "registry request retried", "method", req.Method, "path", req.URL.Path,
			"attempt", attempt+1, "cause", cause, "wait", wait)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		req = next
	}
}

// discard reads a little of a failed answer's body, so that the connection can be reused, and
// closes it.
func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}

// idempotent reports whether sending req twice leaves the registry as sending it once: GET,
// HEAD and DELETE, and a PUT of a manifest, which is content addressed. A POST opens an upload
// session or mounts a blob, a PATCH appends to a session and a PUT to a session closes it:
// none of them is sent twice. A blob push as a whole is idempotent; Push retries it from the
// start (pushRetry).
func idempotent(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodOptions:
		return true
	case http.MethodPut:
		return operation(req.URL.Path) == "manifest"
	}
	return false
}

// transient reports whether a failed attempt is worth another one: a 429, a 5xx other than
// 501 and 505, a timeout, a connection refused or reset, a connection closed in the middle of
// an answer. Never another 4xx, and never once the caller's context has ended.
func transient(ctx context.Context, resp *http.Response, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		return transientErr(err)
	}
	return transientStatus(resp.StatusCode)
}

// transientStatus reports whether a registry's status is worth another attempt.
func transientStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusNotImplemented, http.StatusHTTPVersionNotSupported:
		return false
	}
	return status >= 500 && status <= 599
}

// transientErr reports whether a transport error is worth another attempt. The callers check
// their context first: a deadline of the caller's is a timeout too.
func transientErr(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

// backoff returns the wait before retry attempt+1: the Retry-After of a 429 or a 503, or
// retryBase doubled for each attempt with up to half of it as jitter, within retryWait. ok is
// false when the registry asks for a longer wait than retryWait.max: the failure is returned
// then, not waited out with a client's request.
func backoff(attempt int, resp *http.Response) (wait time.Duration, ok bool) {
	if resp != nil {
		if after, set := retryAfter(resp); set {
			if after > retryWait.max {
				return 0, false
			}
			return max(after, retryWait.min), true
		}
	}
	d := retryBase << min(attempt, 16)
	d = d/2 + rand.N(d/2+1) //nolint:gosec // jitter, not a secret
	return min(max(d, retryWait.min), retryWait.max), true
}

// retryAfter reads the Retry-After of a 429 or a 503, in seconds or as an HTTP date.
func retryAfter(resp *http.Response) (time.Duration, bool) {
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
		return 0, false
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second, true
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(time.Until(at), 0), true
	}
	return 0, false
}

// pushRetry runs push, a whole blob upload, again from the start of content when it failed in
// a way another attempt may fix, at most retries times: the requests of an upload are not
// idempotent one by one, the upload of a content-addressed blob is. Only content that can be
// rewound (an io.Seeker: a spooled file, bytes in memory) is pushed again; a stream is pushed
// once. ctx ends a wait.
func pushRetry(ctx context.Context, retries int, log *slog.Logger, content io.Reader, push func(io.Reader) error) error {
	seeker, ok := content.(io.Seeker)
	if !ok || retries <= 0 {
		return push(content)
	}
	start, err := seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return push(content)
	}
	for attempt := 0; ; attempt++ {
		err := push(content)
		if err == nil || attempt >= retries || ctx.Err() != nil || !transientPush(err) {
			return err
		}
		wait, _ := backoff(attempt, nil)
		log.DebugContext(ctx, "blob push retried from the start", "attempt", attempt+1, "wait", wait)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
		if _, serr := seeker.Seek(start, io.SeekStart); serr != nil {
			return err
		}
	}
}

// transientPush reports whether a failed blob push, as oras-go returns it, is worth another
// one: an error answer with a transient status, or a transient transport error.
func transientPush(err error) bool {
	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) {
		return transientStatus(resp.StatusCode)
	}
	return transientErr(err)
}

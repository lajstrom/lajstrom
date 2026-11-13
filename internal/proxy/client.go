package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/egress"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/version"
)

// The bounds of a request to an upstream (docs/spec/proxy-and-group.md §2.1, §9.2).
const (
	dialTimeout     = 10 * time.Second
	maxRedirects    = 5
	retryFirstWait  = 250 * time.Millisecond
	retryLongest    = 3 * time.Second
	retryAfterMost  = time.Minute
	retryAfterNone  = 5 * time.Second
	errorBodyToRead = 4 << 10
)

// errMissing is an upstream's 404 or 410: the name or the file is not there. It wraps
// index.ErrNotFound, so that a caller that passes it on answers 404.
var errMissing = fmt.Errorf("proxy: the upstream does not have it: %w", index.ErrNotFound)

// Upstream is one entry of upstreams[] with its credentials read and its own HTTP client. It
// sends Lajstrom's own requests only: the credentials go to the origin of its URL alone, a
// client's headers are never forwarded, and a host other than its own must be https and
// public (§9.2, egress.Guard). Its errors name it by its name, never by its URL.
type Upstream struct {
	name, format    string
	base            *url.URL
	timeout         time.Duration
	retries         int
	maxMetadataSize int64
	authorization   string // the value of Authorization on the upstream's origin; never logged
	client          *http.Client
	guard           *egress.Guard // trusts the upstream's host and the egress proxies the environment chose
	log             *slog.Logger
}

// UpstreamOptions are what NewUpstream takes besides the configuration.
type UpstreamOptions struct {
	// Authorization is the value of the Authorization header (Basic or Bearer), "" for none.
	Authorization string
	// TLS is the TLS configuration of the connections; nil means the system's roots.
	TLS *tls.Config
	// Proxy chooses the egress proxy of a request; nil means http.ProxyFromEnvironment
	// (HTTPS_PROXY, HTTP_PROXY, NO_PROXY).
	Proxy func(*http.Request) (*url.URL, error)
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
}

// NewUpstream returns the client of the upstream c, whose URL Load has checked.
func NewUpstream(c config.Upstream, o UpstreamOptions) (*Upstream, error) {
	base, err := url.Parse(c.URL)
	if err != nil {
		return nil, fmt.Errorf("proxy: upstreams %s: the URL does not parse", c.Name)
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	u := &Upstream{name: c.Name, format: c.Format, base: base, timeout: c.Timeout, retries: max(c.Retries, 0),
		maxMetadataSize: c.MaxMetadataSize, authorization: o.Authorization, guard: egress.NewGuard(base.Hostname()), log: o.Log}
	if u.timeout <= 0 {
		u.timeout = config.DefaultUpstreamTimeout
	}
	if u.maxMetadataSize <= 0 {
		u.maxMetadataSize = config.DefaultUpstreamMaxMetadataSize
	}
	if u.log == nil {
		u.log = slog.Default()
	}
	choose := o.Proxy
	if choose == nil {
		choose = http.ProxyFromEnvironment
	}
	dialer := &net.Dialer{Timeout: dialTimeout}
	tr := &http.Transport{
		Proxy:                 u.guard.Proxy(choose),
		DialContext:           u.guard.DialContext(dialer),
		TLSClientConfig:       o.TLS,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: u.timeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	}
	u.client = &http.Client{Transport: tr, CheckRedirect: u.checkRedirect}
	return u, nil
}

// Name is the upstream's name in the configuration.
func (u *Upstream) Name() string { return u.name }

// Format is the format the upstream serves.
func (u *Upstream) Format() string { return u.format }

// MaxMetadataSize is the largest document the upstream may answer for one name.
func (u *Upstream) MaxMetadataSize() int64 { return u.maxMetadataSize }

// Resolve returns ref, a path relative to the upstream's URL or an absolute URL, as an
// absolute URL.
func (u *Upstream) Resolve(ref string) (*url.URL, error) {
	r, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("%w: upstream %s: a URL that does not parse", format.ErrBadGateway, u.name)
	}
	return u.base.ResolveReference(r), nil
}

// Response is an upstream's answer whose headers have come: Body streams with the stall
// bound of the timeout, and Close releases it.
type Response struct {
	Status        int
	Header        http.Header
	Body          io.ReadCloser // nil for a 304, a 404 and a 410
	ContentLength int64
}

// Get sends a GET of target (absolute) with the request's own headers only: Accept, the
// validators etag and lastModified when they are not "", User-Agent, and Authorization on the
// upstream's own origin. It retries as upstreams[].retries says, and answers 200 and 206 with
// a body, 304 without, 404 and 410 with errMissing, and every other outcome with an error that
// wraps format.ErrBadGateway, format.ErrGatewayTimeout or a *format.UnavailableError (§6).
// operation (document, file) labels the metrics.
func (u *Upstream) Get(ctx context.Context, operation string, target *url.URL, accept, etag, lastModified string) (*Response, error) {
	return u.send(ctx, call{operation: operation, method: http.MethodGet, target: target,
		header: map[string]string{"Accept": accept, "If-None-Match": etag, "If-Modified-Since": lastModified}})
}

// call is one request to an upstream, as Get and Core.Forward build it.
type call struct {
	operation string            // the metrics label: document, file, forward
	method    string            // GET, or a forwarded request's
	target    *url.URL          // absolute
	header    map[string]string // the request's own headers; "" for none
	body      []byte            // nil for none; sent again on a retry, a 307 and a 308
}

// send sends c as Get says, retried as upstreams[].retries says; a request of another method
// than GET is answered with its body on any 2xx.
func (u *Upstream) send(ctx context.Context, c call) (*Response, error) {
	if err := u.guard.CheckURL(c.target); err != nil {
		return nil, u.transportError(ctx, err)
	}
	for attempt := 0; ; attempt++ {
		resp, err := u.once(ctx, c)
		if err == nil || attempt >= u.retries || !retryable(err) || ctx.Err() != nil {
			return resp, err
		}
		wait := min(retryFirstWait<<attempt, retryLongest)
		var busy *format.UnavailableError
		if errors.As(err, &busy) && busy.RetryAfter <= retryLongest {
			wait = busy.RetryAfter
		}
		u.log.DebugContext(ctx, "proxy: upstream request retried", "upstream", u.name, "path", c.target.Path, "wait", wait.String(),
			"error", err.Error())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// once sends one request and classifies its answer.
func (u *Upstream) once(parent context.Context, c call) (*Response, error) {
	ctx, cancel := context.WithCancel(parent)
	var body io.Reader
	if c.body != nil {
		body = bytes.NewReader(c.body) // and GetBody, for a 307 and a 308
	}
	req, err := http.NewRequestWithContext(ctx, c.method, c.target.String(), body)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: upstream %s: %s", format.ErrBadGateway, u.name, err.Error())
	}
	req.Header.Set("User-Agent", "lajstrom/"+version.Get().Version)
	for k, v := range c.header {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	u.authorize(req)
	start := time.Now()
	resp, err := u.client.Do(req)
	status := "error"
	if resp != nil {
		status = strconv.Itoa(resp.StatusCode)
	}
	metrics.ProxyUpstreamDuration.WithLabelValues(u.name, c.operation, status).Observe(time.Since(start).Seconds())
	u.log.DebugContext(ctx, "proxy: upstream request", "upstream", u.name, "method", c.method, "path", c.target.Path,
		"status", status, "duration", time.Since(start).String())
	if err != nil {
		cancel()
		return nil, u.transportError(parent, err)
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent ||
		c.method != http.MethodGet && resp.StatusCode/100 == 2 {
		return &Response{Status: resp.StatusCode, Header: resp.Header, ContentLength: resp.ContentLength,
			Body: newStallBody(resp.Body, u.timeout, cancel)}, nil
	}
	_, _ = io.CopyN(io.Discard, resp.Body, errorBodyToRead) // never logged, never in an error
	_ = resp.Body.Close()
	cancel()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return &Response{Status: resp.StatusCode, Header: resp.Header}, nil
	case http.StatusNotFound, http.StatusGone:
		return nil, errMissing
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("upstream %s answered 429: %w", u.name, &format.UnavailableError{RetryAfter: retryAfter(resp.Header)})
	case http.StatusUnauthorized, http.StatusForbidden:
		u.log.ErrorContext(ctx, "proxy: the upstream refused Lajstrom's credentials", "upstream", u.name, "status", resp.StatusCode)
	}
	return nil, &statusError{upstream: u.name, status: resp.StatusCode}
}

// statusError is an upstream's answer that is none of those Get passes on.
type statusError struct {
	upstream string
	status   int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("upstream %s answered %d", e.upstream, e.status)
}

// Unwrap returns format.ErrBadGateway.
func (e *statusError) Unwrap() error { return format.ErrBadGateway }

// transportError classifies an error of the client or of a body: a timeout is
// format.ErrGatewayTimeout, a refused target and every other failure format.ErrBadGateway; the
// end of ctx, the caller's, is returned as it is.
func (u *Upstream) transportError(ctx context.Context, err error) error {
	var ne net.Error
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, errStalled):
		return &timeoutError{upstream: u.name, cause: err}
	case errors.Is(err, egress.ErrRefused):
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // without the URL, which a redirect may have presigned (§9.2)
		}
		return fmt.Errorf("%w: upstream %s: %w", format.ErrBadGateway, u.name, err)
	}
	return &connError{upstream: u.name, cause: err}
}

// timeoutError is a request the upstream did not answer within its timeout.
type timeoutError struct {
	upstream string
	cause    error
}

func (e *timeoutError) Error() string {
	return "upstream " + e.upstream + " did not answer in time"
}

// Unwrap returns format.ErrGatewayTimeout; the cause is for retryable only.
func (e *timeoutError) Unwrap() error { return format.ErrGatewayTimeout }

// connError is a failure to connect or to talk to the upstream. Its message has no URL: a
// *url.Error names the one it was asked for.
type connError struct {
	upstream string
	cause    error
}

func (e *connError) Error() string {
	return "upstream " + e.upstream + " could not be reached"
}

// Unwrap returns format.ErrBadGateway.
func (e *connError) Unwrap() error { return format.ErrBadGateway }

// retryable reports whether another attempt may fix err: a 429, a 5xx but 501 and 505, a
// timeout, a connection refused or reset (upstreams[].retries).
func retryable(err error) bool {
	var (
		se *statusError
		te *timeoutError
		ce *connError
	)
	switch {
	case errors.As(err, &se):
		return se.status >= 500 && se.status != http.StatusNotImplemented && se.status != http.StatusHTTPVersionNotSupported
	case errors.Is(err, format.ErrUnavailable), errors.As(err, &te):
		return true
	case errors.As(err, &ce):
		return errors.Is(ce.cause, syscall.ECONNREFUSED) || errors.Is(ce.cause, syscall.ECONNRESET)
	}
	return false
}

// retryAfter reads the Retry-After of a 429 in seconds or as an HTTP date: at most a minute,
// 5 seconds when there is none or it does not parse.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	d := retryAfterNone
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		d = time.Duration(secs) * time.Second
	} else if at, err := http.ParseTime(v); err == nil {
		d = max(time.Until(at), 0)
	}
	return min(d, retryAfterMost)
}

// authorize sets Authorization on a request to the upstream's own origin, and removes it from
// any other: the credentials are the upstream's (§9.2).
func (u *Upstream) authorize(req *http.Request) {
	req.Header.Del("Authorization")
	if u.authorization != "" && sameOrigin(req.URL, u.base) {
		req.Header.Set("Authorization", u.authorization)
	}
}

// sameOrigin reports whether a and b have the same scheme, host and port.
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// checkRedirect follows at most maxRedirects redirects, never from https to http, to targets
// the guard allows, with the credentials on the upstream's origin only.
func (u *Upstream) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("%w: more than %d redirects", egress.ErrRefused, maxRedirects)
	}
	if prev := via[len(via)-1]; strings.EqualFold(prev.URL.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
		return fmt.Errorf("%w: a redirect from https to http", egress.ErrRefused)
	}
	if err := u.guard.CheckURL(req.URL); err != nil {
		return err
	}
	u.authorize(req)
	return nil
}

// errStalled is the cause of a body that made no progress within the timeout.
var errStalled = errors.New("proxy: the upstream sent nothing within its timeout")

// stallBody bounds every wait for the next bytes of a body by the timeout: a read that waits
// longer cancels the request. The time between reads is not a wait: a proxy fetch spends it on
// the registry, whose pace is not the upstream's (#694). Close releases the request.
type stallBody struct {
	rc      io.ReadCloser
	timer   *time.Timer
	timeout time.Duration
	cancel  context.CancelFunc
	stalled chan struct{}
}

func newStallBody(rc io.ReadCloser, timeout time.Duration, cancel context.CancelFunc) *stallBody {
	b := &stallBody{rc: rc, timeout: timeout, cancel: cancel, stalled: make(chan struct{})}
	var once sync.Once
	b.timer = time.AfterFunc(timeout, func() {
		once.Do(func() {
			close(b.stalled)
			cancel()
		})
	})
	b.timer.Stop() // the first read starts it
	return b
}

// Read reads the body with the bound running while it waits.
func (b *stallBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.timeout)
	n, err := b.rc.Read(p)
	b.timer.Stop()
	if err != nil && !errors.Is(err, io.EOF) {
		select {
		case <-b.stalled:
			return n, errStalled
		default:
		}
	}
	return n, err
}

// Close stops the bound and releases the request.
func (b *stallBody) Close() error {
	b.timer.Stop()
	err := b.rc.Close()
	b.cancel()
	return err
}

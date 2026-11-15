package harbor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"syscall"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/version"
)

const (
	// apiPath is the root of Harbor's REST API.
	apiPath = "/api/v2.0"
	// maxErrorBody bounds how much of an error answer is read to find its code.
	maxErrorBody = 8 << 10
	// maxDialTimeout bounds the dial and the TLS handshake, as the generic provider's.
	maxDialTimeout = 10 * time.Second
	// firstWait is the wait before the first retry; each next one doubles, at most maxWait.
	firstWait = 250 * time.Millisecond
	maxWait   = 3 * time.Second
)

// client sends requests to Harbor's REST API with registries[].auth as Basic credentials. It
// follows no redirect, so the credentials go to the registry's host only, and retries a GET,
// a HEAD or a DELETE that failed without an answer or with a 429 or a 5xx but 501 and 505,
// registries[].retries times.
type client struct {
	base     string // scheme://host
	registry string // registries[].name, for a *provider.RejectedError
	user     string
	pass     string
	http     *http.Client
	retries  int
	// wait is the wait before retry n (1, 2, …); the tests make it zero.
	wait func(n int) time.Duration
}

// newClient returns the client of the REST API of cfg, whose url is base, scheme://host.
func newClient(cfg config.Registry, base string) *client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = config.DefaultRegistryTimeout
	}
	retries := cfg.Retries
	switch {
	case retries == 0:
		retries = config.DefaultRegistryRetries
	case retries < 0:
		retries = 0
	}
	var t *http.Transport
	if d, ok := http.DefaultTransport.(*http.Transport); ok {
		t = d.Clone()
	} else {
		t = &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}
	}
	dial := min(timeout, maxDialTimeout)
	t.DialContext = (&net.Dialer{Timeout: dial, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = dial
	t.ResponseHeaderTimeout = timeout
	return &client{
		base:     base,
		registry: cfg.Name,
		user:     cfg.Auth.Username,
		pass:     cfg.Auth.Password,
		retries:  retries,
		wait:     backoff,
		http: &http.Client{
			Transport:     t,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// backoff is the wait before retry n: firstWait, doubled for each retry before, at most maxWait.
func backoff(n int) time.Duration {
	d := firstWait
	for i := 1; i < n && d < maxWait; i++ {
		d *= 2
	}
	return min(d, maxWait)
}

// do sends method to path below apiPath (escaped as it goes on the wire) with query and the
// JSON body (nil for none), and returns the answer, whatever its status; the caller closes its
// body. An error is one of the request, without its URL.
func (c *client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	u := c.base + apiPath + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	for n := 0; ; n++ {
		req, err := c.request(ctx, method, u, payload)
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		var ue *url.Error // names the URL, and so the project
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if !c.again(req, n, resp, err) {
			return resp, err
		}
		if resp != nil {
			drain(resp)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.wait(n + 1)):
		}
	}
}

// again reports whether req, sent n+1 times, goes again after resp or err: a GET, a HEAD or a
// DELETE, within the retries and the request's context, that got no answer or a 429 or a 5xx but
// 501 and 505.
func (c *client) again(req *http.Request, n int, resp *http.Response, err error) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodDelete:
	default:
		return false
	}
	return n < c.retries && req.Context().Err() == nil && retryable(resp, err)
}

// request is one request of do to the URL u, with the JSON payload (nil for none) and the
// credentials.
func (c *client) request(ctx context.Context, method, u string, payload []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "lajstrom/"+version.Get().Version)
	req.Header.Set("Accept", "application/json")
	// A project's name that is all digits would be read as its id.
	req.Header.Set("X-Is-Resource-Name", "true")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	return req, nil
}

// retryable reports whether a request that got resp or err may be sent again: no answer, or
// 429, or a 5xx but 501 and 505.
func retryable(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	s := resp.StatusCode
	return s == http.StatusTooManyRequests || s >= 500 && s != http.StatusNotImplemented && s != http.StatusHTTPVersionNotSupported
}

// apiError is an answer of the REST API, or a request that got none, translated into a
// sentinel of package provider. Its message names the operation, the Lajstrom name, the status
// and code of the answer, and what the operator may do, and leaves out the URL, the project
// and Harbor's own message, which names the project: format.WriteError puts the message of a
// 4xx error in the response, and clients never see the prefix.
type apiError struct {
	op     string // "list repositories raw/"
	kind   error  // a sentinel of package provider, or a *provider.RejectedError
	detail string // "HTTP 403, FORBIDDEN; the account needs repository:list in the project"
	cause  error  // the request's error; nil for an answer
}

func (e *apiError) Error() string {
	msg := "harbor: " + e.op + ": " + e.kind.Error()
	if e.detail != "" {
		msg += " (" + e.detail + ")"
	}
	return msg
}

func (e *apiError) Unwrap() []error {
	if e.cause != nil {
		return []error{e.kind, e.cause}
	}
	return []error{e.kind}
}

// translate returns the error of op for resp, an answer that is not the 2xx op wanted; need is
// the permission of the robot account that the request needs, for the hint of a 403. It closes
// nothing. The statuses:
//
//   - 401: provider.ErrUnauthorized, Harbor refused the credentials;
//   - 403: provider.ErrUnauthorized, the account lacks need (Harbor answers 403 too for a project
//     that does not exist, which the probe asks about first);
//   - 404: provider.ErrNotFound;
//   - 409, 412: provider.ErrConflict, what Harbor holds refuses the write: an immutability rule
//     (412 PRECONDITION), an artifact others refer to (412 VIOLATE_FOREIGN_KEY_CONSTRAINT), a
//     name that exists (409 CONFLICT);
//   - any other 4xx, and 503: a *provider.RejectedError, one of a 429 or 503 provider.ErrUnavailable
//     too, with the Retry-After it sent;
//   - any other 5xx: an error that wraps no sentinel.
func (c *client) translate(op string, resp *http.Response, need string) error {
	code := errorCode(resp)
	detail := "HTTP " + strconv.Itoa(resp.StatusCode)
	if code != "" {
		detail += ", " + code
	}
	e := &apiError{op: op, detail: detail}
	switch s := resp.StatusCode; {
	case s == http.StatusUnauthorized:
		e.kind = provider.ErrUnauthorized
	case s == http.StatusForbidden:
		e.kind = provider.ErrUnauthorized
		if need != "" {
			e.detail += "; the account needs " + need + " in the project"
		}
	case s == http.StatusNotFound:
		e.kind = provider.ErrNotFound
	case s == http.StatusConflict, s == http.StatusPreconditionFailed:
		e.kind = provider.ErrConflict
	case s >= 400 && s <= 499, s == http.StatusServiceUnavailable:
		rej := &provider.RejectedError{Registry: c.registry, Status: s, Code: code}
		if rej.Busy() {
			rej.RetryAfter = retryAfter(resp.Header.Get("Retry-After"))
		}
		e.kind, e.detail = rej, ""
	default:
		return fmt.Errorf("harbor: %s: %s", op, detail)
	}
	return e
}

// failed returns the error of op for err, a request that got no answer: provider.ErrUnavailable
// when the host name does not resolve, the connection was refused or reset, or the host cannot be
// reached; else err with op. A context's error stays its own.
func failed(op string, err error) error {
	var dns *net.DNSError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
	case errors.As(err, &dns), errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return &apiError{op: op, kind: provider.ErrUnavailable, cause: err}
	}
	return fmt.Errorf("harbor: %s: %w", op, err)
}

// errorCode is the code of the first error of Harbor's answer, {"errors":[{"code":…}]}, "" when
// it has none. It reads at most maxErrorBody of the body.
func errorCode(resp *http.Response) string {
	var body struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&body) != nil || len(body.Errors) == 0 {
		return ""
	}
	return body.Errors[0].Code
}

// retryAfter reads a Retry-After of seconds; zero for none or another form.
func retryAfter(v string) time.Duration {
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return 0
}

// drain reads what is left of a body, a little, and closes it, so that its connection is reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
}

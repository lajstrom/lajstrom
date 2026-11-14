package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lajstrom/lajstrom/internal/egress"
	"github.com/lajstrom/lajstrom/internal/version"
)

// The bounds of a delivery (docs/spec/webhooks.md §4).
const (
	DefaultTimeout     = 10 * time.Second
	MaxTimeout         = 60 * time.Second
	DefaultMaxAttempts = 8
	// ClaimStale is how old a worker's claim of a delivery is when another replica takes it
	// over: the bound of fetch_claims, well above MaxTimeout.
	ClaimStale    = 2 * time.Minute
	maxRedirects  = 5
	maxURL        = 2048
	answerToRead  = 4 << 10
	dialTimeout   = 10 * time.Second
	firstBackoff  = 30 * time.Second
	longest       = time.Hour
	jitterPercent = 10
)

// The words of the delivery log for a target the egress rules refuse. A refused address and a
// name that does not resolve read the same, so that the log is no oracle for an internal DNS.
const (
	refusedAddress   = "refused: the host has no public address"
	refusedDowngrade = "refused: a redirect from https to http"
	refusedRedirects = "refused: more than 5 redirects"
	refusedScheme    = "refused: http to a host that webhooks.allowedHosts does not list"
)

var (
	errDowngrade = errors.New(refusedDowngrade)
	errRedirects = errors.New(refusedRedirects)
	errScheme    = errors.New(refusedScheme)
)

// Options configure a Deliverer: the keys of webhooks in the configuration.
type Options struct {
	// AllowedHosts are dialed as they are: http and addresses that are not public allowed.
	AllowedHosts []string
	// Timeout bounds one attempt, the answer's headers and body; zero means DefaultTimeout, and
	// more than MaxTimeout is MaxTimeout.
	Timeout time.Duration
	// Proxy chooses the egress proxy of a request; nil means http.ProxyFromEnvironment.
	Proxy func(*http.Request) (*url.URL, error)
	// TLS is the TLS configuration of the connections; nil means the system's roots.
	TLS *tls.Config
	// Now is the clock of the signature's timestamp; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
}

// Deliverer sends the requests of webhooks (webhooks.md §2): signed, within its timeout, through
// the egress guard. Its methods may be called concurrently.
type Deliverer struct {
	client  *http.Client
	guard   *egress.Guard
	timeout time.Duration
	now     func() time.Time
	log     *slog.Logger
	agent   string
}

// NewDeliverer returns the Deliverer of o.
func NewDeliverer(o Options) *Deliverer {
	d := &Deliverer{guard: egress.NewGuard(o.AllowedHosts...), timeout: o.Timeout, now: o.Now, log: o.Log,
		agent: "lajstrom/" + version.Get().Version}
	if d.timeout <= 0 {
		d.timeout = DefaultTimeout
	}
	d.timeout = min(d.timeout, MaxTimeout)
	if d.now == nil {
		d.now = time.Now
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	choose := o.Proxy
	if choose == nil {
		choose = http.ProxyFromEnvironment
	}
	tr := &http.Transport{
		Proxy:                 d.guard.Proxy(choose),
		DialContext:           d.guard.DialContext(&net.Dialer{Timeout: dialTimeout}),
		TLSClientConfig:       o.TLS,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: d.timeout,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
	}
	d.client = &http.Client{Transport: tr, CheckRedirect: d.checkRedirect}
	return d
}

// CheckURL returns an error, for the client, when raw may not be a webhook's URL
// (webhooks.md §5): not absolute http or https with a host, with user information or a
// fragment, longer than 2048 bytes, or http or an address that is not public on a host that
// webhooks.allowedHosts does not list.
func (d *Deliverer) CheckURL(raw string) error {
	if len(raw) > maxURL {
		return fmt.Errorf("longer than %d bytes", maxURL)
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return errors.New("does not parse as a URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("must be an http or https URL")
	case u.Host == "" || u.Hostname() == "":
		return errors.New("must name a host")
	case u.User != nil:
		return errors.New("must not hold user information: Lajstrom signs its requests instead")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return errors.New("must not have a fragment")
	}
	if err := d.guard.CheckURL(u); err != nil {
		if u.Scheme != "https" {
			return errors.New("must be https, unless webhooks.allowedHosts lists its host")
		}
		return errors.New("names an address that is not public, and webhooks.allowedHosts does not list it")
	}
	return nil
}

// Result is the outcome of one attempt.
type Result struct {
	// Delivered is an answer 2xx.
	Delivered bool
	// Status is the status of the answer; 0 when none came.
	Status int
	// Error says why an attempt was not a delivery, in Lajstrom's words; "" when delivered.
	Error string
	// Refused is a target the egress rules refused: another attempt would be refused alike.
	Refused bool
}

// Attempt sends body, the event eventID, to target, signed with key, and reads the answer
// within the timeout. The end of ctx, the caller's, is returned as an error, not as a Result.
func (d *Deliverer) Attempt(ctx context.Context, target string, key []byte, eventID string, body []byte) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return Result{Error: "the URL does not parse", Refused: true}, nil
	}
	if err := d.guard.CheckURL(req.URL); err != nil { // the URL was checked when stored; the allowed hosts may have changed since
		if !strings.EqualFold(req.URL.Scheme, "https") {
			return Result{Error: refusedScheme, Refused: true}, nil
		}
		return Result{Error: refusedAddress, Refused: true}, nil
	}
	ts := d.now().Unix()
	req.Header.Set("Content-Type", "application/cloudevents+json; charset=utf-8")
	req.Header.Set("User-Agent", d.agent)
	req.Header.Set("webhook-id", eventID)
	req.Header.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("webhook-signature", Sign(key, eventID, ts, body))
	resp, err := d.client.Do(req)
	if err != nil {
		if parent := context.Cause(ctx); parent != nil && !errors.Is(parent, context.DeadlineExceeded) {
			return Result{}, parent
		}
		return d.classify(target, err), nil
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, answerToRead))
	switch {
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return Result{Status: resp.StatusCode, Error: "answered " + strconv.Itoa(resp.StatusCode)}, nil
	case err != nil && ctx.Err() != nil:
		return Result{Status: resp.StatusCode, Error: "timeout: the answer's body did not end in time"}, nil
	}
	return Result{Delivered: true, Status: resp.StatusCode}, nil
}

// classify turns an error of the client into the Result of webhooks.md §4. The error of a
// refused dial names the address the host resolved to: it is logged at debug for the operator
// and never stored.
func (d *Deliverer) classify(target string, err error) Result {
	var (
		dnsErr *net.DNSError
		netErr net.Error
	)
	host := ""
	if u, perr := url.Parse(target); perr == nil {
		host = u.Hostname()
	}
	switch {
	case errors.Is(err, errDowngrade):
		return Result{Error: refusedDowngrade, Refused: true}
	case errors.Is(err, errRedirects):
		return Result{Error: refusedRedirects, Refused: true}
	case errors.Is(err, errScheme):
		return Result{Error: refusedScheme, Refused: true}
	case errors.Is(err, egress.ErrRefused), errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		d.log.Debug("webhook: target refused", "host", host, "reason", unwrapURL(err))
		return Result{Error: refusedAddress, Refused: true}
	case errors.As(err, &dnsErr):
		return Result{Error: "the host could not be resolved now"}
	case errors.As(err, &netErr) && netErr.Timeout(), errors.Is(err, context.DeadlineExceeded):
		return Result{Error: "timeout"}
	case errors.Is(err, syscall.ECONNREFUSED):
		return Result{Error: "connection refused"}
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return Result{Error: "connection reset"}
	case isTLS(err):
		return Result{Error: "TLS: the receiver's certificate or handshake failed"}
	}
	return Result{Error: "the request failed"}
}

// unwrapURL is err without the URL a *url.Error names, which may carry a token in its path.
func unwrapURL(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// isTLS reports whether err is a failure of the TLS handshake or of the certificate.
func isTLS(err error) bool {
	var (
		recordErr *tls.RecordHeaderError
		certErr   *tls.CertificateVerificationError
	)
	return errors.As(err, &recordErr) || errors.As(err, &certErr) || strings.Contains(err.Error(), "tls:")
}

// checkRedirect follows a 307 or 308, at most maxRedirects times, never from https to http, to
// targets the guard allows; any other redirect is the answer.
func (d *Deliverer) checkRedirect(req *http.Request, via []*http.Request) error {
	if s := req.Response.StatusCode; s != http.StatusTemporaryRedirect && s != http.StatusPermanentRedirect {
		return http.ErrUseLastResponse
	}
	if len(via) >= maxRedirects {
		return errRedirects
	}
	if prev := via[len(via)-1]; strings.EqualFold(prev.URL.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
		return errDowngrade
	}
	if err := d.guard.CheckURL(req.URL); err != nil {
		if !strings.EqualFold(req.URL.Scheme, "https") {
			return errScheme
		}
		return err
	}
	return nil
}

// Sign returns the value of webhook-signature (Standard Webhooks): v1 and the base64 of
// HMAC-SHA256 over "<id>.<timestamp>.<body>" keyed with key.
func Sign(key []byte, id string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + strconv.FormatInt(timestamp, 10) + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Backoff is the wait after the attempt-th failed attempt: 30 s doubled at each attempt, at most
// an hour, plus up to 10 % of jitter.
func Backoff(attempt int) time.Duration {
	wait := firstBackoff
	for i := 1; i < attempt && wait < longest; i++ {
		wait *= 2
	}
	wait = min(wait, longest)
	return wait + time.Duration(rand.Int64N(int64(wait)*jitterPercent/100+1)) //nolint:gosec // jitter, not a secret
}

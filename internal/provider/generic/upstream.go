package generic

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"

	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Credential is what NewUpstream answers an upstream's challenges with: a username and its
// password, for Basic and for the token realm, or a static bearer token. The zero value is
// anonymous: an anonymous token where the registry asks for one.
type Credential struct {
	Username, Password string
	Token              string
}

// NewUpstream returns the Provider of the upstream registry of an oci proxy repo (ADR 0018,
// docs/spec/proxy-and-group.md §11.1), whose configuration Load has checked: the root URL,
// timeout, retries and tokenRealmHosts of cfg, and cred, which serve read from
// upstreams[].auth. Lajstrom only reads from it, through Repository's Resolve and Fetch and
// through OpenBlob without redirect: it has no instance prefix and is never probed. It differs
// from a backend's Provider in three ways:
//
//   - its redirects to another host are held to Options.PublicRedirects;
//   - a 401 or 403 of a repository read (a manifest, a blob, tags) after a token was granted,
//     which is how Docker Hub and quay.io answer for a repository that does not exist, is
//     provider.ErrNotFound, and so is any 401 or 403 when cred is anonymous (ghcr.io refuses the
//     token request then); a refused token request with credentials stays
//     provider.ErrUnauthorized;
//   - the ratelimit-limit and ratelimit-remaining headers of its answers set the gauges
//     lajstrom_proxy_upstream_ratelimit_* of cfg.Name, the window read from w=, with a warning
//     when fewer than 10 % are left.
func NewUpstream(cfg config.Upstream, cred Credential, log *slog.Logger) (*Provider, error) {
	if cfg.Format != config.FormatOCI {
		return nil, fmt.Errorf("generic: upstream %s is of format %s, not %s", cfg.Name, cfg.Format, config.FormatOCI)
	}
	reg := config.Registry{Name: cfg.Name, Type: Type, URL: cfg.URL, Timeout: cfg.Timeout, Retries: cfg.Retries,
		TokenRealmHosts: cfg.TokenRealmHosts, Auth: config.RegistryAuth{Username: cred.Username, Password: cred.Password}}
	o := Options{PublicRedirects: true, upstream: true}
	if cred.Token != "" {
		u, err := url.Parse(cfg.URL)
		if err != nil {
			return nil, errors.New("generic: url must be an absolute http(s) URL")
		}
		o.Credential = auth.StaticCredential(u.Host, auth.Credential{AccessToken: cred.Token})
	}
	p, err := NewWithOptions(reg, log, o)
	if err != nil {
		return nil, err
	}
	p.upstream = true
	p.anonymous = cred == Credential{}
	return p, nil
}

// upstreamError is the error rule of an upstream (NewUpstream): err, which translateAs made of
// cause, is provider.ErrNotFound instead of provider.ErrUnauthorized when the registry refused
// a repository read, or when the upstream has no credentials.
func (p *Provider) upstreamError(op string, err, cause error) error {
	if !errors.Is(err, provider.ErrUnauthorized) {
		return err
	}
	var resp *errcode.ErrorResponse
	read := errors.As(cause, &resp) && resp.URL != nil && strings.EqualFold(resp.URL.Host, p.host) &&
		strings.HasPrefix(resp.URL.Path, "/v2/") && operation(resp.URL.Path) != "other"
	if !read && !p.anonymous {
		return err
	}
	detail := "the upstream does not have it, or refuses Lajstrom's read of it"
	if resp != nil {
		detail = responseDetail(resp) + ": " + detail
	}
	return &backendError{op: op, kind: provider.ErrNotFound, detail: detail, cause: cause}
}

// rateLimitTransport reads the rate limit of an upstream's answers (NewUpstream): the
// ratelimit-limit and ratelimit-remaining headers that Docker Hub sends, "100;w=21600", set the
// gauges of the upstream; the window comes from w= or is left as it was. Crossing below 10 % of
// the limit is logged at warn, once until it is above again.
type rateLimitTransport struct {
	next     http.RoundTripper
	upstream string
	log      *slog.Logger
	low      atomic.Bool
}

// RoundTrip implements http.RoundTripper.
func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	limit, window, okLimit := rateLimitHeader(resp.Header.Get("ratelimit-limit"))
	remaining, _, okRemaining := rateLimitHeader(resp.Header.Get("ratelimit-remaining"))
	if okLimit {
		metrics.ProxyRateLimit.WithLabelValues(t.upstream).Set(float64(limit))
		if window > 0 {
			metrics.ProxyRateWindow.WithLabelValues(t.upstream).Set(float64(window))
		}
	}
	if okRemaining {
		metrics.ProxyRateRemaining.WithLabelValues(t.upstream).Set(float64(remaining))
	}
	if okLimit && okRemaining && limit > 0 {
		low := remaining*10 < limit
		if low && !t.low.Swap(true) {
			t.log.WarnContext(req.Context(), "an upstream's rate limit is nearly spent: a 429 serves the cached tags, or 503",
				"upstream", t.upstream, "remaining", remaining, "limit", limit, "windowSeconds", window)
		} else if !low {
			t.low.Store(false)
		}
	}
	return resp, nil
}

// rateLimitHeader reads a header of the form "<n>;w=<seconds>": n, the window (0 without w=),
// and whether n parsed.
func rateLimitHeader(v string) (n, window int64, ok bool) {
	parts := strings.Split(v, ";")
	n, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || n < 0 {
		return 0, 0, false
	}
	for _, param := range parts[1:] {
		if w, found := strings.CutPrefix(strings.TrimSpace(param), "w="); found {
			window, _ = strconv.ParseInt(w, 10, 64)
		}
	}
	return n, max(window, 0), true
}

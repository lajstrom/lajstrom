package generic

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/version"
)

const (
	// maxDialTimeout bounds the TCP connect and the TLS handshake; a shorter registries[].timeout
	// bounds them too.
	maxDialTimeout = 10 * time.Second
	// maxIdleConnsPerHost keeps connections to the registry open for concurrent pushes and
	// pulls; the default of net/http is 2.
	maxIdleConnsPerHost = 16
)

// retryWait bounds the wait between two attempts of a request; within the bounds, the
// backoff is exponential from retryBase with jitter, or the Retry-After of a 429 or a 503
// (backoff). Tests set both bounds to zero.
var retryWait = struct{ min, max time.Duration }{min: 200 * time.Millisecond, max: 3 * time.Second}

// clients are the HTTP clients of a provider. They share the transport; the two auth clients
// share the credentials and the token cache.
type clients struct {
	// follow is the client of oras-go: it follows redirects, so blob downloads that the
	// registry redirects to object storage work.
	follow *auth.Client
	// noFollow returns a redirect as the response, for OpenBlob and StatBlob, which decide
	// for each one whether the client gets it or the provider follows it.
	noFollow *auth.Client
	// anonymous is noFollow without the credentials and the token cache, for the hops of a
	// blob redirect that has left the registry's origin.
	anonymous *http.Client
	// header is what every request carries: the User-Agent.
	header http.Header
	// cache is the token cache of both; the upload sessions look into it (repository.prime).
	cache auth.Cache
	// credentials is true when the registry's challenges are answered with a username and
	// password.
	credentials bool
	// retries is how often a failed request, or a failed blob push, is tried again.
	retries int
	// breaker is the circuit breaker of the registry; every request goes through it.
	breaker *breaker
}

// newClients builds the clients for the registry at base (its URL: scheme and host:port) from
// cfg. Every request carries User-Agent lajstrom/<version>; with a username, the registry's
// Basic or bearer challenge is answered with the username and password, and tokens are cached.
// A non-nil rotating replaces the username and password (Options.Credential). A bearer
// challenge whose realm realmTransport refuses is an error instead. While pushForDelete
// reports true, a token is asked for push where oras-go asks it for delete
// (deleteScopeTransport).
func newClients(cfg config.Registry, base *url.URL, log *slog.Logger, rotating auth.CredentialFunc, pushForDelete func() bool) clients {
	timeout := timeoutOf(cfg)
	// The retry transport resends an idempotent request whose body can be replayed; a blob
	// upload is retried as a whole by Push (pushRetry). The breaker above it counts a request
	// once however often it was tried. Above both, the probe's ping sees the challenges.
	br := newBreaker(cfg.Name, log)
	rt := &deleteScopeTransport{on: pushForDelete, next: &realmTransport{registry: cfg.Name, base: base, hosts: realmHosts(cfg, base), next: &challengeTransport{next: &breakerTransport{b: br, next: &retryTransport{
		next:    &logTransport{next: &metricsTransport{next: newTransport(timeout), registry: cfg.Name}, log: log},
		retries: retries(cfg),
		log:     log,
	}}}}}
	var cred auth.CredentialFunc
	cache := auth.NewCache()
	switch {
	case rotating != nil:
		cred = func(ctx context.Context, hostport string) (auth.Credential, error) {
			c, err := rotating(ctx, hostport)
			if err != nil {
				return auth.EmptyCredential, &credentialError{err: err}
			}
			return c, nil
		}
		cache = &rotatingCache{Cache: cache, cred: cred}
	case cfg.Auth.Username != "":
		cred = auth.StaticCredential(base.Host, auth.Credential{Username: cfg.Auth.Username, Password: cfg.Auth.Password})
	}
	header := http.Header{"User-Agent": {"lajstrom/" + version.Get().Version}}
	noFollow := &http.Client{
		Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return clients{
		follow: &auth.Client{
			Client:     &http.Client{Transport: rt},
			Header:     header,
			Credential: cred,
			Cache:      cache,
		},
		noFollow: &auth.Client{
			Client:     noFollow,
			Header:     header,
			Credential: cred,
			Cache:      cache,
		},
		anonymous:   noFollow,
		header:      header,
		cache:       cache,
		credentials: cred != nil,
		retries:     retries(cfg),
		breaker:     br,
	}
}

// timeoutOf is registries[].timeout with its default, DefaultRegistryTimeout for 0.
func timeoutOf(cfg config.Registry) time.Duration {
	if cfg.Timeout <= 0 {
		return config.DefaultRegistryTimeout
	}
	return cfg.Timeout
}

// retries is registries[].retries with its default: DefaultRegistryRetries for 0, none for a
// negative value.
func retries(cfg config.Registry) int {
	switch {
	case cfg.Retries == 0:
		return config.DefaultRegistryRetries
	case cfg.Retries < 0:
		return 0
	}
	return cfg.Retries
}

// rotatingCache is the token cache of a rotating credential (Options.Credential). The scheme
// and the bearer tokens are cached as by the cache it wraps, but a Basic token is made from
// the credential for every request: oras-go's cache would send the first one until a 401.
type rotatingCache struct {
	auth.Cache
	cred auth.CredentialFunc
}

// GetToken implements auth.Cache.
func (c *rotatingCache) GetToken(ctx context.Context, registry string, scheme auth.Scheme, key string) (string, error) {
	if scheme != auth.SchemeBasic {
		return c.Cache.GetToken(ctx, registry, scheme, key)
	}
	cred, err := c.cred(ctx, registry)
	if err != nil {
		return "", err
	}
	if cred.Username == "" || cred.Password == "" {
		return "", auth.ErrBasicCredentialNotFound
	}
	return base64.StdEncoding.EncodeToString([]byte(cred.Username + ":" + cred.Password)), nil
}

// newTransport is the default transport of net/http with timeout as the bound on the wait for
// response headers, and on dialing and the TLS handshake when it is shorter than
// maxDialTimeout. A response body streams for as long as it takes.
func newTransport(timeout time.Duration) *http.Transport {
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
	t.MaxIdleConnsPerHost = maxIdleConnsPerHost
	// No Accept-Encoding: gzip. A blob is served as its own bytes: a registry that compresses
	// when offered (Nexus's docker repositories, for blobs that look like text) would cost the
	// Content-Length that the provider passes on, and the transport's unzipping the CPU.
	t.DisableCompression = true
	return t
}

// challengeKey is the context key under which a request asks challengeTransport for the
// challenge of a 401: a *string, which the first one fills.
type challengeKey struct{}

// challengeTransport keeps the WWW-Authenticate header of the first 401 of a request whose
// context asks for it (the probe's ping). The auth client answers a challenge itself, and the
// answer it returns in the end does not show the challenge.
type challengeTransport struct{ next http.RoundTripper }

// RoundTrip implements http.RoundTripper.
func (t *challengeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	if dst, ok := req.Context().Value(challengeKey{}).(*string); ok && *dst == "" {
		*dst = resp.Header.Get("WWW-Authenticate")
	}
	return resp, nil
}

// realmTransport refuses the bearer challenge of the registry when the token realm it names
// is not on the registry's host name nor on one of hosts, or is plain http for a registry on
// https: oras-go would send the registry's username and password there (threat model P-3), and
// it takes any public or private host as a realm. Another port of the registry's host is
// allowed, a token server beside the registry, as the dev stack's Harbor and GitLab have. A
// host of hosts (registries[].tokenRealmHosts and the vendor table's, realmHosts) is allowed
// over https only. The refusal is a *realmError, which names the registry and the realm's host,
// not its URL. The auth client never sees the challenge, so nothing is sent to the realm.
type realmTransport struct {
	next     http.RoundTripper
	registry string   // the registry's name, for the error
	base     *url.URL // the registry's URL
	hosts    []string // the other hosts a realm may be on, over https
}

// RoundTrip implements http.RoundTripper.
func (t *realmTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || !sameHostname(req.URL, t.base) {
		return resp, err
	}
	scheme, realm := challengeParam(resp.Header.Get("WWW-Authenticate"), "realm")
	if scheme != "bearer" {
		return resp, nil
	}
	if reason := refuseRealm(t.base, realm, t.hosts); reason != "" {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
		return nil, &realmError{registry: t.registry, reason: reason}
	}
	return resp, nil
}

// refuseRealm says why the registry at base may not name realm as its token realm, or is ""
// when it may: on the registry's host name, or over https on one of hosts.
func refuseRealm(base *url.URL, realm string, hosts []string) string {
	u, err := url.Parse(realm)
	switch {
	case err != nil || u.Host == "":
		return "the realm of its bearer challenge is not an absolute URL"
	case u.Scheme != "https" && u.Scheme != "http":
		return "the realm of its bearer challenge is not http(s)"
	case base.Scheme == "https" && u.Scheme != "https":
		return "the realm of its bearer challenge downgrades https to http"
	case sameHostname(u, base):
		return ""
	case !slices.ContainsFunc(hosts, func(h string) bool { return isHost(u.Hostname(), h) }):
		return fmt.Sprintf("the realm of its bearer challenge is on the host %s, neither the registry's nor one of its tokenRealmHosts", shownHost(u))
	case u.Scheme != "https":
		return fmt.Sprintf("the realm of its bearer challenge is on the host %s of its tokenRealmHosts but not https", shownHost(u))
	}
	return ""
}

// shownHost is the host name of u for an error, quoted; a name longer than any host name is
// cut, since the registry chose it.
func shownHost(u *url.URL) string {
	const maxHost = 253
	h := u.Hostname()
	if len(h) > maxHost {
		h = h[:maxHost] + "..."
	}
	return strconv.Quote(h)
}

// sameHostname reports whether a and b name one host, whatever their ports.
func sameHostname(a, b *url.URL) bool {
	return strings.EqualFold(strings.TrimSuffix(a.Hostname(), "."), strings.TrimSuffix(b.Hostname(), "."))
}

// realmError is a bearer challenge that realmTransport refused. translate drops what net/http
// and oras-go wrap around it, which names the request URL.
type realmError struct{ registry, reason string }

func (e *realmError) Error() string {
	return fmt.Sprintf("registry %q: %s; its credentials are not sent there", e.registry, e.reason)
}

// deleteScopeTransport asks a token for push where oras-go asks it for delete, while on reports
// true: the vendor's quirk (quirks.pushForDelete), Quay's. oras-go adds delete to the scope of
// every DELETE, of a manifest and of a referrers index it replaces, and pull,delete to the
// reads of a delete that cleans the tag schema up; Quay's token service knows pull, push and *
// only and answers any other action 400 INVALID_REQUEST, so that no token was issued and no
// DELETE sent. Its DELETE wants write, which push grants, and its own challenge to a DELETE
// names pull,push. A token request is told by its scope parameters, one per scope, which oras-go
// sends to the realm with a GET (fetchDistributionToken); no request of the distribution API
// has them, and Lajstrom sends no OAuth2 token request, which would POST them. The token is
// cached under the scopes oras-go asked. Every other registry gets the scopes as oras-go asks
// them: a token service that grants delete apart from push is not asked for more.
type deleteScopeTransport struct {
	next http.RoundTripper
	on   func() bool
}

// RoundTrip implements http.RoundTripper.
func (t *deleteScopeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || !t.on() {
		return t.next.RoundTrip(req)
	}
	q := req.URL.Query()
	scopes := q["scope"]
	asked, changed := pushForDelete(scopes)
	if !changed {
		return t.next.RoundTrip(req)
	}
	q["scope"] = asked
	r := req.Clone(req.Context())
	r.URL.RawQuery = q.Encode()
	return t.next.RoundTrip(r)
}

// pushForDelete returns scopes with the action delete of each repository scope replaced by push,
// merged as oras-go merges scopes (auth.CleanScopes), and whether any had delete.
func pushForDelete(scopes []string) ([]string, bool) {
	out := slices.Clone(scopes)
	changed := false
	for i, s := range out {
		resource, actions, ok := cutActions(s)
		if !ok || !strings.HasPrefix(resource, "repository:") {
			continue
		}
		list := strings.Split(actions, ",")
		if !slices.Contains(list, auth.ActionDelete) {
			continue
		}
		for j, a := range list {
			if a == auth.ActionDelete {
				list[j] = auth.ActionPush
			}
		}
		out[i] = resource + ":" + strings.Join(list, ",")
		changed = true
	}
	if !changed {
		return scopes, false
	}
	return auth.CleanScopes(out), true
}

// cutActions splits a scope, <type>:<name>:<actions>, before its actions; ok is false for one
// without a name and actions.
func cutActions(scope string) (resource, actions string, ok bool) {
	i := strings.LastIndex(scope, ":")
	if i < 0 || !strings.Contains(scope[:i], ":") {
		return "", "", false
	}
	return scope[:i], scope[i+1:], true
}

// logTransport logs every attempt of a request to the registry at debug level: method, path,
// status and duration. It never logs headers, the query (upload URLs carry state) or bodies,
// so credentials and tokens stay out of the log.
type logTransport struct {
	next http.RoundTripper
	log  *slog.Logger
}

// RoundTrip implements http.RoundTripper.
func (t *logTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if !t.log.Enabled(ctx, slog.LevelDebug) {
		return t.next.RoundTrip(req)
	}
	start := time.Now()
	resp, err := t.next.RoundTrip(req)
	attrs := []any{"method", req.Method, "path", req.URL.Path, "duration", time.Since(start)}
	if err != nil {
		t.log.DebugContext(ctx, "registry request failed", append(attrs, "error", err.Error())...)
		return resp, err
	}
	t.log.DebugContext(ctx, "registry request", append(attrs, "status", resp.StatusCode)...)
	return resp, nil
}

// metricsTransport records each request to the registry, each retry its own, in
// lajstrom_provider_request_duration_seconds: the registry's name, the operation the path names
// and the status, "error" when there was no answer. The time is to the response headers; a blob
// streams after it.
type metricsTransport struct {
	next     http.RoundTripper
	registry string
}

// RoundTrip implements http.RoundTripper.
func (t *metricsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.next.RoundTrip(req)
	status := "error"
	if err == nil {
		status = metrics.Status(resp.StatusCode)
	}
	metrics.ProviderDuration.WithLabelValues(t.registry, operation(req.URL.Path), req.Method, status).Observe(time.Since(start).Seconds())
	return resp, err
}

// operationMarkers are the path segments of the distribution API that name an operation. A
// repository name may contain any of them (a raw path a/manifests/b), so the operation is the
// marker found last in the path: what follows it, a reference, a digest or an upload id, has no
// slash. uploads comes before blobs, which it contains.
var operationMarkers = []struct{ marker, op string }{
	{"/blobs/uploads/", "upload"}, {"/blobs/", "blob"}, {"/manifests/", "manifest"},
	{"/tags/list", "tags"}, {"/referrers/", "referrers"},
}

// operation names the operation of a request path for the metrics: ping, catalog, manifest,
// blob, upload, tags, referrers, or other (a token realm, a redirect to the storage).
func operation(path string) string {
	switch {
	case path == "/v2/" || path == "/v2":
		return "ping"
	case strings.HasPrefix(path, "/v2/_catalog"):
		return "catalog"
	case !strings.HasPrefix(path, "/v2/"):
		return "other"
	}
	best, op := -1, "other"
	for _, m := range operationMarkers {
		if i := strings.LastIndex(path, m.marker); i > best {
			best, op = i, m.op
		}
	}
	return op
}

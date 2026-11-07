package generic

import (
	"log/slog"
	"net"
	"net/http"
	"time"

	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/lajstrom/lajstrom/internal/config"
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
// backoff is exponential from 250 ms with 10 % jitter, or the Retry-After of a 429. Tests set
// both bounds to zero.
var retryWait = struct{ min, max time.Duration }{min: 200 * time.Millisecond, max: 3 * time.Second}

// clients are the two HTTP clients of a provider. They share the transport, the credentials
// and the token cache.
type clients struct {
	// follow is the client of oras-go: it follows redirects, so blob downloads that the
	// registry redirects to object storage work.
	follow *auth.Client
	// noFollow returns a redirect as the response, for OpenBlob and StatBlob, which decide
	// for each one whether the client gets it or the provider follows it.
	noFollow *auth.Client
	// cache is the token cache of both; the upload sessions look into it (repository.prime).
	cache auth.Cache
	// credentials is true when the registry's challenges are answered with a username and
	// password.
	credentials bool
}

// newClients builds the clients for the registry at host (host:port, as in the URL) from cfg.
// Every request carries User-Agent lajstrom/<version>; with a username, the registry's Basic
// or bearer challenge is answered with the username and password, and tokens are cached.
func newClients(cfg config.Registry, host string, log *slog.Logger) clients {
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
	policy := &retry.GenericPolicy{
		Retryable: retry.DefaultPredicate, // 408, 429, 5xx, timeouts, connection resets
		Backoff:   retry.DefaultBackoff,
		MinWait:   retryWait.min,
		MaxWait:   retryWait.max,
		MaxRetry:  retries,
	}
	// The retry transport resends a request only when its body can be replayed (GetBody); a
	// streamed blob upload is sent once. Above it, the probe's ping sees the challenges.
	rt := &challengeTransport{next: &retry.Transport{
		Base:   &logTransport{next: newTransport(timeout), log: log},
		Policy: func() retry.Policy { return policy },
	}}
	var cred auth.CredentialFunc
	if cfg.Auth.Username != "" {
		cred = auth.StaticCredential(host, auth.Credential{Username: cfg.Auth.Username, Password: cfg.Auth.Password})
	}
	header := http.Header{"User-Agent": {"lajstrom/" + version.Get().Version}}
	cache := auth.NewCache()
	return clients{
		follow: &auth.Client{
			Client:     &http.Client{Transport: rt},
			Header:     header,
			Credential: cred,
			Cache:      cache,
		},
		noFollow: &auth.Client{
			Client: &http.Client{
				Transport: rt,
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			},
			Header:     header,
			Credential: cred,
			Cache:      cache,
		},
		cache:       cache,
		credentials: cred != nil,
	}
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

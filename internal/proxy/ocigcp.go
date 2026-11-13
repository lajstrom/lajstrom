package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
)

// The access token of an oci upstream with auth.gcp, from the GCP metadata server
// (docs/spec/proxy-and-group.md §11.1).
const (
	// gcpMetadataHost is the metadata server of Compute Engine, GKE (its metadata server with
	// Workload Identity) and Cloud Run; GCE_METADATA_HOST replaces it, as Google's libraries read it.
	gcpMetadataHost    = "169.254.169.254"
	gcpMetadataHostEnv = "GCE_METADATA_HOST"
	gcpTokenPath       = "/computeMetadata/v1/instance/service-accounts/default/token"
	// gcpUser is the Basic username of an access token at Artifact Registry and Container
	// Registry.
	gcpUser = "oauth2accesstoken"
	// gcpRefreshMargin is how long before its expiry the token is replaced. The metadata server
	// hands out a new one when the one it caches has 5 minutes left (Compute Engine's
	// documentation), so asking with 4 left gets the new one.
	gcpRefreshMargin = 4 * time.Minute
	// gcpRetryRefresh is the least time between two refreshes that fail while the token works.
	gcpRetryRefresh = 10 * time.Second
	// gcpDialTimeout bounds the connection to the metadata server, which is on the node.
	gcpDialTimeout = 2 * time.Second
	// gcpMaxAnswer bounds what is read of the metadata server's answer.
	gcpMaxAnswer = 64 << 10
)

// gcpTokens caches the access token of the workload's service account, which the metadata
// server gives, and replaces it gcpRefreshMargin before it expires, or at half its life when it
// lives less than two margins. A refresh that fails leaves the token in use while it works, asked
// again at most every gcpRetryRefresh. One request fetches at a time: while a token works, the
// others use it; without one, they wait. It never logs the token, and no error shows it. It is
// safe for concurrent use.
type gcpTokens struct {
	url     string // the token's URL at the metadata server
	client  *http.Client
	retries int           // further attempts after a 5xx, a 429 or a failed connection
	backoff time.Duration // the wait before the first of them, doubled each time, at most 3 s
	now     func() time.Time
	log     *slog.Logger

	fetching sync.Mutex // held for the request to the metadata server

	mu      sync.Mutex
	token   string
	expires time.Time
	refresh time.Time // when the next token is fetched
	retryAt time.Time // after a refresh failed, the next one waits until then
}

// newGCPTokens returns the gcpTokens of the metadata server at host (empty: gcpMetadataHost),
// with an upstream's timeout and retries (0 for their defaults, negative retries none). Its
// client never goes through an egress proxy and follows no redirect.
func newGCPTokens(host string, timeout time.Duration, retries int, log *slog.Logger) (*gcpTokens, error) {
	if host == "" {
		host = gcpMetadataHost
	}
	u, err := url.Parse("http://" + host + gcpTokenPath)
	if err != nil || u.Host != host || u.User != nil || strings.Contains(host, "/") {
		return nil, fmt.Errorf("%s must be a host and an optional port of the metadata server", gcpMetadataHostEnv)
	}
	if timeout <= 0 {
		timeout = config.DefaultUpstreamTimeout
	}
	switch {
	case retries == 0:
		retries = config.DefaultUpstreamRetries
	case retries < 0:
		retries = 0
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil, // the metadata server is on the node: never the egress proxy
			DialContext:           (&net.Dialer{Timeout: gcpDialTimeout}).DialContext,
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          1,
			IdleConnTimeout:       time.Minute,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       timeout,
	}
	return &gcpTokens{url: u.String(), client: client, retries: retries, backoff: 250 * time.Millisecond, now: time.Now, log: log}, nil
}

// gcpRegistry reports whether the URL is on a host of Google's registries: gcr.io, its regional
// hosts, or Artifact Registry's *.pkg.dev.
func gcpRegistry(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return host == "gcr.io" || strings.HasSuffix(host, ".gcr.io") || strings.HasSuffix(host, ".pkg.dev")
}

// basic is the generic.Credential.Basic of the upstream: oauth2accesstoken and the token.
func (g *gcpTokens) basic(ctx context.Context) (username, password string, err error) {
	token, err := g.get(ctx)
	if err != nil {
		return "", "", err
	}
	return gcpUser, token, nil
}

// get returns a token that works now, fetching one when there is none or the current one is
// due. A refresh that fails returns the current token while it works.
func (g *gcpTokens) get(ctx context.Context) (string, error) {
	if token, ok := g.cached(); ok {
		return token, nil
	}
	g.fetching.Lock()
	defer g.fetching.Unlock()
	if token, ok := g.cached(); ok { // another request fetched it while this one waited
		return token, nil
	}
	token, lifetime, err := g.fetch(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if err != nil {
		if g.token != "" && now.Before(g.expires) {
			g.retryAt = now.Add(gcpRetryRefresh)
			g.log.WarnContext(ctx, "GCP access token: refresh failed; the current token is used until it expires",
				"expires", g.expires, "error", err.Error())
			return g.token, nil
		}
		return "", err
	}
	g.token, g.expires, g.retryAt = token, now.Add(lifetime), time.Time{}
	g.refresh = g.expires.Add(-min(gcpRefreshMargin, lifetime/2))
	return token, nil
}

// cached returns the current token when no fetch is needed: it is not due, or it works and a
// refresh failed less than gcpRetryRefresh ago.
func (g *gcpTokens) cached() (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if g.token != "" && now.Before(g.expires) && (now.Before(g.refresh) || now.Before(g.retryAt)) {
		return g.token, true
	}
	return "", false
}

// fetch asks the metadata server for a token and its lifetime, again after a 5xx, a 429 or a
// failed connection, up to g.retries times.
func (g *gcpTokens) fetch(ctx context.Context) (string, time.Duration, error) {
	wait := g.backoff
	for attempt := 0; ; attempt++ {
		token, lifetime, again, err := g.fetchOnce(ctx)
		if err == nil || !again || attempt >= g.retries {
			return token, lifetime, err
		}
		select {
		case <-ctx.Done():
			return "", 0, err
		case <-time.After(wait):
		}
		wait = min(2*wait, 3*time.Second)
	}
}

// fetchOnce is one request for the token; again says whether its failure is worth another.
func (g *gcpTokens) fetchOnce(ctx context.Context) (token string, lifetime time.Duration, again bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.url, nil)
	if err != nil {
		return "", 0, false, fmt.Errorf("gcp metadata server: %w", err)
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := g.client.Do(req)
	if err != nil {
		return "", 0, ctx.Err() == nil, fmt.Errorf("gcp metadata server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode != http.StatusOK:
		return "", 0, resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests,
			fmt.Errorf("gcp metadata server: %d for the access token of the default service account", resp.StatusCode)
	case resp.Header.Get("Metadata-Flavor") != "Google":
		return "", 0, false, errors.New("gcp metadata server: the answer has no Metadata-Flavor: Google, so it is not the metadata server (" +
			gcpMetadataHostEnv + ")")
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, gcpMaxAnswer)).Decode(&answer); err != nil || answer.AccessToken == "" ||
		answer.ExpiresIn <= 0 {
		return "", 0, false, errors.New("gcp metadata server: the answer is not an access token with expires_in")
	}
	return answer.AccessToken, time.Duration(answer.ExpiresIn) * time.Second, false, nil
}

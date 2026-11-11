package facade

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestRegistryUnresolvable sends requests to the facade for a registry whose host name does
// not resolve (#561), the circuit breaker closed: a manifest read, the HEAD and the GET of a
// blob, an upload. Each is 503 UNAVAILABLE at once, without Retry-After, and is logged at
// warn, not as a failure.
func TestRegistryUnresolvable(t *testing.T) {
	prev := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("no name server in this test")
	}}
	t.Cleanup(func() { net.DefaultResolver = prev })
	reg, err := generic.New(config.Registry{Name: "local", Type: generic.Type, URL: "http://registry.lajstrom.test:5000", Retries: -1},
		slog.New(slog.DiscardHandler))
	must(t, err)
	e := newEnv(t, envOptions{registry: func(*providertest.Fake) provider.Provider { return reg }})
	blob := "/v2/oci/images/app/blobs/" + digest.FromString("blob").String()
	for _, tt := range []struct{ method, target string }{
		{http.MethodGet, "/v2/oci/images/app/manifests/v1"},
		{http.MethodHead, blob},
		{http.MethodGet, blob},
		{http.MethodPost, uploads},
	} {
		what := tt.method + " " + tt.target
		rr := e.do(e.h, tt.method, tt.target, nil)
		if msg := wantError(t, what, rr, http.StatusServiceUnavailable, codeUnavailable); msg != "Service Unavailable" {
			t.Errorf("%s: message %q, want the status text only", what, msg)
		}
		if ra := rr.Header().Get("Retry-After"); ra != "" {
			t.Errorf("%s: Retry-After %q, want none", what, ra)
		}
	}
	if logs := e.logs.String(); strings.Count(logs, "facade: registry unavailable") != 4 || strings.Contains(logs, "request failed") {
		t.Errorf("want four warnings, no error:\n%s", logs)
	}
}

// openCircuit returns the generic provider of a registry that answers every request with 503,
// after enough failed pings to open its circuit breaker: it refuses requests from then on.
func openCircuit(t *testing.T) provider.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	p, err := generic.New(config.Registry{Name: "local", Type: generic.Type, URL: srv.URL, Retries: -1}, slog.New(slog.DiscardHandler))
	must(t, err)
	for range 5 {
		_ = p.Ping(t.Context())
	}
	return p
}

// TestRegistryUnavailable sends requests to the facade while the circuit breaker of the
// registry is open: 503 UNAVAILABLE, which docker retries, with Retry-After, the seconds left
// until the breaker's trial request (at most its 30 s cooldown).
func TestRegistryUnavailable(t *testing.T) {
	reg := openCircuit(t)
	e := newEnv(t, envOptions{registry: func(*providertest.Fake) provider.Provider { return reg }})
	for _, tt := range []struct{ method, target string }{
		{http.MethodGet, "/v2/oci/images/app/manifests/v1"},
		{http.MethodPost, uploads},
	} {
		what := tt.method + " " + tt.target
		rr := e.do(e.h, tt.method, tt.target, nil)
		if msg := wantError(t, what, rr, http.StatusServiceUnavailable, codeUnavailable); msg != "Service Unavailable" {
			t.Errorf("%s: message %q, want the status text only", what, msg)
		}
		if s, err := strconv.Atoi(rr.Header().Get("Retry-After")); err != nil || s < 1 || s > 30 {
			t.Errorf("%s: Retry-After %q, want 1 to 30 seconds", what, rr.Header().Get("Retry-After"))
		}
	}
}

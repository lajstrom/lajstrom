package facade

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
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

// TestRegistryUnreachable sends requests to the facade for a registry that refuses connections,
// and for one that resets them (#593), the circuit breaker closed: a manifest read and push, the
// HEAD and the GET of a blob, an upload. Each is 503 UNAVAILABLE without Retry-After, as for a
// host name that does not resolve, and is logged at warn, not as a failure.
func TestRegistryUnreachable(t *testing.T) {
	refused, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	_ = refused.Close() // nothing listens there now
	resetting, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { _ = resetting.Close() })
	go func() {
		for {
			c, err := resetting.Accept()
			if err != nil {
				return
			}
			_, _ = http.ReadRequest(bufio.NewReader(c))
			_ = c.(*net.TCPConn).SetLinger(0)
			_ = c.Close()
		}
	}()
	for name, addr := range map[string]string{"refused": refused.Addr().String(), "reset": resetting.Addr().String()} {
		t.Run(name, func(t *testing.T) {
			reg, err := generic.New(config.Registry{Name: "local", Type: generic.Type, URL: "http://" + addr, Retries: -1},
				slog.New(slog.DiscardHandler))
			must(t, err)
			e := newEnv(t, envOptions{registry: func(*providertest.Fake) provider.Provider { return reg }})
			blob := "/v2/oci/images/app/blobs/" + digest.FromString("blob").String()
			body, _ := manifest(t, []byte("layer"), "unreachable")
			for _, tt := range []struct {
				method, target string
				body           []byte
			}{
				{http.MethodGet, "/v2/oci/images/app/manifests/v1", nil},
				{http.MethodPut, "/v2/oci/images/app/manifests/v1", body},
				{http.MethodHead, blob, nil},
				{http.MethodGet, blob, nil},
				{http.MethodPost, uploads, nil},
			} {
				what := tt.method + " " + tt.target
				rr := e.do(e.h, tt.method, tt.target, bytes.NewReader(tt.body), "Content-Type", ocispecManifest)
				if msg := wantError(t, what, rr, http.StatusServiceUnavailable, codeUnavailable); msg != "Service Unavailable" {
					t.Errorf("%s: message %q, want the status text only", what, msg)
				}
				if ra := rr.Header().Get("Retry-After"); ra != "" {
					t.Errorf("%s: Retry-After %q, want none", what, ra)
				}
			}
			if logs := e.logs.String(); strings.Count(logs, "facade: registry unavailable") != 5 || strings.Contains(logs, "request failed") {
				t.Errorf("want five warnings, no error:\n%s", logs)
			}
		})
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

// downIndex is a store whose database does not answer: its repo lookups fail as the store's
// do while Postgres is out.
type downIndex struct{ index.Store }

func (downIndex) GetRepo(context.Context, string, string) (*index.Repo, error) {
	return nil, fmt.Errorf("%w: repos: failed to connect to `host=db`: dial tcp: connection refused", index.ErrUnavailable)
}

// TestIndexUnavailable sends requests to the facade while the database does not answer: a
// read and an upload whose repo cannot be looked up, and a manifest push whose ingest fails so.
// Each is 503 UNAVAILABLE, which docker retries, with Retry-After: 5, and is logged at warn.
func TestIndexUnavailable(t *testing.T) {
	down := fmt.Errorf("ingest: claim: %w: tag_claims: dial tcp: connection refused", index.ErrUnavailable)
	e := newEnv(t, envOptions{ingest: stubIngester{down}})
	h := New(Options{Index: downIndex{e.st}, Providers: provider.Map{"local": e.reg}, Tokens: e.tokens,
		Log: slog.New(slog.NewTextHandler(e.logs, nil))})
	f := e.newRawFile("docs/e.txt", "the database is out")
	body, _ := f.body(t)
	for _, tt := range []struct {
		what string
		rr   *httptest.ResponseRecorder
	}{
		{"manifest read", e.do(h, http.MethodGet, "/v2/oci/images/app/manifests/v1", nil)},
		{"upload", e.do(h, http.MethodPost, uploads, nil)},
		{"manifest push", e.put(f, "t1", body)},
	} {
		if msg := wantError(t, tt.what, tt.rr, http.StatusServiceUnavailable, codeUnavailable); msg != "Service Unavailable" {
			t.Errorf("%s: message %q, want the status text only", tt.what, msg)
		}
		if got := tt.rr.Header().Get("Retry-After"); got != "5" {
			t.Errorf("%s: Retry-After %q, want 5", tt.what, got)
		}
	}
	if logs := e.logs.String(); strings.Count(logs, "facade: index unavailable") != 3 || strings.Contains(logs, "request failed") {
		t.Errorf("want three warnings, no error:\n%s", logs)
	}
}

package raw

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestRegistryRefusals uploads a file to a registry that refuses it in each way of
// formattest.Refusals (#494).
func TestRegistryRefusals(t *testing.T) {
	for _, r := range formattest.Refusals {
		t.Run(r.Name, func(t *testing.T) {
			e := newEnv(t, envOptions{})
			r.Apply(e.prov)
			r.Check(t, e.do(http.MethodPut, target("dir/file.txt"), strings.NewReader("content")))
		})
	}
}

// TestNotServedNamed: an upload that the registry takes and does not serve within the visibility
// timeout is 503 with Retry-After 30, and the message names what the registry does not serve
// yet, after the status text, so that the client tells a lagging registry from a lost push
// (#667).
func TestNotServedNamed(t *testing.T) {
	lag := &providertest.Lag{Fake: &providertest.Fake{RegistryName: "local"}, Manifests: -1}
	e := newEnv(t, envOptions{registry: lag, visible: 50 * time.Millisecond})
	rr := e.do(http.MethodPut, target("dir/file.txt"), strings.NewReader("content"))
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") != "30" {
		t.Fatalf("status %d, Retry-After %q; want 503, 30; body %s", rr.Code, rr.Header().Get("Retry-After"), rr.Body)
	}
	body := rr.Body.String()
	for _, want := range []string{"Service Unavailable; the registry took raw/", "/dir/file.txt@sha256:",
		" and does not serve the manifest, the tag ", " yet; the same request again completes it\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q does not have %q", body, want)
		}
	}
}

// TestRegistryRefusalsOverHTTP is TestRegistryRefusals through the generic provider, against a
// registry that knows no blob or manifest, takes blob uploads, and refuses the requests of one
// method and path with a status: its answer is the provider's refusal, Retry-After included,
// and nothing of the registry's own message reaches the client.
func TestRegistryRefusalsOverHTTP(t *testing.T) {
	tests := []struct {
		name, method, path string // the request the registry refuses: its method and a part of its path
		status             int
		retryAfter         string // the registry's
		want               formattest.Refusal
	}{
		{name: "blob over the registry's size", method: http.MethodPost, path: "/blobs/uploads/", status: http.StatusRequestEntityTooLarge,
			want: formattest.Refusal{Status: http.StatusRequestEntityTooLarge}},
		{name: "throttled", method: http.MethodPost, path: "/blobs/uploads/", status: http.StatusTooManyRequests, retryAfter: "9",
			want: formattest.Refusal{Status: http.StatusServiceUnavailable, RetryAfter: "9"}},
		{name: "busy, without Retry-After", method: http.MethodPut, path: "/manifests/", status: http.StatusServiceUnavailable,
			want: formattest.Refusal{Status: http.StatusServiceUnavailable, RetryAfter: "1"}},
		{name: "manifest refused", method: http.MethodPut, path: "/manifests/", status: http.StatusBadRequest,
			want: formattest.Refusal{Status: http.StatusBadGateway}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			blobs := map[string][]byte{} // the blobs the registry took, by digest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				defer mu.Unlock()
				_, d, isBlob := strings.Cut(r.URL.Path, "/blobs/")
				switch {
				case r.Method == tt.method && strings.Contains(r.URL.Path, tt.path):
					if tt.retryAfter != "" {
						w.Header().Set("Retry-After", tt.retryAfter)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tt.status)
					_, _ = fmt.Fprint(w, `{"errors":[{"code":"DENIED_BY_POLICY","message":"refused by registry.internal"}]}`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/"):
					w.Header().Set("Location", r.URL.Path+"1")
					w.WriteHeader(http.StatusAccepted)
				case r.Method == http.MethodPut:
					if d := r.URL.Query().Get("digest"); d != "" {
						blobs[d] = body
					}
					w.WriteHeader(http.StatusCreated)
				case isBlob && blobs[d] != nil:
					w.Header().Set("Content-Length", strconv.Itoa(len(blobs[d])))
					w.Header().Set("Docker-Content-Digest", d)
					_, _ = w.Write(blobs[d])
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			reg, err := generic.New(config.Registry{Name: "local", Type: generic.Type, URL: srv.URL, Retries: -1}, slog.New(slog.DiscardHandler))
			must(t, err)
			e := newEnv(t, envOptions{registry: reg})
			rr := e.do(http.MethodPut, target("dir/file.txt"), strings.NewReader("content"))
			tt.want.Name = tt.name
			tt.want.Check(t, rr)
			if strings.Contains(rr.Body.String(), "registry.internal") || strings.Contains(rr.Body.String(), srv.URL) {
				t.Errorf("the answer names the registry's internals: %s", rr.Body)
			}
		})
	}
}

// TestPutStalledOverHTTP is TestPutStalled through the generic provider: the client's body
// streams into the registry's upload, and the error of its read comes back through net/http
// and oras-go to a 408 whose message does not name the registry (#576).
func TestPutStalledOverHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/"):
			w.Header().Set("Location", r.URL.Path+"1")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPatch || r.Method == http.MethodPut:
			w.Header().Set("Location", r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	reg, err := generic.New(config.Registry{Name: "local", Type: generic.Type, URL: srv.URL}, slog.New(slog.DiscardHandler))
	must(t, err)
	e := newEnv(t, envOptions{registry: reg})
	rr := e.do(http.MethodPut, target("dir/file.txt"), formattest.Stalled(strings.NewReader(strings.Repeat("x", 1000))))
	formattest.CheckStalled(t, rr)
	if strings.Contains(rr.Body.String(), srv.URL) || strings.Contains(e.logs.String(), "level=ERROR") {
		t.Errorf("answer %q, log %s", rr.Body, e.logs)
	}
}

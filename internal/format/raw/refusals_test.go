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

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
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

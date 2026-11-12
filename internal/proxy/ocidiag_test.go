package proxy_test

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// TestOCIBlobRedirectHost: a blob that the upstream redirects to another host, a CDN, whose
// connection breaks is a 502 whose message and warn line name the CDN's host and say it was a
// redirect (#889).
func TestOCIBlobRedirectHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		cut  int // bytes the CDN sends before it closes the connection; -1 none, at the request
	}{
		{"the request", -1},
		{"the body", 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newOCIEnv(t, ociSettings)
			img := e.hub.image(t, "library/nginx", "1.27", "amd64")
			layer := img.blobs[0][1]
			blob := e.hub.bytesOf("library/nginx", layer.Digest)
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.cut >= 0 {
					w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
					_, _ = w.Write(blob[:tc.cut])
					w.(http.Flusher).Flush()
				}
				if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = c.Close()
				}
			}))
			t.Cleanup(cdn.Close)
			e.hub.mu.Lock()
			e.hub.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/blobs/"+layer.Digest.String()) {
					return false
				}
				http.Redirect(w, r, cdn.URL+"/blob?sig=secret", http.StatusTemporaryRedirect)
				return true
			}
			e.hub.mu.Unlock()
			var logs logBuffer
			c := e.core("a", proxy.Options{Log: slog.New(slog.NewTextHandler(&logs, nil))})
			_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
			must(t, err)
			_, _, err = c.OCIBlob(t.Context(), e.repo, "library/nginx", layer.Digest, true, false)
			host := strings.TrimPrefix(cdn.URL, "http://")
			if !errors.Is(err, format.ErrBadGateway) || !strings.Contains(err.Error(), host+", where the upstream redirected the blob") {
				t.Fatalf("a CDN that breaks: %v, want a 502 naming %s", err, host)
			}
			out := logs.String()
			if !strings.Contains(out, `msg="proxy: a blob not fetched from the upstream"`) || !strings.Contains(out, "host="+host+" redirected=true") {
				t.Errorf("the warn line does not name the redirect's host %s:\n%s", host, out)
			}
			if strings.Contains(out, "secret") || strings.Contains(err.Error(), "secret") {
				t.Errorf("the redirect's query is in the log or the error: %v\n%s", err, out)
			}
		})
	}
}

// TestOCIBreakerRetryAfter: while the upstream's circuit breaker is open, a blob is a 503 with a
// Retry-After of the time until the breaker lets a trial through, not a 502 without one (#889).
func TestOCIBreakerRetryAfter(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.core("a", proxy.Options{})
	_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	layer := img.blobs[0][1]
	e.hub.mu.Lock()
	e.hub.hook = func(w http.ResponseWriter, _ *http.Request) bool {
		hubError(w, http.StatusInternalServerError, "UNKNOWN")
		return true
	}
	e.hub.mu.Unlock()
	for i := range 5 { // the breaker's threshold
		if _, _, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", layer.Digest, true, false); !errors.Is(err, format.ErrBadGateway) {
			t.Fatalf("failure %d: %v, want format.ErrBadGateway", i+1, err)
		}
	}
	_, _, err = c.OCIBlob(t.Context(), e.repo, "library/nginx", layer.Digest, true, false)
	after, ok := provider.RetryAfter(err)
	if format.StatusOf(err) != http.StatusServiceUnavailable || !ok || after <= 0 || after > 30*time.Second {
		t.Fatalf("with the breaker open: %v (status %d, Retry-After %s), want 503 with the cooldown left", err, format.StatusOf(err), after)
	}
	h := http.Header{}
	format.SetRetryAfter(h, err)
	if v := h.Get("Retry-After"); v == "" || v == "0" {
		t.Errorf("Retry-After %q", v)
	}
}

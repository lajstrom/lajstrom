package generic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
)

// trickle is an upstream whose blob sends its first half at once, then a byte every gap; with
// stall, nothing after the first half, the connection held until the client leaves.
func trickle(t *testing.T, blob []byte, gap time.Duration, stall bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/blobs/") && !strings.Contains(r.URL.Path, "/manifests/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
		w.Header().Set("Docker-Content-Digest", digest.FromBytes(blob).String())
		half := len(blob) / 2
		_, _ = w.Write(blob[:half])
		w.(http.Flusher).Flush()
		if stall {
			<-r.Context().Done()
			return
		}
		for i := half; i < len(blob); i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(gap):
			}
			_, _ = w.Write(blob[i : i+1])
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestUpstreamStall: an upstream's body whose bytes stop coming, the connection open, is cut
// once a read waited upstreams[].timeout, with a timeout error; one that keeps moving, slower
// than the timeout in all, is read whole (proxy-and-group.md §2.1, #883). A caller that is slow
// to read is not cut either.
func TestUpstreamStall(t *testing.T) {
	const timeout = 300 * time.Millisecond
	blob := bytes.Repeat([]byte("layer"), 8)
	d := digest.FromBytes(blob)
	open := func(t *testing.T, srv *httptest.Server) io.ReadCloser {
		t.Helper()
		log, _ := testLogger()
		p, err := NewUpstream(config.Upstream{Name: "up", Format: config.FormatOCI, URL: srv.URL, Timeout: timeout, Retries: -1}, Credential{}, log)
		if err != nil {
			t.Fatal(err)
		}
		b, err := p.OpenBlob(context.Background(), "library/nginx", d, "", false)
		if err != nil {
			t.Fatalf("OpenBlob: %v", err)
		}
		return b.Body
	}

	t.Run("stalled", func(t *testing.T) {
		body := open(t, trickle(t, blob, 0, true))
		defer func() { _ = body.Close() }()
		start := time.Now()
		got, err := io.ReadAll(body)
		took := time.Since(start)
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("a stalled body: %d bytes, %v; want a timeout", len(got), err)
		}
		if errors.Is(err, context.Canceled) {
			t.Errorf("the stall is a context.Canceled: %v", err)
		}
		if len(got) != len(blob)/2 || took < timeout || took > timeout+2*time.Second {
			t.Errorf("a stalled body: %d bytes after %s, want %d after about %s", len(got), took, len(blob)/2, timeout)
		}
	})
	t.Run("slow", func(t *testing.T) {
		body := open(t, trickle(t, blob, timeout/4, false))
		defer func() { _ = body.Close() }()
		start := time.Now()
		got, err := io.ReadAll(body)
		if err != nil || !bytes.Equal(got, blob) {
			t.Fatalf("a body that keeps moving: %d bytes, %v; want the blob", len(got), err)
		}
		if took := time.Since(start); took < 2*timeout {
			t.Fatalf("the slow body took %s, not longer than the timeout in all", took)
		}
	})
	t.Run("slow caller", func(t *testing.T) {
		body := open(t, trickle(t, blob, 0, false))
		defer func() { _ = body.Close() }()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(body, buf); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * timeout) // the caller's own pause
		rest, err := io.ReadAll(body)
		if err != nil || !bytes.Equal(append(buf, rest...), blob) {
			t.Fatalf("after the caller's pause: %v", err)
		}
	})
}

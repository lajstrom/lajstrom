package proxy_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// The faults of a poor link to an oci upstream (docs/quickstart/oci-proxy.md §5), on the hub.

// upstream replaces the env's upstream by one of cfg as edit changes it: its timeout, its
// retries. Cores made after it use it.
func (e *ociEnv) upstream(t *testing.T, edit func(*config.Upstream)) {
	t.Helper()
	cfg := config.Upstream{Name: "hub", Format: config.FormatOCI, URL: e.hub.srv.URL, Retries: -1}
	edit(&cfg)
	client, err := generic.NewUpstream(cfg, generic.Credential{}, slog.New(slog.DiscardHandler))
	must(t, err)
	e.up = proxy.NewOCIUpstream("hub", client, true)
}

// stallAfter makes the hub's GETs of the blob or manifest d, whose bytes are b, send the
// headers and n bytes, then nothing, the connection held until the client leaves.
func (h *hub) stallAfter(d digest.Digest, mediaType string, b []byte, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/"+d.String()) {
			return false
		}
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Header().Set("Docker-Content-Digest", d.String())
		_, _ = w.Write(b[:n])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return true
	}
}

// bytesOf returns the bytes the hub holds of the blob d at repo.
func (h *hub) bytesOf(repo string, d digest.Digest) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.blobs[repo][d]
}

// TestOCIBlobStall: an upstream that sends part of a blob, then nothing with the connection
// open, is given up after upstreams[].timeout without a byte: the fetch ends with a gateway
// timeout, a request that waits on it gets it, the claim holder's tee fails, the claim is
// released and the backend holds nothing (proxy-and-group.md §2.1, §6; #883).
func TestOCIBlobStall(t *testing.T) {
	const timeout = 300 * time.Millisecond
	e := newOCIEnv(t, ociSettings)
	e.upstream(t, func(u *config.Upstream) { u.Timeout = timeout })
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.core("a", proxy.Options{})
	_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	layer := img.blobs[0][1]
	e.hub.stallAfter(layer.Digest, "application/octet-stream", e.hub.bytesOf("library/nginx", layer.Digest), int(layer.Size/2))

	start := time.Now()
	_, rc, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", layer.Digest, true, true)
	if err != nil || rc == nil {
		t.Fatalf("the claim holder: %v, %v; want the tee", rc, err)
	}
	defer func() { _ = rc.Close() }()
	waited := make(chan error, 1)
	go func() {
		_, _, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", layer.Digest, true, false)
		waited <- err
	}()
	got, err := io.ReadAll(rc)
	if err == nil || int64(len(got)) != layer.Size/2 {
		t.Errorf("the tee of a stalled body: %d bytes, %v; want the first half and an error", len(got), err)
	}
	err = <-waited
	if !errors.Is(err, format.ErrGatewayTimeout) {
		t.Fatalf("the request that waited: %v, want format.ErrGatewayTimeout", err)
	}
	if took := time.Since(start); took > timeout+2*time.Second {
		t.Errorf("the fetch ended after %s, want about %s", took, timeout)
	}
	if _, err := e.st.GetFetchClaim(t.Context(), e.repo.ID, "library/nginx", layer.Digest.String()); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the claim after the stall: %v, want it released", err)
	}
	if ok, _ := e.hub.blobHeld(t, e, "library/nginx", layer.Digest); ok {
		t.Error("the backend holds a blob whose upstream stalled")
	}
}

// TestOCIManifestStall: a manifest whose body stops the same way is a gateway timeout after
// upstreams[].timeout.
func TestOCIManifestStall(t *testing.T) {
	const timeout = 300 * time.Millisecond
	e := newOCIEnv(t, ociSettings)
	e.upstream(t, func(u *config.Upstream) { u.Timeout = timeout })
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	m := img.manifests[0]
	e.hub.mu.Lock()
	raw := e.hub.docs["library/nginx"][m.Digest.String()].raw
	e.hub.mu.Unlock()
	e.hub.stallAfter(m.Digest, m.MediaType, raw, len(raw)/2)
	c := e.core("a", proxy.Options{})
	start := time.Now()
	_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", m.Digest.String())
	if !errors.Is(err, format.ErrGatewayTimeout) {
		t.Fatalf("a stalled manifest: %v, want format.ErrGatewayTimeout", err)
	}
	if took := time.Since(start); took > timeout+2*time.Second {
		t.Errorf("the manifest failed after %s, want about %s", took, timeout)
	}
}

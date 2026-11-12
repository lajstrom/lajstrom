package proxy_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

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
	e.up = proxy.NewOCIUpstream("hub", client, true, proxy.OCIRetries(cfg.Retries))
}

// breaking is how the hub answers the GETs of a blob on a link that breaks (breakBlob): the
// i-th answer, from 0, sends cut(i) bytes of what it serves, or all of it when cut(i) is
// negative, and then closes the connection, or with stall holds it until the client leaves. A
// Range bytes=<n>- is answered 206 from n; with whole, 200 with the blob; with shift, a 206 that
// starts a byte later. The answers after the first wait until hold is closed, when it is set,
// so that a request can join the fetch before it ends. ranges records the Range of each GET, ""
// for none.
type breaking struct {
	cut   func(i int) int
	stall bool
	whole bool
	shift bool
	hold  chan struct{}

	mu     sync.Mutex
	ranges []string
}

// seen returns the Ranges of the GETs so far.
func (f *breaking) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ranges)
}

// breakBlob makes the hub answer the GETs of the blob d, whose bytes are b, as f says.
func (h *hub) breakBlob(d digest.Digest, b []byte, f *breaking) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/blobs/"+d.String()) {
			return false
		}
		rng := r.Header.Get("Range")
		f.mu.Lock()
		i := len(f.ranges)
		f.ranges = append(f.ranges, rng)
		f.mu.Unlock()
		if i > 0 && f.hold != nil {
			select {
			case <-f.hold:
			case <-r.Context().Done():
				return true
			}
		}
		w.Header().Set("Docker-Content-Digest", d.String())
		body, status := b, http.StatusOK
		if from, ok := strings.CutPrefix(rng, "bytes="); ok && !f.whole {
			n, _ := strconv.Atoi(strings.TrimSuffix(from, "-"))
			if f.shift {
				n++
			}
			body, status = b[n:], http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", n, len(b)-1, len(b)))
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		if n := f.cut(i); n >= 0 && n < len(body) {
			_, _ = w.Write(body[:n])
			w.(http.Flusher).Flush()
			if f.stall {
				<-r.Context().Done()
				return true
			}
			if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = c.Close() // the rest never comes: the client reads an unexpected EOF
			}
			return true
		}
		_, _ = w.Write(body)
		return true
	}
}

// TestOCIBlobResume: a blob whose upstream breaks the connection mid-body is resumed with Range
// from the byte reached, in the same stream: the backend holds the blob's bytes, the claim
// holder's tee gets every byte once, and a request that waits is served. A 200 or a 206 at
// another offset instead, or more resumes in a row without a byte than upstreams[].retries,
// fail the fetch, and the backend holds nothing (proxy-and-group.md §11.5, #885).
func TestOCIBlobResume(t *testing.T) {
	third := func(size int) func(int) int { return func(int) int { return size/3 + 1 } }
	for _, tc := range []struct {
		name    string
		retries int
		f       func(size int) *breaking
		ok      bool
		ranges  func(size int) []string
	}{
		{"resumed twice", -1, func(size int) *breaking { return &breaking{cut: third(size)} }, true,
			func(size int) []string {
				c := size/3 + 1
				return []string{"", fmt.Sprintf("bytes=%d-", c), fmt.Sprintf("bytes=%d-", 2*c)}
			}},
		{"stalled, resumed", -1, func(size int) *breaking { return &breaking{cut: third(size), stall: true} }, true,
			func(size int) []string {
				c := size/3 + 1
				return []string{"", fmt.Sprintf("bytes=%d-", c), fmt.Sprintf("bytes=%d-", 2*c)}
			}},
		{"200 instead of 206", -1, func(size int) *breaking { return &breaking{cut: third(size), whole: true} }, false,
			func(size int) []string { return []string{"", fmt.Sprintf("bytes=%d-", size/3+1)} }},
		{"206 at another offset", -1, func(size int) *breaking { return &breaking{cut: third(size), shift: true} }, false,
			func(size int) []string { return []string{"", fmt.Sprintf("bytes=%d-", size/3+1)} }},
		{"resumes without a byte", 1, func(size int) *breaking {
			return &breaking{cut: func(i int) int { return map[bool]int{true: size / 2, false: 0}[i == 0] }}
		}, false, func(size int) []string {
			r := fmt.Sprintf("bytes=%d-", size/2)
			return []string{"", r, r}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newOCIEnv(t, ociSettings)
			e.upstream(t, func(u *config.Upstream) { u.Retries, u.Timeout = tc.retries, 500*time.Millisecond })
			img := e.hub.image(t, "library/nginx", "1.27", "amd64")
			c := e.core("a", proxy.Options{})
			_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
			must(t, err)
			layer := img.blobs[0][1]
			blob := e.hub.bytesOf("library/nginx", layer.Digest)
			f := tc.f(len(blob))
			f.hold = make(chan struct{})
			e.hub.breakBlob(layer.Digest, blob, f)

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
			time.Sleep(100 * time.Millisecond) // the request joins the fetch, which the first resume holds
			close(f.hold)
			got, err := io.ReadAll(rc)
			werr := <-waited
			if want := tc.ranges(len(blob)); !slices.Equal(f.seen(), want) {
				t.Errorf("the GETs' Ranges: %q, want %q", f.seen(), want)
			}
			held := e.backendBytes(t, "library/nginx", layer)
			if !tc.ok {
				if err == nil || werr == nil || held != nil {
					t.Fatalf("tee %d bytes, %v; waiter %v; backend %d bytes: want the fetch failed and nothing held", len(got), err, werr,
						len(held))
				}
				return
			}
			if err != nil || !bytes.Equal(got, blob) {
				t.Errorf("the tee: %d of %d bytes, %v; want the blob once", len(got), len(blob), err)
			}
			if werr != nil {
				t.Errorf("the request that waited: %v", werr)
			}
			if !bytes.Equal(held, blob) {
				t.Errorf("the backend holds %d bytes, not the blob", len(held))
			}
		})
	}
}

// backendBytes returns the bytes the env's backend holds of the blob desc at path, nil for none.
func (e *ociEnv) backendBytes(t *testing.T, path string, desc ocispec.Descriptor) []byte {
	t.Helper()
	rr, err := e.reg.Repository(t.Context(), "oci/hub/"+path)
	must(t, err)
	rc, err := rr.Fetch(t.Context(), desc)
	if err != nil {
		return nil
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	must(t, err)
	return b
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

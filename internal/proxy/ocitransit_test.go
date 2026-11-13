package proxy_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// slowCopy makes the hub's first GET of the blob d, whose bytes are b, send half of them and the
// rest once gate is closed: a copy that takes as long as the test wants. The GETs after it get
// later at once, b when later is nil.
func (h *hub) slowCopy(d digest.Digest, b []byte, gate chan struct{}, later []byte) {
	var first atomic.Bool
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/blobs/"+d.String()) {
			return false
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Header().Set("Docker-Content-Digest", d.String())
		if first.CompareAndSwap(false, true) {
			_, _ = w.Write(b[:len(b)/2])
			w.(http.Flusher).Flush()
			select {
			case <-gate:
			case <-r.Context().Done():
				return true
			}
			_, _ = w.Write(b[len(b)/2:])
			return true
		}
		_, _ = w.Write(cmpOr(later, b))
		return true
	}
}

func cmpOr(a, b []byte) []byte {
	if a != nil {
		return a
	}
	return b
}

// transitEnv is an env whose upstream has most as maxConcurrentTransits (nil: none), its layer,
// a copy of it held back by gate, and the claim holder's tee, open.
type transitEnv struct {
	*ociEnv
	c     *proxy.Core
	layer ocispec.Descriptor
	blob  []byte
	gate  chan struct{}
}

func newTransitEnv(t *testing.T, most *int, later func(blob []byte) []byte) *transitEnv {
	t.Helper()
	e := newOCIEnv(t, ociSettings)
	e.upstream(t, func(u *config.Upstream) { u.MaxConcurrentTransits = most })
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	te := &transitEnv{ociEnv: e, c: e.core("a", proxy.Options{}), layer: img.blobs[0][1], gate: make(chan struct{})}
	te.blob = e.hub.bytesOf("library/nginx", te.layer.Digest)
	var l []byte
	if later != nil {
		l = later(te.blob)
	}
	e.hub.slowCopy(te.layer.Digest, te.blob, te.gate, l)
	_, _, err := te.c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	_, rc, err := te.c.OCIBlob(t.Context(), e.repo, "library/nginx", te.layer.Digest, true, true)
	if err != nil || rc == nil {
		t.Fatalf("the claim holder: %v, %v; want the tee", rc, err)
	}
	t.Cleanup(func() {
		te.open()
		_ = rc.Close()
	})
	return te
}

// open lets the claim holder's copy end.
func (te *transitEnv) open() {
	select {
	case <-te.gate:
	default:
		close(te.gate)
	}
}

// serve answers GETs of blobs of oci/hub as the facade does: the stream of OCIBlob when it gives
// one, the connection aborted on its error; the backend's bytes otherwise.
func (te *transitEnv) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := digest.Digest(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		desc, rc, err := te.c.OCIBlob(r.Context(), te.repo, "library/nginx", d, true, true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(desc.Size, 10))
		if rc == nil {
			_, _ = w.Write(te.backendBytes(t, "library/nginx", desc))
			return
		}
		defer func() { _ = rc.Close() }()
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, rc); err != nil {
			panic(http.ErrAbortHandler)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestOCITransitHeaders: a client whose transport waits 1 s for response headers, as containerd
// waits 30 s, asks for a blob whose copy runs: with transit it gets its headers at once and the
// blob's bytes, from a GET of its own; with maxConcurrentTransits 0 it waits for the copy, and
// gives up as before (#875).
func TestOCITransitHeaders(t *testing.T) {
	zero := 0
	for _, tc := range []struct {
		name    string
		cap     *int
		transit bool
	}{
		{"transit", nil, true},
		{"cap 0", &zero, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTransitEnv(t, tc.cap, nil)
			srv := te.serve(t)
			passed := fetches("transit")
			client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: time.Second}}
			start := time.Now()
			resp, err := client.Get(srv.URL + "/v2/oci/hub/library/nginx/blobs/" + te.layer.Digest.String())
			if !tc.transit {
				if err == nil {
					_ = resp.Body.Close()
					t.Fatalf("with transit off: %s, want the client to give up on the headers", resp.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("with transit: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			got, err := io.ReadAll(resp.Body)
			if err != nil || !bytes.Equal(got, te.blob) || time.Since(start) > time.Second {
				t.Fatalf("the transit: %d bytes after %s, %v; want the blob at once", len(got), time.Since(start), err)
			}
			if n := fetches("transit") - passed; n != 1 {
				t.Errorf("%v transits counted, want 1", n)
			}
			if ok, _ := te.hub.blobHeld(t, te.ociEnv, "library/nginx", te.layer.Digest); ok {
				t.Error("the backend holds the blob before the claim holder's copy ended: the transit wrote it")
			}
		})
	}
}

// TestOCITransitCap: with maxConcurrentTransits 2, two requests that join a running copy are in
// transit and a third waits for the copy, then is served from the backend.
func TestOCITransitCap(t *testing.T) {
	two := 2
	te := newTransitEnv(t, &two, nil)
	var open []io.ReadCloser
	for i := range 2 {
		_, rc, err := te.c.OCIBlob(t.Context(), te.repo, "library/nginx", te.layer.Digest, true, true)
		if err != nil || rc == nil {
			t.Fatalf("request %d under the cap: %v, %v; want a transit", i+1, rc, err)
		}
		open = append(open, rc)
	}
	third := make(chan error, 1)
	go func() {
		_, rc, err := te.c.OCIBlob(t.Context(), te.repo, "library/nginx", te.layer.Digest, true, true)
		if rc != nil {
			_ = rc.Close()
			err = errors.New("a transit beyond the cap")
		}
		third <- err
	}()
	select {
	case err := <-third:
		t.Fatalf("the request beyond the cap did not wait for the copy: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	for _, rc := range open {
		got, err := io.ReadAll(rc)
		if err != nil || !bytes.Equal(got, te.blob) {
			t.Errorf("a transit: %d bytes, %v", len(got), err)
		}
		_ = rc.Close()
	}
	te.open()
	if err := <-third; err != nil {
		t.Fatalf("the request that waited: %v", err)
	}
	if ok, _ := te.hub.blobHeld(t, te.ociEnv, "library/nginx", te.layer.Digest); !ok {
		t.Error("the backend does not hold the blob after the copy")
	}
}

// TestOCITransitRefused: bytes in transit that are not the blob abort the client's stream before
// its last byte and write proxy.reject; the claim holder's copy is not affected.
func TestOCITransitRefused(t *testing.T) {
	te := newTransitEnv(t, nil, func(b []byte) []byte { return append(bytes.Clone(b[:len(b)-1]), 'X') })
	_, rc, err := te.c.OCIBlob(t.Context(), te.repo, "library/nginx", te.layer.Digest, true, true)
	if err != nil || rc == nil {
		t.Fatalf("a request that joined: %v, %v; want a transit", rc, err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err == nil || len(got) >= len(te.blob) {
		t.Fatalf("other bytes in transit: %d of %d bytes, %v; want an error before the last byte", len(got), len(te.blob), err)
	}
	evs, err := te.st.ListAudit(t.Context(), index.AuditFilter{Action: "proxy.reject"})
	if err != nil || len(evs) != 1 {
		t.Fatalf("proxy.reject rows = %+v, %v; want one", evs, err)
	}
	te.open()
	for range 100 {
		if ok, _ := te.hub.blobHeld(t, te.ociEnv, "library/nginx", te.layer.Digest); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the backend does not hold the blob after the claim holder's copy")
}

// TestOCITransitResumed: a transit whose upstream connection breaks is resumed with Range from the
// byte it reached, as a fetch is (#885), and its client gets every byte once.
func TestOCITransitResumed(t *testing.T) {
	te := newTransitEnv(t, nil, nil) // the claim holder's GET holds the slow copy's hook
	f := &breaking{cut: func(int) int { return len(te.blob)/3 + 1 }}
	te.hub.breakBlob(te.layer.Digest, te.blob, f) // the GETs after it break after a third
	_, rc, err := te.c.OCIBlob(t.Context(), te.repo, "library/nginx", te.layer.Digest, true, true)
	if err != nil || rc == nil {
		t.Fatalf("a request that joined: %v, %v; want a transit", rc, err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, te.blob) {
		t.Fatalf("a transit resumed: %d of %d bytes, %v; want the blob once", len(got), len(te.blob), err)
	}
	c := len(te.blob)/3 + 1
	if want := []string{"", "bytes=" + strconv.Itoa(c) + "-", "bytes=" + strconv.Itoa(2*c) + "-"}; !slices.Equal(f.seen(), want) {
		t.Errorf("the transit's GETs: %q, want %q", f.seen(), want)
	}
}

// TestOCITransitOtherReplica: a request on a replica whose fetch waits on another replica's claim
// is answered by a transit while the other replica copies (the wait path of Core.claim).
func TestOCITransitOtherReplica(t *testing.T) {
	te := newTransitEnv(t, nil, nil)
	b := te.core("b", proxy.Options{})
	start := time.Now()
	_, rc, err := b.OCIBlob(t.Context(), te.repo, "library/nginx", te.layer.Digest, true, true)
	if err != nil || rc == nil {
		t.Fatalf("on the other replica: %v, %v; want a transit", rc, err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, te.blob) || time.Since(start) > 2*time.Second {
		t.Fatalf("the other replica's transit: %d bytes after %s, %v; want the blob", len(got), time.Since(start), err)
	}
}

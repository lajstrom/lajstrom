package generic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// The blob of the tests of the read path's quirks.
const quirkName = "raw/scratch/notes"

var quirkData = []byte("0123456789")

// nexusOCI makes f answer as Nexus's oci format (docs/registries/nexus.md, item 3): Server:
// Nexus/… on every answer and 500 for a GET with a Range, counted in the result; while down is
// set, every request is 503. It stores quirkData under quirkName.
func nexusOCI(f *fakeRegistry, down *atomic.Bool) *atomic.Int32 {
	var ranged atomic.Int32
	f.blobs["art/"+quirkName] = map[digest.Digest][]byte{digest.FromBytes(quirkData): quirkData}
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Server", "Nexus/3.98.1-01 (COMMUNITY)")
		switch {
		case down != nil && down.Load():
			writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "starting")
			return true
		case r.Header.Get("Range") == "":
			return false
		}
		ranged.Add(1)
		writeErr(w, http.StatusInternalServerError, "UNKNOWN", "IllegalArgumentException: Paths must start with a slash")
		return true
	}
	return &ranged
}

// openWhole fails t unless OpenBlob of quirkData with a Range is the whole blob, 200, without
// ranges. It does not stop the test: goroutines call it.
func openWhole(ctx context.Context, t *testing.T, p *Provider) {
	t.Helper()
	b, err := p.OpenBlob(ctx, quirkName, digest.FromBytes(quirkData), "bytes=2-4", false)
	if err != nil {
		t.Errorf("OpenBlob with a Range: %v", err)
		return
	}
	got, err := io.ReadAll(b.Body)
	_ = b.Body.Close()
	if err != nil || b.Status != http.StatusOK || !bytes.Equal(got, quirkData) || b.Size != int64(len(quirkData)) || b.ContentRange != "" || b.AcceptRanges {
		t.Errorf("OpenBlob with a Range = %+v, %q, %v; want the whole blob, 200, without ranges", b, got, err)
	}
}

// TestQuirksConcurrentFirstReads: reads that come together on a provider that has not probed
// wait for one probe, and each gets the vendor's quirks.
func TestQuirksConcurrentFirstReads(t *testing.T) {
	ctx := context.Background()
	f := newFakeRegistry(t)
	ranged := nexusOCI(f, nil)
	log, _ := testLogger()
	p := f.provider(t, log, func(c *config.Registry) { c.Retries = -1 })
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { openWhole(ctx, t, p) })
	}
	wg.Wait()
	if n := f.count("GET /v2/_catalog"); n != 1 {
		t.Errorf("%d probes, want one:\n%s", n, strings.Join(f.log(), "\n"))
	}
	if n := ranged.Load(); n != 0 {
		t.Errorf("%d requests with a Range", n)
	}
}

// TestQuirksDown: while the registry does not answer, a read fails at once with the ping's
// provider.ErrUnavailable and sends nothing more; once it answers again, the first read probes
// and gets the vendor's quirks, also when the probe at start found it down (#797).
func TestQuirksDown(t *testing.T) {
	ctx := context.Background()
	f := newFakeRegistry(t)
	var down atomic.Bool
	down.Store(true)
	ranged := nexusOCI(f, &down)
	log, _ := testLogger()
	p := f.provider(t, log, func(c *config.Registry) { c.Retries = -1 })
	if _, err := p.Capabilities(ctx); err == nil {
		t.Fatal("Capabilities succeeded; the registry is down")
	}
	for range 2 {
		if _, err := p.OpenBlob(ctx, quirkName, digest.FromBytes(quirkData), "bytes=2-4", false); !errors.Is(err, provider.ErrUnavailable) {
			t.Errorf("OpenBlob while the registry is down: %v, want ErrUnavailable", err)
		}
	}
	if n, blobs := pings(f), f.count("GET /v2/art/"+quirkName+"/blobs/"); n != 3 || blobs != 0 {
		t.Errorf("%d pings and %d blob requests, want one ping each for the start and two reads, no blob request:\n%s",
			n, blobs, strings.Join(f.log(), "\n"))
	}
	down.Store(false)
	openWhole(ctx, t, p)
	if n := ranged.Load(); n != 0 {
		t.Errorf("%d requests with a Range", n)
	}
}

// TestQuirksProbeBackoff: after a probe of the read path failed though the registry answered its
// ping, the reads within probeBackoff go without probing and without the quirks, as before
// #797; the first read after it probes again.
func TestQuirksProbeBackoff(t *testing.T) {
	ctx := context.Background()
	f := newFakeRegistry(t)
	nexusOCI(f, nil)
	nexus := f.hook
	var broken atomic.Bool
	broken.Store(true)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if broken.Load() && r.URL.Path == "/v2/_catalog" {
			writeErr(w, http.StatusInternalServerError, "UNKNOWN", "the catalog fails")
			return true
		}
		return nexus(w, r)
	}
	log, _ := testLogger()
	p := f.provider(t, log, func(c *config.Registry) { c.Retries = -1 })
	for range 3 {
		b, err := p.OpenBlob(ctx, quirkName, digest.FromBytes(quirkData), "", false)
		if err != nil {
			t.Fatalf("OpenBlob without a Range: %v", err)
		}
		_ = b.Body.Close()
	}
	if n := f.count("GET /v2/_catalog"); n != 1 {
		t.Errorf("%d probes by three reads within the backoff, want one:\n%s", n, strings.Join(f.log(), "\n"))
	}
	broken.Store(false)
	p.mu.Lock()
	p.failedAt = time.Now().Add(-probeBackoff)
	p.mu.Unlock()
	openWhole(ctx, t, p)
	if n := f.count("GET /v2/_catalog"); n != 2 {
		t.Errorf("%d probes, want a second one after the backoff", n)
	}
}

// pings counts the probes' pings in the log of f.
func pings(f *fakeRegistry) int {
	n := 0
	for _, r := range f.log() {
		if r == "GET /v2/" {
			n++
		}
	}
	return n
}

// TestQuirksBoundedWait: a read waits for a probe that does not end at most registries[].timeout,
// not the probe's retries, and goes on without the quirks.
func TestQuirksBoundedWait(t *testing.T) {
	ctx := context.Background()
	f := newFakeRegistry(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // before the server closes
	f.blobs["art/"+quirkName] = map[digest.Digest][]byte{digest.FromBytes(quirkData): quirkData}
	f.hook = func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v2/_catalog" {
			return false
		}
		select { // the catalog probe never answers
		case <-r.Context().Done():
		case <-release:
		}
		return true
	}
	log, _ := testLogger()
	const timeout = 200 * time.Millisecond
	p := f.provider(t, log, func(c *config.Registry) { c.Timeout, c.Retries = timeout, 5 })
	start := time.Now()
	b, err := p.OpenBlob(ctx, quirkName, digest.FromBytes(quirkData), "bytes=2-4", false)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	_ = b.Body.Close()
	if b.Status != http.StatusPartialContent {
		t.Errorf("OpenBlob status %d, want 206: no quirks", b.Status)
	}
	// Six attempts of the catalog probe would take 1.2 s.
	if took > 4*timeout {
		t.Errorf("OpenBlob took %s, want about %s", took, timeout)
	}
}

// TestQuirksProbeAbandoned: a probe that ends with the context of the caller that ran it
// neither fails a read that waited for it nor holds back the next: the read probes itself.
func TestQuirksProbeAbandoned(t *testing.T) {
	f := newFakeRegistry(t)
	ranged := nexusOCI(f, nil)
	nexus := f.hook
	started := make(chan struct{})
	var first atomic.Bool
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v2/" && first.CompareAndSwap(false, true) {
			close(started)
			<-r.Context().Done() // the first ping hangs until its caller leaves
			return true
		}
		return nexus(w, r)
	}
	log, _ := testLogger()
	p := f.provider(t, log, func(c *config.Registry) { c.Retries = -1 })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := p.Capabilities(ctx)
		done <- err
	}()
	<-started
	read := make(chan struct{})
	go func() {
		defer close(read)
		openWhole(context.Background(), t, p)
	}()
	cancel()
	if err := <-done; err == nil {
		t.Error("Capabilities with a cancelled context succeeded")
	}
	<-read
	if n := ranged.Load(); n != 0 {
		t.Errorf("%d requests with a Range", n)
	}
}

// TestBlobExistsByGETFirst: on JFrog, a blob's Exists that is the provider's first request asks
// GET, not HEAD, which says yes for a blob GET does not serve.
func TestBlobExistsByGETFirst(t *testing.T) {
	ctx := context.Background()
	f := newFakeRegistry(t)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("X-Jfrog-Version", "Artifactory/7.161.26 86126900")
		if r.Method == http.MethodHead && strings.Contains(r.URL.Path, "/blobs/") {
			w.Header().Set("Content-Length", "9")
			w.WriteHeader(http.StatusOK)
			return true
		}
		return false
	}
	log, _ := testLogger()
	p := f.provider(t, log, nil)
	rr, err := p.Repository(ctx, quirkName)
	if err != nil {
		t.Fatal(err)
	}
	absent := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromString("elsewhere"), Size: 9}
	if ok, err := rr.Exists(ctx, absent); ok || err != nil {
		t.Errorf("Exists of a blob GET does not serve = %v, %v; want false", ok, err)
	}
	if n := f.count("HEAD /v2/art/" + quirkName + "/blobs/"); n != 0 {
		t.Errorf("%d HEAD requests of blobs, want none", n)
	}
}

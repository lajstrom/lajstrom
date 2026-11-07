package facade

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// heapLimit is the most heap in use the 1 GiB transfers may reach.
const heapLimit = 64 << 20

// TestStreamingMemory uploads a 1 GiB blob through POST and PUT and downloads it through GET,
// over real connections, and checks that the heap in use stays under heapLimit: the facade
// streams both ways and buffers nothing. The fake hashes and discards the upload (it keeps no
// blob over providertest.MaxKept); the download is a generated stream of the same bytes,
// counted and hashed on the client side.
func TestStreamingMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 2 GiB; skipped under -short")
	}
	const size = 1 << 30
	d := patternDigest(size)
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider { return generated{Fake: f, d: d, size: size} }})
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	peak := sampleHeap(t)

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+uploads, nil)
	hdr := send(t, req, http.StatusAccepted)
	req, _ = http.NewRequestWithContext(ctx, http.MethodPut, srv.URL+hdr.Get("Location")+"?digest="+d.String(), newPattern(size))
	req.ContentLength = size
	send(t, req, http.StatusCreated)

	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v2/oci/images/app/blobs/"+d.String(), nil)
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.ContentLength != size {
		t.Fatalf("GET: status %d, Content-Length %d", resp.StatusCode, resp.ContentLength)
	}
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	must(t, err)
	if n != size || digest.NewDigest(digest.SHA256, h) != d {
		t.Errorf("downloaded %d bytes with digest %s, want %d with %s", n, digest.NewDigest(digest.SHA256, h), size, d)
	}
	top := peak()
	t.Logf("peak heap in use: %d MiB", top>>20)
	if top > heapLimit {
		t.Errorf("peak heap in use %d MiB, want under %d MiB", top>>20, heapLimit>>20)
	}
}

// send sends req, checks the status and returns the header of the answer.
func send(t *testing.T, req *http.Request, want int) http.Header {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d", req.Method, req.URL.Path, resp.StatusCode, want)
	}
	return resp.Header
}

// sampleHeap samples HeapInuse every 10 ms from now on; the function it returns stops and
// gives the peak.
func sampleHeap(t *testing.T) func() uint64 {
	t.Helper()
	runtime.GC()
	var peak atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	})
	var once sync.Once
	finish := func() uint64 {
		once.Do(func() { close(stop); wg.Wait() })
		return peak.Load()
	}
	t.Cleanup(func() { finish() })
	return finish
}

// patternBlock is the block that pattern repeats.
var patternBlock = func() []byte {
	b := make([]byte, 64<<10+7) // not a power of two, so the block's seams fall anywhere
	x := uint64(24)
	for i := range b {
		x = x*6364136223846793005 + 1442695040888963407 // an LCG: bytes that do not repeat soon
		b[i] = byte(x >> 56)
	}
	return b
}()

// pattern is a deterministic stream of size bytes, as an io.ReadSeekCloser that holds no
// more than the block it repeats.
type pattern struct {
	size, off int64
}

func newPattern(size int64) *pattern { return &pattern{size: size} }

func (p *pattern) Read(b []byte) (int, error) {
	if p.off >= p.size {
		return 0, io.EOF
	}
	b = b[:min(int64(len(b)), p.size-p.off)]
	n := 0
	for n < len(b) {
		n += copy(b[n:], patternBlock[(p.off+int64(n))%int64(len(patternBlock)):])
	}
	p.off += int64(n)
	return n, nil
}

func (p *pattern) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += p.off
	case io.SeekEnd:
		offset += p.size
	default:
		return 0, errors.New("pattern: invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("pattern: negative offset")
	}
	p.off = offset
	return offset, nil
}

func (p *pattern) Close() error { return nil }

// patternDigest is the sha256 digest of the first size bytes of pattern.
func patternDigest(size int64) digest.Digest {
	h := sha256.New()
	_, _ = io.Copy(h, newPattern(size))
	return digest.NewDigest(digest.SHA256, h)
}

// generated is the fake whose repositories serve the blob d as a pattern of size bytes:
// downloads of a blob the fake discarded on upload.
type generated struct {
	*providertest.Fake
	d    digest.Digest
	size int64
}

func (g generated) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := g.Fake.Repository(ctx, name)
	return generatedRepository{Repository: rr, g: g}, err
}

type generatedRepository struct {
	provider.Repository
	g generated
}

func (r generatedRepository) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if desc.Digest == r.g.d {
		if ok, err := r.Exists(ctx, desc); err != nil || !ok {
			return nil, fmt.Errorf("generated: blob %s was not uploaded: %w", desc.Digest, provider.ErrNotFound)
		}
		return newPattern(r.g.size), nil
	}
	return r.Repository.Fetch(ctx, desc)
}

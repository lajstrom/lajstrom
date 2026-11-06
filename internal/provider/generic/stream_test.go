package generic

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
)

// generatorBlock is the pattern a generator repeats: SHA-256 of a counter, so it does not
// compress and is the same in every run. Its length is a prime, so the pattern does not line
// up with any buffer size.
var generatorBlock = sync.OnceValue(func() []byte {
	b := make([]byte, 0, 65521+sha256.Size)
	var counter [8]byte
	for i := uint64(0); len(b) < 65521; i++ {
		binary.BigEndian.PutUint64(counter[:], i)
		sum := sha256.Sum256(counter[:])
		b = append(b, sum[:]...)
	}
	return b[:65521]
})

// generator reads size bytes of a fixed pattern without holding more than one block.
type generator struct {
	size, off int64
}

func (g *generator) Read(p []byte) (int, error) {
	if g.off >= g.size {
		return 0, io.EOF
	}
	if rem := g.size - g.off; int64(len(p)) > rem {
		p = p[:rem]
	}
	block := generatorBlock()
	n := 0
	for n < len(p) {
		n += copy(p[n:], block[(g.off+int64(n))%int64(len(block)):])
	}
	g.off += int64(n)
	return n, nil
}

// digestOf returns the digest of the size bytes of a generator.
func digestOf(t *testing.T, size int64) digest.Digest {
	t.Helper()
	d := digest.SHA256.Digester()
	if _, err := io.Copy(d.Hash(), &generator{size: size}); err != nil {
		t.Fatal(err)
	}
	return d.Digest()
}

// midpoint passes writes on to w and calls fn once, when at bytes have passed.
type midpoint struct {
	w     io.Writer
	n, at int64
	fn    func()
}

func (m *midpoint) Write(p []byte) (int, error) {
	k, err := m.w.Write(p)
	if m.n < m.at && m.n+int64(k) >= m.at {
		m.fn()
	}
	m.n += int64(k)
	return k, err
}

// heapAlloc returns the bytes of allocated heap objects.
func heapAlloc() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// blobSink is a registry for one blob of size bytes with digest sum: it hashes and discards
// an upload, and generates the blob on download. It samples the heap halfway through the
// upload.
type blobSink struct {
	size int64
	sum  digest.Digest

	mu          sync.Mutex
	uploaded    int64
	uploadedSum digest.Digest
	midUpload   uint64
}

func (s *blobSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/"):
		w.Header().Set("Location", r.URL.Path+"1")
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut:
		s.receive(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blobs/"+s.sum.String()):
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(s.size, 10))
		w.Header().Set("Docker-Content-Digest", s.sum.String())
		_, _ = io.Copy(w, &generator{size: s.size})
	default:
		http.NotFound(w, r)
	}
}

// receive completes an upload: it hashes the body as it streams by.
func (s *blobSink) receive(w http.ResponseWriter, r *http.Request) {
	d := digest.SHA256.Digester()
	var mid uint64
	n, err := io.Copy(&midpoint{w: d.Hash(), at: s.size / 2, fn: func() { mid = heapAlloc() }}, r.Body)
	s.mu.Lock()
	s.uploaded, s.uploadedSum, s.midUpload = n, d.Digest(), mid
	s.mu.Unlock()
	if err != nil || d.Digest().String() != r.URL.Query().Get("digest") {
		writeErr(w, http.StatusBadRequest, "DIGEST_INVALID", "digest mismatch")
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// TestStream1GiB pushes and fetches a 1 GiB blob through the provider against a registry that
// discards uploads and generates downloads, and checks that the heap does not grow with the
// blob: the provider streams, it never holds the blob in memory.
func TestStream1GiB(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 1 GiB twice; skipped with -short")
	}
	const size = int64(1) << 30
	ctx := context.Background()
	sink := &blobSink{size: size, sum: digestOf(t, size)}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	p, err := New(config.Registry{Name: "stream", Type: Type, URL: srv.URL, Prefix: "art/"}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := p.Repository(ctx, "raw/big/blob")
	if err != nil {
		t.Fatal(err)
	}
	desc := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: sink.sum, Size: size}

	runtime.GC()
	before := heapAlloc()
	if err := repo.Push(ctx, desc, &generator{size: size}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	afterPush := heapAlloc()
	rc, err := repo.Fetch(ctx, desc)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got := digest.SHA256.Digester()
	var midFetch uint64
	n, err := io.Copy(&midpoint{w: got.Hash(), at: size / 2, fn: func() { midFetch = heapAlloc() }}, rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read the fetched blob: %v", err)
	}
	afterFetch := heapAlloc()

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.uploaded != size || sink.uploadedSum != sink.sum {
		t.Errorf("the registry received %d bytes with digest %s, want %d and %s", sink.uploaded, sink.uploadedSum, size, sink.sum)
	}
	if n != size || got.Digest() != sink.sum {
		t.Errorf("fetched %d bytes with digest %s, want %d and %s", n, got.Digest(), size, sink.sum)
	}
	t.Logf("HeapAlloc before %d KiB; halfway through the push %d KiB; after the push %d KiB; halfway through the fetch %d KiB; after the fetch %d KiB",
		before>>10, sink.midUpload>>10, afterPush>>10, midFetch>>10, afterFetch>>10)
	checkHeap(t, before, map[string]uint64{
		"halfway through the push":  sink.midUpload,
		"after the push":            afterPush,
		"halfway through the fetch": midFetch,
		"after the fetch":           afterFetch,
	})
}

// checkHeap fails t when a sample of HeapAlloc exceeds before by 64 MiB or more.
func checkHeap(t *testing.T, before uint64, samples map[string]uint64) {
	t.Helper()
	const limit = 64 << 20
	for when, heap := range samples {
		if heap == 0 {
			t.Errorf("no HeapAlloc sample %s", when)
		}
		if heap > before && heap-before >= limit {
			t.Errorf("HeapAlloc %s grew by %d MiB, want less than %d MiB", when, (heap-before)>>20, limit>>20)
		}
	}
}

package formattest

import (
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The helpers of the tests that a frontend's upload streams into the registry (ADR 0014): no
// temporary file, and memory that does not grow with the file.

// HeapLimit is the most heap in use that an upload of 1 GiB may reach in those tests: the
// provider's chunk, the inspector's bounds (a control archive, a POM), and the test's own.
const HeapLimit = 64 << 20

// NoTempDir points TMPDIR at a directory nothing can be created in, for the rest of the test,
// and returns it; it skips the test where that does not hold (a root user writes anywhere).
func NoTempDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // a directory without write: the point of the test
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // t.TempDir's mode, so that its cleanup can remove it
	t.Setenv("TMPDIR", dir)
	if f, err := os.CreateTemp("", "probe-*"); err == nil {
		_ = f.Close()
		t.Skip("the read-only TMPDIR takes files here")
	}
	return dir
}

// Pattern is an endless stream of bytes that do not repeat soon: the body of a large upload.
type Pattern struct{ off int }

var patternBlock = func() []byte {
	b := make([]byte, 64<<10+7)
	x := uint64(24)
	for i := range b {
		x = x*6364136223846793005 + 1442695040888963407
		b[i] = byte(x >> 56)
	}
	return b
}()

func (p *Pattern) Read(b []byte) (int, error) {
	n := 0
	for n < len(b) {
		k := copy(b[n:], patternBlock[p.off:])
		p.off = (p.off + k) % len(patternBlock)
		n += k
	}
	return n, nil
}

// SampleHeap samples HeapInuse every 10 ms from now on; the function it returns stops and
// gives the peak.
func SampleHeap(t testing.TB) func() uint64 {
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

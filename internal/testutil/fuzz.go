package testutil

import (
	"os"
	"path/filepath"
	"runtime/metrics"
	"strconv"
	"testing"
)

// FuzzCorpusEnv is the environment variable that makes the TestFuzzCorpus test of a package
// write the seed corpus of its fuzz targets:
//
//	LAJSTROM_FUZZ_CORPUS=write go test -run '^TestFuzzCorpus$' ./...
const FuzzCorpusEnv = "LAJSTROM_FUZZ_CORPUS"

// WriteFuzzCorpus writes seeds, a file name to the value of the one argument of the fuzz target
// target, to testdata/fuzz/<target>/ of the package under test, in the format go test reads:
// "go test fuzz v1" and the value as a Go literal, []byte("…") or string("…"). go test runs
// every file there as a seed of the target, as it runs a crasher that go test -fuzz leaves
// there. A file of the same name is overwritten, other files are left alone. Unless
// FuzzCorpusEnv is "write", it skips t and writes nothing.
func WriteFuzzCorpus(t testing.TB, target string, seeds map[string]any) {
	t.Helper()
	if os.Getenv(FuzzCorpusEnv) != "write" {
		t.Skip("set " + FuzzCorpusEnv + "=write to write testdata/fuzz/" + target)
	}
	dir := filepath.Join("testdata", "fuzz", target)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, v := range seeds {
		var lit string
		switch v := v.(type) {
		case []byte:
			lit = "[]byte(" + strconv.Quote(string(v)) + ")"
		case string:
			lit = "string(" + strconv.Quote(v) + ")"
		default:
			t.Fatalf("fuzz corpus %s/%s: a %T, not []byte or string", target, name, v)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("go test fuzz v1\n"+lit+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// AllocSlack is the constant part of a bound on what a parser may allocate, the other being a
// multiple of the size of its input: what Allocated counts besides what fn allocated, since the
// runtime counts small objects a span at a time, up to a span per size class.
const AllocSlack = 1 << 20

// CheckAllocated fails t when fn, named by what, allocates more than limit bytes on the heap, so
// that a fuzz target can check that a small input does not make a parser allocate without
// bound. A single count of Allocated can be hundreds of KiB over what fn allocated (a span
// counted at once, a GC in between), so a count over limit is taken again, twice at most, and t
// fails only when the three are over: fn must allocate the same each time it runs.
func CheckAllocated(t testing.TB, what string, limit int, fn func()) {
	t.Helper()
	n := 0
	for range 3 {
		if n = Allocated(fn); n <= limit {
			return
		}
	}
	t.Fatalf("%s allocated %d bytes, at most %d", what, n, limit)
}

// Allocated returns about how many bytes the process allocated on the heap while fn ran; see
// CheckAllocated.
func Allocated(fn func()) int {
	s := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
	metrics.Read(s)
	before := s[0].Value.Uint64()
	fn()
	metrics.Read(s)
	return int(s[0].Value.Uint64() - before) //nolint:gosec // the difference of two readings of a counter, far below 2^63
}

package provider

import (
	"math"
	"slices"
	"testing"
)

// TestNewChunkedWriterBounds: a chunk size below 1 is 1, one above maxChunk is maxChunk.
func TestNewChunkedWriterBounds(t *testing.T) {
	for _, tt := range []struct{ size, want int }{
		{math.MinInt, 1}, {0, 1}, {1, 1}, {8 << 20, 8 << 20}, {maxChunk, maxChunk}, {maxChunk + 1, maxChunk}, {math.MaxInt, maxChunk},
	} {
		if got := NewChunkedWriter(t.Context(), nil, tt.size).(*chunkedWriter).chunk; got != tt.want {
			t.Errorf("NewChunkedWriter(%d) has chunks of %d, want %d", tt.size, got, tt.want)
		}
	}
}

// TestChunkedWriterGrows: the buffer starts at minChunk and doubles until a write fits, at most
// to the chunk, also for the largest chunk and for more than the chunk has room for.
func TestChunkedWriterGrows(t *testing.T) {
	const m = minChunk
	for _, tt := range []struct {
		chunk       int
		writes, cap []int
	}{
		{1 << 20, []int{10, m, 3 * m, 1<<20 - 4*m - 10}, []int{m, 2 * m, 8 * m, 1 << 20}},
		{math.MaxInt, []int{10, m, 3 * m, 600 << 10, 1}, []int{m, 2 * m, 8 * m, 16 * m, 16 * m}},
		{1 << 20, []int{math.MaxInt}, []int{1 << 20}},
		{100, []int{1, 99}, []int{100, 100}},
	} {
		w := NewChunkedWriter(t.Context(), nil, tt.chunk).(*chunkedWriter)
		var caps []int
		for _, k := range tt.writes {
			w.grow(k)
			w.buf = w.buf[:min(len(w.buf)+k, cap(w.buf))]
			caps = append(caps, cap(w.buf))
		}
		if !slices.Equal(caps, tt.cap) || slices.Max(caps) > w.chunk {
			t.Errorf("chunk %d, writes %v: capacities %v, want %v", tt.chunk, tt.writes, caps, tt.cap)
		}
	}
}

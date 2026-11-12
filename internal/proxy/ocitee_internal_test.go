package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// readAsync reads r to its end in a goroutine.
func readAsync(r io.Reader) <-chan readResult {
	ch := make(chan readResult, 1)
	go func() {
		b, err := io.ReadAll(r)
		ch <- readResult{b, err}
	}()
	return ch
}

type readResult struct {
	b   []byte
	err error
}

// TestTeeLastByte: the reader gets the bytes as they come but the last, which waits for the
// commit; a failed copy is the reader's error, without the last byte (§11.8, rule 2).
func TestTeeLastByte(t *testing.T) {
	data := []byte("0123456789")
	for _, tc := range []struct {
		name string
		end  error
	}{{"committed", nil}, {"refused", errors.New("the registry refused the digest")}} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTee(int64(len(data)), 1<<20)
			r := te.reader(t.Context())
			_, _ = te.Write(data)
			got := make([]byte, len(data))
			n, err := io.ReadAtLeast(r, got, len(data)-1)
			if err != nil || n != len(data)-1 {
				t.Fatalf("read %d bytes, %v; want all but the last", n, err)
			}
			res := readAsync(r)
			select {
			case <-res:
				t.Fatal("the last byte came before the copy ended")
			case <-time.After(20 * time.Millisecond):
			}
			te.finish(tc.end)
			out := <-res
			switch {
			case tc.end == nil && (out.err != nil || !bytes.Equal(append(got[:n], out.b...), data)):
				t.Errorf("after the commit: %q, %v", out.b, out.err)
			case tc.end != nil && (out.err == nil || len(out.b) != 0):
				t.Errorf("after a refused copy: %q, %v; want an error and no last byte", out.b, out.err)
			}
		})
	}
}

// TestTeeNeverWaits: the copy never waits for the reader: a reader past the buffer, or one that
// left, is dropped, and the writes go on (§11.8, rule 3).
func TestTeeNeverWaits(t *testing.T) {
	te := newTee(100, 10)
	r := te.reader(t.Context())
	for range 10 { // 100 bytes into a buffer of 10, with no reader
		if n, err := te.Write(make([]byte, 10)); n != 10 || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, errTeeDropped) {
		t.Fatalf("a reader past the buffer: %v, want errTeeDropped", err)
	}

	te = newTee(100, 1<<20)
	ctx, cancel := context.WithCancel(t.Context())
	r = te.reader(ctx)
	res := readAsync(r)
	cancel()
	if out := <-res; !errors.Is(out.err, errTeeDropped) {
		t.Fatalf("a reader whose context ended: %v", out.err)
	}
	if n, err := te.Write(make([]byte, 50)); n != 50 || err != nil {
		t.Fatalf("Write after the reader left = %d, %v", n, err)
	}
	_ = r.Close()
	te.finish(nil)
}

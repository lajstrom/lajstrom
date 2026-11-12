package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// defaultTeeBuffer is Options.TeeBuffer when it is zero: a few MiB, the most a claim holder may
// fall behind the copy before it is dropped (docs/spec/proxy-and-group.md §11.8, rule 3).
const defaultTeeBuffer = 4 << 20

// errTeeDropped is the error of a claim holder that fell behind the copy, or left: its
// connection is aborted, and the copy goes on.
var errTeeDropped = errors.New("proxy: the client fell behind the copy of the blob into the registry, or left")

// tee is the verified tee of a blob (§11.8): the fetch writes the upstream's bytes into it as they
// go into the backend, and the request that holds the blob's claim reads them. The fetch never
// waits: a client more than limit bytes behind, or one that closed its reader, is dropped and
// what comes after is discarded. The last byte of the blob is held back until the fetch finished
// with the backend's commit of bytes that have the digest (finish with nil); any other end is the
// reader's error, so that no client completes a blob the backend does not hold. Nothing but the
// buffer is kept.
type tee struct {
	size, limit int64
	started     chan struct{}
	start       sync.Once

	mu      sync.Mutex
	cond    *sync.Cond
	buf     []byte
	read    int64 // bytes the reader got
	done    bool
	err     error // the fetch's end: nil once the backend committed the blob
	dropped bool
}

// newTee returns the tee of a blob of size bytes whose reader may fall limit bytes behind.
func newTee(size, limit int64) *tee {
	t := &tee{size: size, limit: limit, started: make(chan struct{})}
	t.cond = sync.NewCond(&t.mu)
	return t
}

// begin says that the fetch holds the claim and streams: the request may read from now on.
func (t *tee) begin() { t.start.Do(func() { close(t.started) }) }

// Write takes the upstream's bytes as they go into the backend; it never blocks and never fails.
func (t *tee) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dropped {
		return len(p), nil
	}
	if int64(len(t.buf)+len(p)) > t.limit {
		t.dropped, t.buf = true, nil
	} else {
		t.buf = append(t.buf, p...)
	}
	t.cond.Broadcast()
	return len(p), nil
}

// finish ends the fetch: nil once the backend committed bytes that have the blob's digest, the
// failure otherwise.
func (t *tee) finish(err error) {
	t.begin()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.done {
		t.done, t.err = true, err
	}
	t.cond.Broadcast()
}

// endedBefore reports whether the fetch ended without streaming: begin came from finish, and
// the request is served from the backend once the flight is done.
func (t *tee) endedBefore() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done && t.err != nil && len(t.buf) == 0 && t.read == 0
}

// drop drops the reader: it left, or its context ended.
func (t *tee) drop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropped, t.buf = true, nil
	t.cond.Broadcast()
}

// reader returns the reader of the claim holder, dropped when ctx ends.
func (t *tee) reader(ctx context.Context) io.ReadCloser {
	stop := context.AfterFunc(ctx, t.drop)
	return &teeReader{t: t, stop: stop}
}

type teeReader struct {
	t    *tee
	stop func() bool
}

// Read returns the bytes as they come, the last one only once the backend committed the blob.
func (r *teeReader) Read(p []byte) (int, error) {
	t := r.t
	t.mu.Lock()
	defer t.mu.Unlock()
	for {
		committed := t.done && t.err == nil
		ready := len(t.buf)
		if !committed && t.read+int64(ready) >= t.size {
			ready = int(max(t.size-1-t.read, 0)) // the last byte waits for the commit (rule 2)
		}
		switch {
		case t.dropped:
			return 0, errTeeDropped
		case ready > 0:
			n := copy(p, t.buf[:ready])
			t.buf, t.read = t.buf[n:], t.read+int64(n)
			return n, nil
		case t.done && t.err != nil:
			return 0, fmt.Errorf("proxy: the copy of the blob into the registry failed: %w", t.err)
		case committed && t.read == t.size:
			return 0, io.EOF
		case committed:
			return 0, fmt.Errorf("proxy: the blob was committed with %d of its %d bytes read", t.read, t.size)
		}
		t.cond.Wait()
	}
}

// Close drops the reader; the fetch goes on.
func (r *teeReader) Close() error {
	r.stop()
	r.t.drop()
	return nil
}

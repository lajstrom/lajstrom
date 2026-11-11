package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

// counter is a read that sees the write at its nth call; n < 0 never.
type counter struct{ reads, n int }

func (c *counter) read(context.Context) (bool, error) {
	c.reads++
	return c.reads == c.n, nil
}

// TestReadBack: Await reads until the read sees the write, with the backoff's waits in
// between, and gives up just past the timeout.
func TestReadBack(t *testing.T) {
	ctx := context.Background()
	c := &counter{n: 3}
	start := time.Now()
	ok, err := ReadBack{Timeout: 10 * time.Second}.Await(ctx, c.read)
	if !ok || err != nil || c.reads != 3 {
		t.Errorf("seen at the third read: %v, %v after %d reads", ok, err, c.reads)
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 2*time.Second {
		t.Errorf("two waits took %s, want 300 ms to 600 ms: 100 to 200 ms, then 200 to 400 ms", d)
	}

	c = &counter{n: -1}
	start = time.Now()
	ok, err = ReadBack{Timeout: 250 * time.Millisecond}.Await(ctx, c.read)
	if ok || err != nil || c.reads < 2 {
		t.Errorf("never seen: %v, %v after %d reads, want false, nil after two at least", ok, err, c.reads)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > time.Second {
		t.Errorf("gave up after %s, want just past the timeout of 250 ms", d)
	}
}

// TestReadBackEnds: without a timeout Await reads once; an error of the read and a cancelled
// context end it.
func TestReadBackEnds(t *testing.T) {
	ctx := context.Background()
	c := &counter{n: -1}
	if ok, err := (ReadBack{}).Await(ctx, c.read); ok || err != nil || c.reads != 1 {
		t.Errorf("without a timeout: %v, %v after %d reads, want one read", ok, err, c.reads)
	}

	boom := errors.New("boom")
	reads := 0
	_, err := ReadBack{Timeout: time.Minute}.Await(ctx, func(context.Context) (bool, error) { reads++; return false, boom })
	if !errors.Is(err, boom) || reads != 1 {
		t.Errorf("an error of the read: %v after %d reads, want it after one", err, reads)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := (ReadBack{Timeout: time.Minute}).Await(cancelled, (&counter{n: -1}).read); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context: %v, want context.Canceled", err)
	}
}

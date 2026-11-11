package provider

import (
	"context"
	"math/rand/v2"
	"time"
)

// The schedule of a read-back: the first wait is 100 to 200 ms, each next one twice as long,
// at most readBackStep.
const (
	readBackFirst = 100 * time.Millisecond
	readBackStep  = 5 * time.Second
)

// ReadBack reads a backend again that does not serve yet what it acknowledged, as an
// eventually consistent backend does for a while (Capabilities.Consistency, docs/registries/README.md,
// Consistency): ingest's visibility barrier after a push, and the read of a blob a client says
// it uploaded.
type ReadBack struct {
	// Timeout bounds the read-back, from its first read; zero or negative means one read.
	Timeout time.Duration
}

// Await calls seen until it reports true, and returns true then. Between two calls it waits,
// at first 100 to 200 ms, then twice as long each time with jitter, at most 5 s a wait; it
// returns false when the next wait would end past Timeout. An error of seen ends it with that
// error, and so does ctx's.
func (b ReadBack) Await(ctx context.Context, seen func(context.Context) (bool, error)) (bool, error) {
	deadline := time.Now().Add(b.Timeout)
	step := readBackFirst
	for {
		ok, err := seen(ctx)
		if ok || err != nil {
			return ok, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false, nil
		}
		wait := min(step+rand.N(step), readBackStep, left) //nolint:gosec // jitter, not a secret
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return false, ctx.Err()
		case <-t.C:
		}
		step = min(2*step, readBackStep)
	}
}

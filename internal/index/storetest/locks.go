package storetest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testReferrersLock checks WithReferrersLock: the callers of one (name, subject) never overlap;
// another subject, or the same subject in another repository, does not wait for a held lock;
// a caller whose context ends while it waits gets the context's error and fn does not run;
// fn's error is returned.
func testReferrersLock(t *testing.T, s index.Store) {
	ctx := t.Context()
	// A name of the test's own: on SQLite the lock is the process's, shared by every store.
	name, subject := "raw/a/"+strings.ReplaceAll(strings.ToLower(t.Name()), "/", "-"), dg("subject")

	// Eight callers of one key, each inside for a few milliseconds: at most one at a time.
	var inside, most atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			err := s.WithReferrersLock(ctx, name, subject, func(context.Context) error {
				n := inside.Add(1)
				for m := most.Load(); n > m; m = most.Load() {
					if most.CompareAndSwap(m, n) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				inside.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("WithReferrersLock: %v", err)
			}
		})
	}
	wg.Wait()
	if most.Load() != 1 {
		t.Errorf("%d callers of one key were inside at once, want 1", most.Load())
	}

	// The key held: other keys go through, the same key waits until its context ends.
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.WithReferrersLock(ctx, name, subject, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	for _, other := range []struct{ name, subject string }{{name, "another subject"}, {name + "-other", "subject"}} {
		quick, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.WithReferrersLock(quick, other.name, dg(other.subject), func(context.Context) error { return nil })
		cancel()
		if err != nil {
			t.Errorf("%s@%s waited for %s@%s: %v", other.name, other.subject, name, subject, err)
		}
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	ran := false
	err := s.WithReferrersLock(short, name, subject, func(context.Context) error { ran = true; return nil })
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || ran {
		t.Errorf("the held key: %v, fn ran %t; want the deadline's error and no call", err, ran)
	}
	close(release)
	must(t, <-done)

	boom := errors.New("boom")
	if err := s.WithReferrersLock(ctx, name, subject, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("fn's error: %v", err)
	}
}

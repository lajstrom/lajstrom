package proxy

import (
	"context"
	"sync"
)

// flights are the upstream requests running on this replica, by what they ask: the requests
// that miss one artifact at once share one fetch (docs/spec/proxy-and-group.md §5, step 6), and
// those that miss one document share one request for it (§4.2). Across replicas the fetch
// claim does the same for an artifact; a flight lives no longer than its work.
type flights struct {
	mu sync.Mutex
	m  map[string]*flight
}

// flight is one fetch or one document request; done is closed when it ends, and then err and
// shared hold its outcome.
type flight struct {
	done   chan struct{}
	err    error
	shared bool // another replica fetched the artifact, and this one waited for it
}

// join returns the flight of key, starting run in a goroutine of its own when none runs, and
// reports whether it joined one that ran already.
func (f *flights) join(key string, run func() (shared bool, err error)) (fl *flight, joined bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fl, ok := f.m[key]; ok {
		return fl, true
	}
	if f.m == nil {
		f.m = map[string]*flight{}
	}
	fl = &flight{done: make(chan struct{})}
	f.m[key] = fl
	go func() {
		fl.shared, fl.err = run()
		f.mu.Lock()
		delete(f.m, key)
		f.mu.Unlock()
		close(fl.done)
	}()
	return fl, false
}

// detach returns the context of a flight's work: the values of ctx without its end, cancelled
// by the server's shutdown (Options.Context), so that a client that leaves does not cancel the
// work for the requests that wait on it.
func (c *Core) detach(ctx context.Context) (context.Context, context.CancelFunc) {
	fctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(c.o.Context, cancel)
	return fctx, func() {
		stop()
		cancel()
	}
}

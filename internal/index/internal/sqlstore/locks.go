package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/opencontainers/go-digest"
)

// Coordination: the referrers lock (docs/spec/index-schema.md, Coordination).

// WithReferrersLock calls fn holding the lock on the referrers of subject in the OCI repository
// name (index.Store). On Postgres it is the advisory lock of referrersLockKey in a transaction
// of its own, begun without ctx's cancellation so that it ends when fn returns and not before;
// the wait for the lock is ctx's. On SQLite (no XactLock) it is processLocks.
func (s *Store) WithReferrersLock(ctx context.Context, name string, subject digest.Digest, fn func(context.Context) error) error {
	what := name + "@" + subject.String()
	if s.d.XactLock == "" {
		unlock, err := processLocks.lock(ctx, what)
		if err != nil {
			return fmt.Errorf("index: the referrers lock of %s: %w", what, err)
		}
		defer unlock()
		return fn(ctx)
	}
	tx, err := s.db.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return fmt.Errorf("index: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // nothing was written: the end of the transaction releases the lock
	if _, err := tx.ExecContext(ctx, s.d.Rebind(s.d.XactLock), referrersLockKey(name, subject)); err != nil {
		return fmt.Errorf("index: the referrers lock of %s: %w", what, err)
	}
	return fn(ctx)
}

// referrersLockKey is the key of the Postgres advisory lock on the referrers of subject in name:
// the first eight bytes of a sha256 of both. A collision, with another subject or with the
// migration and reindex locks, only makes one writer wait for another.
func referrersLockKey(name string, subject digest.Digest) int64 {
	sum := sha256.Sum256([]byte("referrers\x00" + name + "\x00" + subject.String()))
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // a hash, any 64 bits do
}

// keyLocks are locks of the process by key, which a waiter may give up on when its context
// ends. A key's entry lives while someone holds or awaits it.
type keyLocks struct {
	mu sync.Mutex
	m  map[string]*keyLock
}

// keyLock is one key's lock: holding it is having sent into held.
type keyLock struct {
	held  chan struct{} // capacity 1
	users int           // holders and waiters
}

// processLocks are the referrers locks on SQLite.
var processLocks = &keyLocks{m: map[string]*keyLock{}}

// lock waits for key's lock, or for ctx to end, and returns the function that releases it.
func (l *keyLocks) lock(ctx context.Context, key string) (func(), error) {
	l.mu.Lock()
	k := l.m[key]
	if k == nil {
		k = &keyLock{held: make(chan struct{}, 1)}
		l.m[key] = k
	}
	k.users++
	l.mu.Unlock()
	leave := func() {
		l.mu.Lock()
		if k.users--; k.users == 0 {
			delete(l.m, key)
		}
		l.mu.Unlock()
	}
	select {
	case k.held <- struct{}{}:
		return func() { <-k.held; leave() }, nil
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
}

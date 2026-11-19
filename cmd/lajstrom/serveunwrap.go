package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/keys"
)

// The data keys of keys.masterKeyKMS at the start of serve (docs/spec/index-schema.md, A master
// key in a KMS, #1388): serve listens without waiting for the KMS. Each unwrap is one request at
// its start; one that did not open is asked for again in the background until it opens, and the
// beat that follows writes the ids of the replica again and reads the sealing key.

const (
	// unwrapAtStartTimeout bounds the unwrap of a data key at the start of serve, the client's
	// two retries included.
	unwrapAtStartTimeout = 10 * time.Second
	// unwrapEvery is the wait between two unwraps of a data key once unwrapBackoff is spent.
	unwrapEvery = 30 * time.Second
)

// unwrapBackoff are the waits before the first unwraps of a data key that did not open at the
// start: 1, 2, 4, 8 and 16 seconds, then unwrapEvery. A variable, which the tests shorten.
var unwrapBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}

// unwrapWait is the wait before the unwrap n (0 the first after the start) of a data key.
func unwrapWait(n int) time.Duration {
	if n < len(unwrapBackoff) {
		return unwrapBackoff[n]
	}
	return unwrapEvery
}

// unwrapAtStart has the KMS unwrap each data key of ds into m, all at once, each one call bounded
// by unwrapAtStartTimeout, and returns the error of each, nil for one that opened.
func unwrapAtStart(ctx context.Context, m *keys.Master, ds []dataKey) []error {
	errs := make([]error, len(ds))
	var wg sync.WaitGroup
	for i, d := range ds {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, unwrapAtStartTimeout)
			defer cancel()
			errs[i] = d.unwrap(ctx, m)
		})
	}
	wg.Wait()
	return errs
}

// unwrappedAtStart logs the unwraps of ds at the start, errs their errors, once the sealing key
// is read, and returns the keys that did not open and may later: a KMS that refused or did not
// answer. A key whose answer had another id is not asked for again until a start.
func unwrappedAtStart(ctx context.Context, log *slog.Logger, m *keys.Master, ds []dataKey, errs []error) []dataKey {
	var left []dataKey
	for i, d := range ds {
		switch {
		case errs[i] == nil:
			log.InfoContext(ctx, "master key unwrapped by its KMS key", "id", d.row.KeyID, "kms", d.key.String())
		case unwrapFailed(ctx, log, m, d, errs[i]):
			left = append(left, d)
		}
	}
	return left
}

// unwrapLater asks the KMS again for each data key of ds, the first after unwrapWait(0), then
// after each wait that follows, until it opens or ctx ends: a key enabled again or a permission
// granted ends it without a restart. Once open, the key joins the held keys, logged at info, and
// is never asked for again; the next beat writes the ids of the replica and reads the sealing key.
func unwrapLater(ctx context.Context, log *slog.Logger, m *keys.Master, ds []dataKey) {
	var wg sync.WaitGroup
	for _, d := range ds {
		wg.Go(func() {
			for n := 0; ; n++ {
				t := time.NewTimer(unwrapWait(n))
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
				}
				err := d.unwrap(ctx, m)
				if err == nil {
					log.InfoContext(ctx, "master key unwrapped by its KMS key: it is held from now on, and the next beat reads the "+
						"sealing key", "id", d.row.KeyID, "kms", d.key.String(), "attempts", n+2)
					return
				}
				if ctx.Err() != nil || !unwrapFailed(ctx, log, m, d, err) {
					return
				}
			}
		})
	}
	wg.Wait()
}

// unwrapFailed logs err, an unwrap of d that did not open, and reports whether d may open when
// asked again: at warn, at error while d is the key that seals or will (the sole key of a database
// that names none), and at error, never to be asked again, for an answer of another id. The line
// names the id and the KMS key, with the KMS's error, never a key or a ciphertext.
func unwrapFailed(ctx context.Context, log *slog.Logger, m *keys.Master, d dataKey, err error) bool {
	attrs := []any{"id", d.row.KeyID, "kms", d.key.String(), "error", err.Error()}
	if errors.Is(err, keys.ErrOtherDataKey) {
		log.ErrorContext(ctx, "master key not held: its KMS key unwrapped another key, and it is not asked for again until a start; "+
			"the row of wrapped_master_keys was changed", attrs...)
		return false
	}
	sealing, _ := m.Sealing()
	level := slog.LevelWarn
	if sealing == d.row.KeyID || sealing == "" && len(m.IDs())+len(m.Pending()) == 1 {
		level = slog.LevelError
	}
	log.Log(ctx, level, "master key not unwrapped yet: asked again in the background", attrs...)
	return true
}

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/version"
)

// The row of a serve process in replicas (docs/spec/index-schema.md, Coordination): written at
// the start, beaten every replicaBeatEvery, deleted at the stop. A row whose heartbeat is older
// than replicaStaleAfter is of a replica that is gone, as for reindex runs and fetch claims.
const (
	replicaBeatEvery  = 30 * time.Second
	replicaStaleAfter = 2 * time.Minute
)

// The bounds of the comparison of this replica's clock with the database's (docs/operations.md,
// Clocks): past clockWarn every comparison logs a warning, past clockFail an error, and
// /-/readyz answers 503. clockFail is the skew a bearer token is checked with
// (docs/spec/guarantees.md, clocks agree).
const (
	clockWarn = 10 * time.Second
	clockFail = 60 * time.Second
)

// replica is the row of this process, its heartbeat, and the comparison of this process's clock
// with the database's that runs with it.
type replica struct {
	st    index.Store
	log   *slog.Logger
	now   func() time.Time
	every time.Duration
	row   index.Replica

	mu   sync.Mutex
	lost error         // set while another replica holds the database (SQLite)
	skew time.Duration // this process's clock minus the database's, at the last comparison
}

// startReplica writes the row of this process and compares its clock with the database's. On
// SQLite, which serves one replica, a row of another replica with a fresh heartbeat is an error
// that names it.
func startReplica(ctx context.Context, st index.Store, log *slog.Logger, now func() time.Time) (*replica, error) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	at := now()
	r := &replica{st: st, log: log, now: now, every: replicaBeatEvery, row: index.Replica{ID: index.NewUID(at), Host: host,
		PID: os.Getpid(), Version: version.Get().Version, StartedAt: at}}
	if err := st.StartReplica(ctx, &r.row, at.Add(-replicaStaleAfter)); err != nil {
		return nil, held(err)
	}
	log.Info("replica registered in the index", "replica", r.row.ID, "host", host, "pid", r.row.PID)
	r.clock(ctx)
	return r, nil
}

// held returns the error of a start that another replica's row refused, saying who holds the
// database and when its row goes stale; any other error as it is.
func held(err error) error {
	var c *index.ReplicaConflictError
	if !errors.As(err, &c) {
		return err
	}
	l := c.Live
	return fmt.Errorf("another replica serves on this SQLite database: %s on host %s, process %d, %s, started %s, "+
		"last heartbeat %s; SQLite serves one replica, more need database.driver postgres. If that process is gone, "+
		"start again after %s, when its heartbeat is stale", l.ID, l.Host, l.PID, l.Version, l.StartedAt.Format(time.RFC3339),
		l.HeartbeatAt.Format(time.RFC3339), l.HeartbeatAt.Add(replicaStaleAfter).Format(time.RFC3339))
}

// Run beats every r.every until ctx ends.
func (r *replica) Run(ctx context.Context) {
	t := time.NewTicker(r.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.beat(ctx)
		}
	}
}

// beat sets the heartbeat of the row, then compares the clocks.
func (r *replica) beat(ctx context.Context) {
	r.heartbeat(ctx)
	r.clock(ctx)
}

// heartbeat sets the heartbeat of the row. A row that is gone, deleted by a start elsewhere that
// found it stale (this process stood still for replicaStaleAfter), is written again; on SQLite
// that is refused while the other replica's row is fresh, and Err says so until it is not.
func (r *replica) heartbeat(ctx context.Context) {
	at := r.now()
	err := r.st.HeartbeatReplica(ctx, r.row.ID, at)
	if errors.Is(err, index.ErrNotFound) {
		r.log.WarnContext(ctx, "the row of this replica was deleted as stale; writing it again", "replica", r.row.ID)
		r.row.HeartbeatAt = at
		err = r.st.StartReplica(ctx, &r.row, at.Add(-replicaStaleAfter))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var c *index.ReplicaConflictError
	switch {
	case err == nil:
		if r.lost != nil {
			r.log.InfoContext(ctx, "the row of this replica is written again", "replica", r.row.ID)
		}
		r.lost = nil
	case ctx.Err() != nil:
	case errors.As(err, &c):
		r.lost = errors.New("another replica serves on this SQLite database")
		r.log.ErrorContext(ctx, "not ready", "error", held(err).Error())
	default:
		r.log.WarnContext(ctx, "the heartbeat of this replica not written", "replica", r.row.ID, "error", err.Error())
	}
}

// clockReader is the part of index.Store that skewOf reads.
type clockReader interface {
	DatabaseTime(ctx context.Context) (time.Time, error)
}

// skewOf compares the clock now with the database's: the skew is the time of now at the middle
// of the query minus the database's, exact to half the query's round trip. It returns the
// database's time too.
func skewOf(ctx context.Context, st clockReader, now func() time.Time) (skew time.Duration, db time.Time, err error) {
	before := now()
	db, err = st.DatabaseTime(ctx)
	after := now()
	if err != nil {
		return 0, time.Time{}, err
	}
	return before.Add(after.Sub(before) / 2).Sub(db), db, nil
}

// offBy says how far a clock skew off the database's is, and on which side: "1m15s ahead of".
func offBy(skew time.Duration) string {
	side := "ahead of"
	if skew < 0 {
		side = "behind"
	}
	return skew.Abs().Round(time.Second).String() + " " + side
}

// clock compares this process's clock with the database's (skewOf). It sets
// lajstrom_clock_skew_seconds, logs past clockWarn and keeps the skew for Err. A database that
// does not answer leaves the last comparison as it was: readiness reports the database itself.
func (r *replica) clock(ctx context.Context) {
	skew, db, err := skewOf(ctx, r.st, r.now)
	if err != nil {
		if ctx.Err() == nil {
			r.log.WarnContext(ctx, "the database's clock not read", "error", err.Error())
		}
		return
	}
	metrics.ClockSkew.Set(skew.Seconds())
	r.mu.Lock()
	r.skew = skew
	r.mu.Unlock()
	attrs := []any{"skew", skew.Round(time.Millisecond).String(), "database", db.UTC().Format(time.RFC3339Nano)}
	switch abs := skew.Abs(); {
	case abs > clockFail:
		r.log.ErrorContext(ctx, "not ready: the clock of this replica is more than 60s off the database's; run NTP", attrs...)
	case abs > clockWarn:
		r.log.WarnContext(ctx, "the clock of this replica is more than 10s off the database's; run NTP", attrs...)
	}
}

// Err is the error of readiness while another replica serves on the database, or while the last
// comparison of the clocks found them more than clockFail apart; nil otherwise.
func (r *replica) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lost != nil {
		return r.lost
	}
	if r.skew.Abs() <= clockFail {
		return nil
	}
	return errors.New("the clock of this replica is " + offBy(r.skew) + " the database's")
}

// Stop deletes the row of this process, so that a replica started after it need not wait for
// the row to go stale.
func (r *replica) Stop(ctx context.Context) {
	if err := r.st.StopReplica(ctx, r.row.ID); err != nil && !errors.Is(err, index.ErrNotFound) {
		r.log.WarnContext(ctx, "the row of this replica not deleted; it goes stale", "replica", r.row.ID, "error", err.Error())
	}
}

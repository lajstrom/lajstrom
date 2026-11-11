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
	"github.com/lajstrom/lajstrom/internal/version"
)

// The row of a serve process in replicas (docs/spec/index-schema.md, Coordination): written at
// the start, beaten every replicaBeatEvery, deleted at the stop. A row whose heartbeat is older
// than replicaStaleAfter is of a replica that is gone, as for reindex runs and fetch claims.
const (
	replicaBeatEvery  = 30 * time.Second
	replicaStaleAfter = 2 * time.Minute
)

// replica is the row of this process and its heartbeat.
type replica struct {
	st    index.Store
	log   *slog.Logger
	now   func() time.Time
	every time.Duration
	row   index.Replica

	mu   sync.Mutex
	lost error // set while another replica holds the database (SQLite)
}

// startReplica writes the row of this process. On SQLite, which serves one replica, a row of
// another replica with a fresh heartbeat is an error that names it.
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

// beat sets the heartbeat of the row. A row that is gone, deleted by a start elsewhere that found
// it stale (this process stood still for replicaStaleAfter), is written again; on SQLite that is
// refused while the other replica's row is fresh, and Err says so until it is not.
func (r *replica) beat(ctx context.Context) {
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

// Err is the error of readiness while another replica serves on the database, nil otherwise.
func (r *replica) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lost
}

// Stop deletes the row of this process, so that a replica started after it need not wait for
// the row to go stale.
func (r *replica) Stop(ctx context.Context) {
	if err := r.st.StopReplica(ctx, r.row.ID); err != nil && !errors.Is(err, index.ErrNotFound) {
		r.log.WarnContext(ctx, "the row of this replica not deleted; it goes stale", "replica", r.row.ID, "error", err.Error())
	}
}

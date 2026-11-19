package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
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
	lock  *os.File // the lock file of the SQLite file while this process holds its lock (Flock), nil otherwise
	// master is the master keys this process holds, nil for none (#1288).
	master *keys.Master

	mu   sync.Mutex
	lost error         // set while another replica holds the database (SQLite)
	skew time.Duration // this process's clock minus the database's, at the last comparison
}

// startReplica writes the row of this process, with the ids of the master keys m it holds (nil:
// none), and compares its clock with the database's. On SQLite, which serves one replica, it first
// takes the lock of the lock file next to the SQLite file, file (lockFile; file is "" for postgres
// and an in-memory database), and a row of another replica with a fresh heartbeat is an error
// that names it, unless its process is known to be gone (start).
func startReplica(ctx context.Context, st index.Store, log *slog.Logger, now func() time.Time, file string, m *keys.Master) (*replica, error) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	at := now()
	r := &replica{st: st, log: log, now: now, every: replicaBeatEvery, row: index.Replica{ID: index.NewUID(at), Host: host,
		PID: os.Getpid(), Process: thisProcess(), Version: version.Get().Version, StartedAt: at}, master: m}
	if m != nil {
		r.row.MasterKeys = m.IDs()
	}
	if file != "" {
		r.takeLock(ctx, file+lockSuffix)
	}
	if err := r.start(ctx, at); err != nil {
		r.unlock()
		return nil, held(err)
	}
	log.Info("replica registered in the index", "replica", r.row.ID, "host", host, "pid", r.row.PID)
	r.clock(ctx)
	return r, nil
}

// takeLock takes the lock of the lock file name for this process, and its row says so: Flock is
// the file's identity. A lock that another process holds, or that cannot be taken, is logged,
// and the row says none: a start after this process crashed knows it gone by its PID only, or
// once its row is stale.
func (r *replica) takeLock(ctx context.Context, name string) {
	f, id, err := lockFile(name)
	switch {
	case errors.Is(err, errors.ErrUnsupported):
	case err != nil:
		r.log.WarnContext(ctx, "the lock file of the SQLite database not locked", "file", name, "error", err.Error())
	case f == nil:
		r.log.WarnContext(ctx, "another process of this host holds the lock file of the SQLite database", "file", name)
	}
	r.lock, r.row.Flock = f, id
}

// unlock closes the lock file, which drops its lock.
func (r *replica) unlock() {
	if r.lock != nil {
		_ = r.lock.Close()
		r.lock = nil
	}
}

// start writes the row of this process, deleting the rows whose heartbeat is stale at at
// (Store.StartReplica). On SQLite a fresh row of another replica refuses it; when that row's
// process is known to be gone (processGone: a process of this host that crashed or was killed;
// lockGone: a process of this boot that held the lock this process holds now), start deletes
// the row and tries again, up to 8 times.
func (r *replica) start(ctx context.Context, at time.Time) error {
	var err error
	for range 8 {
		err = r.st.StartReplica(ctx, &r.row, at.Add(-replicaStaleAfter))
		var c *index.ReplicaConflictError
		if !errors.As(err, &c) || !processGone(c.Live, r.row.Host, r.row.Process) && !lockGone(c.Live, r.row) {
			return err
		}
		r.log.WarnContext(ctx, "deleting the row of a replica whose process is gone", "replica", c.Live.ID,
			"host", c.Live.Host, "pid", c.Live.PID, "heartbeat", c.Live.HeartbeatAt.Format(time.RFC3339))
		if err := r.st.StopReplica(ctx, c.Live.ID); err != nil && !errors.Is(err, index.ErrNotFound) {
			return err
		}
	}
	return err
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

// beat sets the heartbeat of the row, then compares the clocks, then writes the ids of the keys it
// holds again when an unwrap changed them, then reads the sealing key again.
func (r *replica) beat(ctx context.Context) {
	r.heartbeat(ctx)
	r.clock(ctx)
	r.heldKeys(ctx)
	r.sealing(ctx)
}

// heldKeys writes the ids of the master keys this replica holds in its row when they are not
// those the row holds: a data key of keys.masterKeyKMS that the KMS unwrapped since, which the
// rewrap's guard counts from then on (SetReplicaMasterKeys, #1388). A row that is gone is the
// heartbeat's to write again; a failure is logged, and the next beat tries again.
func (r *replica) heldKeys(ctx context.Context) {
	if r.master == nil {
		return
	}
	ids := r.master.IDs()
	if slices.Equal(ids, r.row.MasterKeys) || len(ids) == 0 && len(r.row.MasterKeys) == 0 {
		return
	}
	if err := r.st.SetReplicaMasterKeys(ctx, r.row.ID, ids); err != nil {
		if ctx.Err() == nil && !errors.Is(err, index.ErrNotFound) {
			r.log.WarnContext(ctx, "the master keys of this replica not written in its row", "replica", r.row.ID, "error", err.Error())
		}
		return
	}
	r.row.MasterKeys = ids
	r.log.InfoContext(ctx, "the master keys of this replica written in its row", "replica", r.row.ID, "held", ids)
}

// sealing reads the row of sealing_key again, at each beat (docs/spec/index-schema.md, The master
// key and its replacement, #1288). A new id of a key this replica holds seals from then on,
// logged at info. An id of a key it does not hold fails closed from this beat on: it seals
// nothing, the key it sealed with before included, Err says so (/-/readyz answers 503), and every
// beat logs an error that names the id, until a beat reads the id of a key it holds. It does not
// exit: its keys change only at a start. A row that cannot be read, or none, leaves the sealing
// key as it is: the database's own failure is readiness's to report.
func (r *replica) sealing(ctx context.Context) {
	if r.master == nil {
		return
	}
	row, err := r.st.GetSealingKey(ctx)
	if errors.Is(err, index.ErrNotFound) && len(r.master.Pending()) == 0 && len(r.master.IDs()) == 1 {
		row, err = r.recordSealing(ctx) // the sole key, a data key unwrapped since the start (#1388)
	}
	if err != nil {
		if !errors.Is(err, index.ErrNotFound) && ctx.Err() == nil {
			r.log.WarnContext(ctx, "the sealing key of the index not read", "error", err.Error())
		}
		return
	}
	was, held := r.master.Sealing()
	switch {
	case row.KeyID == was && held:
	case r.master.SealWith(row.KeyID) && row.KeyID == was:
		r.log.InfoContext(ctx, "the master key that seals is unwrapped: this replica seals with it, and is ready", "sealing", row.KeyID)
	case r.master.Holds(row.KeyID):
		r.log.InfoContext(ctx, "the index seals with another master key, which this replica holds", "sealing", row.KeyID,
			"before", was)
	case r.master.IsPending(row.KeyID):
		r.log.ErrorContext(ctx, "not ready: the index seals with a master key that the KMS has not unwrapped for this replica yet, "+
			"and the replica seals nothing until it does; it is asked for again in the background", "sealing", row.KeyID)
	default:
		r.log.ErrorContext(ctx, "not ready: the index seals with a master key this replica does not hold, and the replica "+
			"seals nothing until it does: add the key to its sources and start it again, or name a key it holds with "+
			"lajstrom admin key rewrap --to <id>", "sealing", row.KeyID, "held", r.master.IDs())
	}
}

// recordSealing records the sole key of this replica as the sealing key where the database names
// none, as a start does (recordSealing), and reads the row again.
func (r *replica) recordSealing(ctx context.Context) (*index.SealingKey, error) {
	if err := recordSealing(ctx, r.st, r.master, r.row.Host); err != nil {
		r.log.ErrorContext(ctx, "not ready: the master key of this replica cannot seal", "error", err.Error())
		return nil, err
	}
	return r.st.GetSealingKey(ctx)
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
		err = r.start(ctx, at)
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

// Err is the error of readiness while another replica serves on the database, while the index
// seals with a master key this replica does not hold, or has not unwrapped (#1388), or names none
// to it (each names the id, which shows nothing of the key), or while the last comparison of the
// clocks found them more than clockFail apart; nil otherwise.
func (r *replica) Err() error {
	if r.master != nil {
		switch id, held := r.master.Sealing(); {
		case held:
		case r.master.IsPending(id):
			return errors.New("this replica has not unwrapped the master key that seals: " + id)
		case id == "" && soleKeyPending(r.master):
			return errors.New("this replica has not unwrapped the master key that seals: " + r.master.Pending()[0])
		case id == "" && len(r.master.IDs()) > 0:
			return errors.New("no master key seals yet: the index names none")
		case id != "":
			return errors.New("this replica does not hold the master key that seals: " + id)
		}
	}
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
// the row to go stale, then drops the lock of the lock file.
func (r *replica) Stop(ctx context.Context) {
	if err := r.st.StopReplica(ctx, r.row.ID); err != nil && !errors.Is(err, index.ErrNotFound) {
		r.log.WarnContext(ctx, "the row of this replica not deleted; it goes stale", "replica", r.row.ID, "error", err.Error())
	}
	r.unlock()
}

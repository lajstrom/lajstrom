package sqlstore

import (
	"context"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Coordination: replicas, a row per serve process on the database
// (docs/spec/index-schema.md, Coordination).

const replicaCols = "id, host, pid, process, version, started_at, heartbeat_at"

func replicaDests(r *index.Replica) []any {
	return []any{&r.ID, &r.Host, &r.PID, &r.Process, &r.Version, timeDest{&r.StartedAt}, timeDest{&r.HeartbeatAt}}
}

// ListReplicas returns the rows of replicas ordered by StartedAt, then ID.
func (c *conn) ListReplicas(ctx context.Context) ([]index.Replica, error) {
	return getAll(ctx, c, "replicas", replicaDests, "SELECT "+replicaCols+" FROM replicas ORDER BY started_at, id")
}

// StartReplica deletes the stale rows and inserts r, in one transaction. On SQLite (no
// XactLock: every transaction holds the write lock of the file from its start, so two starts
// on one file are serialized across processes) a fresh row of another replica refuses r.
func (c *conn) StartReplica(ctx context.Context, r *index.Replica, staleBefore time.Time) error {
	stamp(&r.StartedAt, now())
	stamp(&r.HeartbeatAt, r.StartedAt)
	what := fmt.Sprintf("replicas %q", r.ID)
	return c.withTx(ctx, func(c *conn) error {
		if _, err := c.exec(ctx, "replicas", "DELETE FROM replicas WHERE heartbeat_at < ?", c.d.EncodeTime(staleBefore)); err != nil {
			return err
		}
		if c.d.XactLock == "" {
			live, err := getAll(ctx, c, "replicas", replicaDests,
				"SELECT "+replicaCols+" FROM replicas WHERE id <> ? ORDER BY heartbeat_at DESC, id", r.ID)
			if err != nil {
				return err
			}
			if len(live) > 0 {
				return &index.ReplicaConflictError{Live: live[0]}
			}
		}
		_, err := c.exec(ctx, what, "INSERT INTO replicas ("+replicaCols+") VALUES (?, ?, ?, ?, ?, ?, ?)",
			r.ID, r.Host, r.PID, r.Process, r.Version, c.d.EncodeTime(r.StartedAt), c.d.EncodeTime(r.HeartbeatAt))
		return err
	})
}

// HeartbeatReplica sets HeartbeatAt of the replica's row.
func (c *conn) HeartbeatReplica(ctx context.Context, id string, at time.Time) error {
	return c.execOne(ctx, fmt.Sprintf("replicas %q", id), "UPDATE replicas SET heartbeat_at = ? WHERE id = ?",
		c.d.EncodeTime(at), id)
}

// StopReplica deletes the replica's row.
func (c *conn) StopReplica(ctx context.Context, id string) error {
	return c.execOne(ctx, fmt.Sprintf("replicas %q", id), "DELETE FROM replicas WHERE id = ?", id)
}

// DatabaseTime returns the time of the database's clock (Dialect.Clock).
func (c *conn) DatabaseTime(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := c.scanRow(ctx, "the database's clock", c.d.Clock, nil, timeDest{&t})
	return t, err
}

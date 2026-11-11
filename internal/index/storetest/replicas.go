package storetest

import (
	"errors"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testReplicas checks the rows of replicas: a start fills the times and a heartbeat moves
// HeartbeatAt; a second replica with a fresh heartbeat beside it is either refused, naming the
// live one, on an engine that serves one replica (SQLite), or kept beside it (Postgres; the
// engine tests say which); a start deletes the rows whose heartbeat is stale, and their
// heartbeats are ErrNotFound; a stop deletes the row; an id twice is ErrConflict.
func testReplicas(t *testing.T, s index.Store) {
	ctx := t.Context()
	list := func() []string {
		t.Helper()
		rows, err := s.ListReplicas(ctx)
		must(t, err)
		return replicaIDs(rows)
	}
	a := index.Replica{ID: "a", Host: "host-a", PID: 7, Version: "v0.1.0", StartedAt: at(0)}
	must(t, s.StartReplica(ctx, &a, at(-120)))
	if !a.HeartbeatAt.Equal(at(0)) {
		t.Errorf("HeartbeatAt after the start = %v, want StartedAt %v", a.HeartbeatAt, at(0))
	}
	must(t, s.HeartbeatReplica(ctx, a.ID, at(30)))
	a.HeartbeatAt = at(30)
	rows, err := s.ListReplicas(ctx)
	must(t, err)
	same(t, "ListReplicas after a heartbeat", []index.Replica{a}, rows)

	b := index.Replica{ID: "b", Host: "host-b", PID: 8, Version: "v0.1.0", StartedAt: at(40)}
	var conflict *index.ReplicaConflictError
	switch err := s.StartReplica(ctx, &b, at(40-120)); {
	case errors.As(err, &conflict):
		same(t, "the live replica of the conflict", a, conflict.Live)
		wantErr(t, "a second replica", err, index.ErrConflict)
		same(t, "replicas after a refused start", []string{"a"}, list())
	case err != nil:
		t.Fatalf("a second replica: %v", err)
	default:
		same(t, "replicas after a second start", []string{"a", "b"}, list())
	}

	c := index.Replica{ID: "c", Host: "host-c", PID: 9, Version: "v0.1.0", StartedAt: at(200)}
	must(t, s.StartReplica(ctx, &c, at(200-120)))
	same(t, "replicas after a start that found the others stale", []string{"c"}, list())
	wantErr(t, "a heartbeat of a replica whose row a start deleted", s.HeartbeatReplica(ctx, a.ID, at(210)), index.ErrNotFound)
	wantErr(t, "the same id again", s.StartReplica(ctx, &index.Replica{ID: "c", Host: "h", StartedAt: at(210)}, at(90)),
		index.ErrConflict)

	must(t, s.StopReplica(ctx, c.ID))
	same(t, "replicas after the stop", []string{}, list())
	wantErr(t, "a second stop", s.StopReplica(ctx, c.ID), index.ErrNotFound)
}

// replicaIDs returns the IDs of replicas.
func replicaIDs(rows []index.Replica) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

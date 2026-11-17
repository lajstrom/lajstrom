package storetest

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The feed of the audit rows, the observer of their commits and the prune (docs/spec/rbac.md §5,
// docs/spec/index-schema.md, The audit feed and its prune; #1278).

// testAuditFeed: the ids ascend in the order of the inserts, whatever the times; the feed answers
// the rows above an id by id, at most the limit, with every filter of the list.
func testAuditFeed(t *testing.T, s index.Store) {
	ctx := t.Context()
	events := []index.AuditEvent{
		{At: at(5), Principal: "user:alice", Action: "push", Repo: "raw/scratch", Subject: "pkg 1.0"},
		{At: at(1), Principal: "token:7", Action: "push", Repo: "pypi/main", Subject: "requests 2.0"},
		{At: at(3), Principal: "user:alice", Action: "delete", Repo: "raw/scratch", Subject: "pkg 1.0", State: index.AuditPending},
		{At: at(2), Principal: "user:bob", Action: "push", Repo: "raw/scratch", Subject: "pkg 2.0"},
		{At: at(4), Principal: "user:alice", Action: "token.create", Subject: "user:alice"},
	}
	for i := range events {
		must(t, s.AppendAudit(ctx, &events[i]))
		if i > 0 && events[i].ID <= events[i-1].ID {
			t.Errorf("the id of row %d is %d, after %d: want the ids to ascend", i, events[i].ID, events[i-1].ID)
		}
	}
	ev := func(i ...int) []index.AuditEvent {
		out := []index.AuditEvent{}
		for _, j := range i {
			out = append(out, events[j])
		}
		return out
	}
	for _, tt := range []struct {
		name string
		f    index.AuditFilter
		want []index.AuditEvent
	}{
		{"from the oldest", index.AuditFilter{}, ev(0, 1, 2, 3, 4)},
		{"after an id", index.AuditFilter{After: events[1].ID}, ev(2, 3, 4)},
		{"after the newest", index.AuditFilter{After: events[4].ID}, ev()},
		{"limit", index.AuditFilter{Limit: 2}, ev(0, 1)},
		{"after an id, limit", index.AuditFilter{After: events[0].ID, Limit: 2}, ev(1, 2)},
		{"principal", index.AuditFilter{Principal: "user:alice"}, ev(0, 2, 4)},
		{"repo and action", index.AuditFilter{Repo: "raw/scratch", Action: "push"}, ev(0, 3)},
		{"state", index.AuditFilter{State: index.AuditPending}, ev(2)},
		{"since and until", index.AuditFilter{Since: at(2), Until: at(5)}, ev(2, 3, 4)},
		{"after an id, principal", index.AuditFilter{After: events[0].ID, Principal: "user:alice"}, ev(2, 4)},
	} {
		tt.f.Feed = true
		got, err := s.ListAudit(ctx, tt.f)
		must(t, err)
		same(t, "the feed "+tt.name, tt.want, got)
	}
	// Without Feed, After selects nothing: the list is newest first.
	got, err := s.ListAudit(ctx, index.AuditFilter{After: events[3].ID})
	must(t, err)
	same(t, "the list with After and without Feed", ev(0, 4, 2, 3, 1), got)
}

// auditWatch is what an observer of the audit rows (the drivers' ObserveAudit) got: the rows of
// each call.
type auditWatch struct {
	mu    sync.Mutex
	calls [][]index.AuditEvent
}

// watchAudit sets an observer of the audit rows on s and returns what it gets.
func watchAudit(t *testing.T, s index.Store) *auditWatch {
	t.Helper()
	o, ok := s.(interface {
		ObserveAudit(func([]index.AuditEvent))
	})
	if !ok {
		t.Fatalf("%T has no ObserveAudit", s)
	}
	w := &auditWatch{}
	o.ObserveAudit(func(rows []index.AuditEvent) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.calls = append(w.calls, rows)
	})
	return w
}

// take returns the calls since the last take, and forgets them.
func (w *auditWatch) take() [][]index.AuditEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.calls
	w.calls = nil
	return out
}

// auditRows returns the rows of ids as ListAudit answers them now.
func auditRows(t *testing.T, s index.Store, ids ...int64) []index.AuditEvent {
	t.Helper()
	out := []index.AuditEvent{}
	for _, id := range ids {
		got, err := s.ListAudit(t.Context(), index.AuditFilter{Feed: true, After: id - 1, Limit: 1})
		must(t, err)
		if len(got) != 1 || got[0].ID != id {
			t.Fatalf("no audit row %d: %+v", id, got)
		}
		out = append(out, got[0])
	}
	return out
}

// testAuditObserve: the observer gets the rows of each commit as committed, by id, each once in
// its last state: an append or a completion on the pool at once, those of a transaction after its
// commit and never before; a rollback, a completion refused, a deletion, a sweep that changes
// nothing and a transaction without an audit row give it nothing.
func testAuditObserve(t *testing.T, s index.Store) {
	ctx := t.Context()
	w := watchAudit(t, s)
	alice := newUser(t, s, "alice")
	pending := func(i int) index.AuditEvent {
		return index.AuditEvent{At: at(i), Principal: "user:" + alice.UID, Action: "push", Repo: "raw/scratch", Subject: "pkg 1.0",
			State: index.AuditPending, Details: json.RawMessage(`{"requestId": "r", "source": "api"}`)}
	}
	check := func(what string, want ...[]index.AuditEvent) {
		t.Helper()
		if want == nil {
			want = [][]index.AuditEvent{}
		}
		got := w.take()
		if got == nil {
			got = [][]index.AuditEvent{}
		}
		same(t, what, want, got)
	}

	plain := index.AuditEvent{At: at(1), Principal: "user:" + alice.UID, Action: "repo.create", Repo: "raw/scratch",
		Details: json.RawMessage(`{"source": "api"}`)}
	must(t, s.AppendAudit(ctx, &plain))
	check("an append", auditRows(t, s, plain.ID))
	if rows := auditRows(t, s, plain.ID); rows[0].PrincipalName != "alice" || rows[0].State != index.AuditOK {
		t.Errorf("the row of the append: %+v, want alice's name and ok", rows[0])
	}

	first, second := pending(2), pending(3)
	must(t, s.Tx(ctx, func(tx index.Writer) error {
		must(t, tx.AppendAudit(ctx, &first))
		must(t, tx.AppendAudit(ctx, &second))
		first.State = index.AuditOK
		must(t, tx.CompleteAudit(ctx, &first))
		check("a transaction before its commit")
		return nil
	}))
	check("a transaction: its rows in their last state, by id", auditRows(t, s, first.ID, second.ID))

	rolled := pending(4)
	boom := errors.New("boom")
	if err := s.Tx(ctx, func(tx index.Writer) error {
		must(t, tx.AppendAudit(ctx, &rolled))
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("Tx = %v", err)
	}
	check("a rollback")

	second.State, second.Error = index.AuditFailed, "the registry said no"
	must(t, s.CompleteAudit(ctx, &second))
	check("a completion", auditRows(t, s, second.ID))
	again := second
	wantErr(t, "a completed row completed again", s.CompleteAudit(ctx, &again), index.ErrNotFound)
	check("a completion refused")

	stale := pending(5)
	must(t, s.AppendAudit(ctx, &stale))
	check("a pending append", auditRows(t, s, stale.ID))
	n, err := s.ExpirePendingAudit(ctx, at(6))
	must(t, err)
	if n != 1 {
		t.Errorf("ExpirePendingAudit = %d, want 1", n)
	}
	check("a sweep", auditRows(t, s, stale.ID))
	if rows := auditRows(t, s, stale.ID); rows[0].State != index.AuditUnknown {
		t.Errorf("the swept row is %q, want unknown", rows[0].State)
	}
	_, err = s.ExpirePendingAudit(ctx, at(6))
	must(t, err)
	check("a sweep that changes nothing")

	must(t, s.Tx(ctx, func(tx index.Writer) error { newRepo(t, tx, "raw", "scratch"); return nil }))
	check("a transaction without an audit row")
	pruned, err := s.PruneAudit(ctx, at(100), stale.ID, 10)
	must(t, err)
	if pruned != 4 {
		t.Errorf("PruneAudit = %d, want 4", pruned)
	}
	check("a prune")
}

// testAuditObserveImport: an import of a snapshot gives the observer its own row only, after
// its commit, never the rows it brings back.
func testAuditObserveImport(t *testing.T, open func(*testing.T) index.Store) {
	ctx := t.Context()
	src := open(t)
	for i := range 3 {
		must(t, src.AppendAudit(ctx, &index.AuditEvent{At: at(i), Principal: "anonymous", Action: "push"}))
	}
	cp := export(t, src)
	dst := open(t)
	w := watchAudit(t, dst)
	event := index.AuditEvent{Principal: "anonymous", Action: "snapshot.restore", Subject: "lajstrom/control-plane:x", At: at(1000)}
	must(t, snapshotter(t, dst).ImportControlPlane(ctx, cp, index.ImportOptions{Event: &event}))
	same(t, "the observed rows of an import", [][]index.AuditEvent{auditRows(t, dst, event.ID)}, w.take())
}

// testAuditPrune: SpanAudit counts the rows before a cut-off but those pending and a referrer's
// push, with the first and last ids and times; PruneAudit deletes those up to an id, the lowest
// first, at most its limit at a time, and keeps the rest; an id is never used again, the newest
// deleted included.
func testAuditPrune(t *testing.T, s index.Store) {
	ctx := t.Context()
	referrer := dg("signature")
	rows := []index.AuditEvent{
		{At: at(1), Principal: "anonymous", Action: "push", Repo: "raw/scratch", Subject: "pkg 1.0"},                            // 0: pruned
		{At: at(2), Principal: "anonymous", Action: "push", Repo: "oci/app", Subject: referrer.String(), Digest: dg("sbom")},    // 1: a referrer's
		{At: at(3), Principal: "anonymous", Action: "push", Repo: "raw/scratch", Subject: "pkg 2.0", State: index.AuditPending}, // 2: pending
		{At: at(4), Principal: "anonymous", Action: "delete", Repo: "oci/app", Subject: referrer.String()},                      // 3: pruned
		{At: at(5), Principal: "anonymous", Action: "push", Repo: "raw/scratch", Subject: "pkg 3.0", State: index.AuditUnknown}, // 4: pruned
		{At: at(6), Principal: "anonymous", Action: "push", Repo: "raw/scratch", Subject: "pkg 4.0", State: index.AuditFailed},  // 5: pruned
		{At: at(10), Principal: "anonymous", Action: "repo.create", Repo: "raw/scratch"},                                        // 6: at the cut-off
		{At: at(7), Principal: "anonymous", Action: "push", Repo: "oci/app", Subject: digest.SHA512.FromString("x").String()},   // 7: a referrer's
		{At: at(8), Principal: "anonymous", Action: "token.create", Subject: "user:x"},                                          // 8: pruned, the newest id
	}
	for i := range rows {
		must(t, s.AppendAudit(ctx, &rows[i]))
	}
	id := func(i int) int64 { return rows[i].ID }
	for _, tt := range []struct {
		before time.Time
		want   index.AuditSpan
	}{
		{at(10), index.AuditSpan{Rows: 5, FirstID: id(0), LastID: id(8), FirstAt: at(1), LastAt: at(8)}},
		{at(5), index.AuditSpan{Rows: 2, FirstID: id(0), LastID: id(3), FirstAt: at(1), LastAt: at(4)}},
		{at(1), index.AuditSpan{}},
	} {
		got, err := s.SpanAudit(ctx, tt.before)
		must(t, err)
		same(t, "SpanAudit before "+tt.before.Format(time.RFC3339), tt.want, got)
	}
	prune := func(upTo int64, limit, want int) {
		t.Helper()
		n, err := s.PruneAudit(ctx, at(10), upTo, limit)
		must(t, err)
		if n != want {
			t.Errorf("PruneAudit(up to %d, %d) = %d, want %d", upTo, limit, n, want)
		}
	}
	left := func(want ...int) {
		t.Helper()
		got, err := s.ListAudit(ctx, index.AuditFilter{Feed: true})
		must(t, err)
		same(t, "the rows left", ids(rowsOf(rows, want...), func(e index.AuditEvent) int64 { return e.ID }),
			ids(got, func(e index.AuditEvent) int64 { return e.ID }))
	}
	prune(id(5), 2, 2) // the lowest ids first: 0 and 3
	left(1, 2, 4, 5, 6, 7, 8)
	prune(id(5), 0, 0)
	prune(id(5), 2, 2) // 4 and 5
	prune(id(5), 2, 0) // 8 is above the bound
	left(1, 2, 6, 7, 8)
	prune(id(8), 10_000, 1)
	left(1, 2, 6, 7)

	n, err := s.PruneAudit(ctx, at(11), id(6), 10)
	must(t, err)
	if n != 1 {
		t.Errorf("PruneAudit of the newest row = %d, want 1", n)
	}
	next := index.AuditEvent{Principal: "anonymous", Action: "audit.prune", Subject: "audit"}
	must(t, s.AppendAudit(ctx, &next))
	if next.ID <= id(8) {
		t.Errorf("an id after a prune of the newest rows: %d, want above %d, never used again", next.ID, id(8))
	}
}

// rowsOf returns the rows of the indices i.
func rowsOf(rows []index.AuditEvent, i ...int) []index.AuditEvent {
	out := []index.AuditEvent{}
	for _, j := range i {
		out = append(out, rows[j])
	}
	return out
}

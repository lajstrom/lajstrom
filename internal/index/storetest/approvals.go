package storetest

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// newApproval creates a pending request of kind and subject by alice, made at at(0), which
// expires at expires.
func newApproval(t *testing.T, s index.Writer, kind, subject string, expires int) index.Approval {
	t.Helper()
	a := index.Approval{Kind: kind, Subject: subject, Change: json.RawMessage(`{"principal":"user:bob","role":"admin","repoMask":"*/*"}`),
		RequestedBy: "user:alice", RequestedAt: at(0), ExpiresAt: at(expires)}
	must(t, s.CreateApproval(t.Context(), &a))
	return a
}

// testApprovals: a request for a second admin is created pending whatever its State, read back
// as written, listed newest first by state below an id; the same Kind and Subject pending twice
// is ErrConflict, another kind is not.
func testApprovals(t *testing.T, s index.Store) {
	ctx := t.Context()
	a := index.Approval{Kind: index.ApprovalBinding, Subject: "user:bob admin */*", State: index.ApprovalApproved,
		Change: json.RawMessage(`{"principal":"user:bob","role":"admin","repoMask":"*/*"}`), ChangeReason: "CHG-1",
		RequestedBy: "user:alice", RequestedAt: at(0), ExpiresAt: at(7 * 86400), DecidedBy: "user:mallory", Result: "binding:1"}
	must(t, s.CreateApproval(ctx, &a))
	if a.ID == 0 || a.State != index.ApprovalPending || a.DecidedBy != "" || a.Result != "" {
		t.Fatalf("CreateApproval filled %+v, want an ID, pending, undecided", a)
	}
	got, err := s.GetApproval(ctx, a.ID)
	must(t, err)
	same(t, "GetApproval", a, *got)
	wantErr(t, "the same change pending twice", s.CreateApproval(ctx, &index.Approval{Kind: index.ApprovalBinding,
		Subject: a.Subject, Change: a.Change, RequestedBy: "user:carol", ExpiresAt: at(9)}), index.ErrConflict)
	tok := newApproval(t, s, index.ApprovalDeployToken, a.Subject, 100)
	other := newApproval(t, s, index.ApprovalBinding, "group:ops maintainer */*", 50)
	if !other.RequestedAt.Equal(at(0)) {
		t.Errorf("RequestedAt %v, want the one given", other.RequestedAt)
	}
	_, err = s.GetApproval(ctx, 1<<40)
	wantErr(t, "GetApproval of no row", err, index.ErrNotFound)

	for _, tt := range []struct {
		name   string
		state  string
		before int64
		limit  int
		want   []int64
	}{
		{"every request, newest first", "", 0, 10, []int64{other.ID, tok.ID, a.ID}},
		{"a page", "", 0, 2, []int64{other.ID, tok.ID}},
		{"below an id", "", tok.ID, 10, []int64{a.ID}},
		{"pending", index.ApprovalPending, 0, 10, []int64{other.ID, tok.ID, a.ID}},
		{"none approved", index.ApprovalApproved, 0, 10, []int64{}},
		{"a limit below 1", "", 0, 0, []int64{}},
	} {
		list, err := s.ListApprovals(ctx, tt.state, tt.before, tt.limit)
		must(t, err)
		same(t, "ListApprovals: "+tt.name, tt.want, ids(list, func(a index.Approval) int64 { return a.ID }))
	}
}

// testApprovalDecisions: a decision is stored while the row is pending (else ErrConflict, and
// nothing changes), after which the same change may be requested again; an expiry marks the
// pending rows whose expiry is not after its time, once. No write moves auth_version: a request
// grants nothing (docs/spec/rbac.md §13).
func testApprovalDecisions(t *testing.T, s index.Store) {
	ctx := t.Context()
	v0, err := s.AuthVersion(ctx)
	must(t, err)
	a := newApproval(t, s, index.ApprovalBinding, "user:bob admin */*", 7*86400)
	tok := newApproval(t, s, index.ApprovalDeployToken, "raw/* fleet", 100)
	other := newApproval(t, s, index.ApprovalBinding, "group:ops maintainer */*", 50)
	decided := index.Approval{ID: a.ID, State: index.ApprovalApproved, DecidedBy: "user:carol", DecidedAt: ptr(at(60)),
		DecisionReason: "CHG-1 approved", Result: "binding:7"}
	must(t, s.DecideApproval(ctx, &decided))
	want := a
	want.State, want.DecidedBy, want.DecidedAt, want.DecisionReason, want.Result = decided.State, decided.DecidedBy, ptr(at(60)),
		decided.DecisionReason, decided.Result
	same(t, "DecideApproval fills the row", want, decided)
	got, err := s.GetApproval(ctx, a.ID)
	must(t, err)
	same(t, "a decision stored", want, *got)
	again := index.Approval{ID: a.ID, State: index.ApprovalRejected, DecidedBy: "user:dave"}
	wantErr(t, "a second decision", s.DecideApproval(ctx, &again), index.ErrConflict)
	got, err = s.GetApproval(ctx, a.ID)
	must(t, err)
	same(t, "a refused decision changes nothing", want, *got)
	wantErr(t, "DecideApproval of no row", s.DecideApproval(ctx, &index.Approval{ID: 1 << 40, State: index.ApprovalRejected}),
		index.ErrNotFound)
	newApproval(t, s, index.ApprovalBinding, a.Subject, 9000) // the change again, once the first is decided

	expired, err := s.ExpireApprovals(ctx, at(100))
	must(t, err)
	same(t, "ExpireApprovals at the expiry of one, after the other's", []int64{tok.ID, other.ID},
		ids(expired, func(a index.Approval) int64 { return a.ID }))
	for _, e := range expired {
		if e.State != index.ApprovalExpired || e.DecidedBy != "anonymous" || e.DecidedAt == nil || !e.DecidedAt.Equal(at(100)) {
			t.Errorf("ExpireApprovals returned %+v, want expired by anonymous at %v", e, at(100))
		}
	}
	got, err = s.GetApproval(ctx, tok.ID)
	must(t, err)
	same(t, "an expiry stored", expired[0], *got)
	if expired, err = s.ExpireApprovals(ctx, at(200)); err != nil || len(expired) != 0 {
		t.Errorf("ExpireApprovals again: %+v, %v; want none", expired, err)
	}
	if v, err := s.AuthVersion(ctx); err != nil || v != v0 {
		t.Errorf("auth_version after the requests: %d, %v; want %d", v, err, v0)
	}
}

// testApprovalRace: of decisions of one request made at once, one is made and the others are
// ErrConflict; of the replicas that mark the expiries at once, each request is returned by one.
func testApprovalRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	const n = 8
	a := newApproval(t, s, index.ApprovalBinding, "user:bob admin */*", 100)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		state := index.ApprovalApproved
		if i%2 == 1 {
			state = index.ApprovalRejected
		}
		wg.Go(func() {
			errs[i] = s.DecideApproval(ctx, &index.Approval{ID: a.ID, State: state, DecidedBy: "user:carol"})
		})
	}
	wg.Wait()
	made := 0
	for _, err := range errs {
		if err == nil {
			made++
			continue
		}
		wantErr(t, "a decision that lost", err, index.ErrConflict)
	}
	if made != 1 {
		t.Errorf("%d decisions of one request at once: %d made, want 1", n, made)
	}

	var want []int64
	for i := range n {
		want = append(want, newApproval(t, s, index.ApprovalDeployToken, "raw/* fleet-"+string(rune('a'+i)), 100).ID)
	}
	marked := make([][]index.Approval, n)
	for i := range n {
		wg.Go(func() {
			var err error
			marked[i], err = s.ExpireApprovals(ctx, at(100))
			errs[i] = err
		})
	}
	wg.Wait()
	seen := map[int64]int{}
	for i := range n {
		must(t, errs[i])
		for _, e := range marked[i] {
			seen[e.ID]++
		}
	}
	for _, id := range want {
		if seen[id] != 1 {
			t.Errorf("request %d returned by %d replicas, want 1", id, seen[id])
		}
	}
	if len(seen) != len(want) {
		t.Errorf("the replicas returned %d requests, want %d", len(seen), len(want))
	}
}

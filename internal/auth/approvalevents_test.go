package auth

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// recordedEvents is an ApprovalEvents that records each event it is asked to write, with the
// state of the request in the step's transaction, and fails with fail.
type recordedEvents struct {
	mu   sync.Mutex
	got  []recordedEvent
	fail error
}

// recordedEvent is an event of a request: its audit row, the request as passed, and the state
// of its row as the transaction of its step reads it.
type recordedEvent struct {
	ev    index.AuditEvent
	a     Approval
	state string
}

func (r *recordedEvents) ApprovalEvent(ctx context.Context, w index.Writer, ev index.AuditEvent, a Approval) error {
	row, err := w.GetApproval(ctx, a.ID)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, recordedEvent{ev: ev, a: a, state: row.State})
	return r.fail
}

// actions returns the actions of the events recorded, in order.
func (r *recordedEvents) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.got {
		out = append(out, e.ev.Action)
	}
	return out
}

// TestApprovalEvents: each request, approval, rejection and expiry writes the event of its audit
// row once, in its transaction, which reads the request as the step leaves it, with the request
// as the API answers it: never the secret of a deploy token, its hash, nor the credential and the
// sign-in the request keeps. A step refused writes none (docs/spec/rbac.md §13, #1336).
func TestApprovalEvents(t *testing.T) {
	f := newApprovals(t)
	rec := &recordedEvents{}
	f.s.Webhooks = rec
	ctx, rootCtx := f.as(t, "admin", password), f.as(t, "root", password)
	bind, err := f.s.RequestBinding(ctx, "user:alice", RoleAdmin, MaskAll, cli)
	must(t, err)
	tok, secret, err := f.s.RequestMaskDeploy(ctx, "raw/*", "fleet", []string{"write"}, Expiry{}, cli)
	must(t, err)
	_, err = f.s.Approve(ctx, bind.ID, cli)
	wantErr(t, "an approval by the requester", err, ErrScope, "another admin does")
	_, err = f.s.Approve(rootCtx, bind.ID, cli)
	must(t, err)
	_, err = f.s.Reject(ctx, tok.ID, cli)
	must(t, err)
	late, _, err := f.s.RequestMaskDeploy(ctx, "oci/*", "mirror", []string{"write"}, Expiry{}, cli)
	must(t, err)
	f.now = f.now.Add(ApprovalLifetime)
	_, err = f.s.Expire(context.Background(), Origin{Source: "expiry"})
	must(t, err)

	want := []struct {
		action, state string
		id            int64
	}{
		{ActionApprovalRequest, index.ApprovalPending, bind.ID},
		{ActionApprovalRequest, index.ApprovalPending, tok.ID},
		{ActionApprovalApprove, index.ApprovalApproved, bind.ID},
		{ActionApprovalReject, index.ApprovalRejected, tok.ID},
		{ActionApprovalRequest, index.ApprovalPending, late.ID},
		{ActionApprovalExpire, index.ApprovalExpired, late.ID},
	}
	if len(rec.got) != len(want) {
		t.Fatalf("events %v, want %d", rec.actions(), len(want))
	}
	hash := HashSecret(secret)
	for i, w := range want {
		e := rec.got[i]
		if e.ev.Action != w.action || e.ev.ID == 0 || e.ev.Subject != "approval:"+strconv.FormatInt(w.id, 10) || e.a.ID != w.id ||
			e.a.State != w.state || e.state != w.state {
			t.Errorf("event %d: %s of %s, request %d %s (%s in its transaction); want %s of approval:%d %s", i, e.ev.Action,
				e.ev.Subject, e.a.ID, e.a.State, e.state, w.action, w.id, w.state)
		}
		for _, never := range []string{secret, hash, "hash", "createdBy", "signIn", "credential"} {
			if strings.Contains(string(e.a.Change), never) {
				t.Errorf("event %d holds %q: %s", i, never, e.a.Change)
			}
		}
	}
	if e := rec.got[2]; e.a.Result == "" || e.a.DecidedBy != f.uid(t, "root") {
		t.Errorf("the approval's event: %+v", e.a)
	}
}

// TestApprovalEventsRollBack: a step whose event is not written is not made: no request, the
// request still pending after an approval or an expiry that failed, no binding; an event is
// never written for a step that rolls back.
func TestApprovalEventsRollBack(t *testing.T) {
	f := newApprovals(t)
	ctx := f.as(t, "admin", password)
	a, err := f.s.RequestBinding(ctx, "user:alice", RoleAdmin, MaskAll, cli)
	must(t, err)
	boom := errors.New("boom")
	f.s.Webhooks = &recordedEvents{fail: boom}
	_, err = f.s.RequestBinding(ctx, "group:ops", RoleAdmin, MaskAll, cli)
	if !errors.Is(err, boom) {
		t.Errorf("a request whose event fails: %v", err)
	}
	if _, err := f.s.Approve(f.as(t, "root", password), a.ID, cli); !errors.Is(err, boom) {
		t.Errorf("an approval whose event fails: %v", err)
	}
	f.now = f.now.Add(ApprovalLifetime)
	if _, err := f.s.Expire(context.Background(), Origin{Source: "expiry"}); !errors.Is(err, boom) {
		t.Errorf("an expiry whose event fails: %v", err)
	}
	list, err := f.s.List(context.Background(), "", 0, 10)
	must(t, err)
	if len(list) != 1 || list[0].State != index.ApprovalPending {
		t.Errorf("requests after the failures: %+v, want the first, pending", list)
	}
	if bs, _ := f.st.ListBindings(context.Background(), f.uid(t, "alice")); len(bs) != 0 {
		t.Errorf("bindings after the approval that failed: %+v", bs)
	}
	for _, action := range []string{ActionApprovalApprove, ActionApprovalExpire} {
		if evs, _ := f.st.ListAudit(context.Background(), index.AuditFilter{Action: action}); len(evs) != 0 {
			t.Errorf("%s rows of steps rolled back: %+v", action, evs)
		}
	}
}

// TestApprovalExpiryEventOnce: of the replicas that expire at once, each with its own outbox,
// one marks each request, and its event is written once in all.
func TestApprovalExpiryEventOnce(t *testing.T) {
	f := newApprovals(t)
	ctx := f.as(t, "admin", password)
	for _, p := range []string{"user:alice", "group:ops", "group:dev"} {
		_, err := f.s.RequestBinding(ctx, p, RoleAdmin, MaskAll, cli)
		must(t, err)
	}
	f.now = f.now.Add(ApprovalLifetime)
	const replicas = 6
	recs := make([]*recordedEvents, replicas)
	var wg sync.WaitGroup
	for i := range recs {
		recs[i] = &recordedEvents{}
		replica := *f.s
		replica.Webhooks = recs[i]
		wg.Go(func() {
			if _, err := replica.Expire(context.Background(), Origin{Source: "expiry"}); err != nil {
				t.Errorf("replica %d: %v", i, err)
			}
		})
	}
	wg.Wait()
	var ids []int64
	for _, r := range recs {
		for _, e := range r.got {
			ids = append(ids, e.a.ID)
		}
	}
	slices.Sort(ids)
	if len(ids) != 3 || len(slices.Compact(slices.Clone(ids))) != 3 {
		t.Errorf("the expiry's events of %d replicas: requests %v, want each of 3 once", replicas, ids)
	}
}

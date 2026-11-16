package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// auditRows is an index.Store whose AppendAudit records the rows, or fails with err.
type auditRows struct {
	index.Store
	mu   sync.Mutex
	rows []index.AuditEvent
	err  error
}

func (r *auditRows) AppendAudit(_ context.Context, ev *index.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.rows = append(r.rows, *ev)
	return nil
}

func (r *auditRows) all() []index.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]index.AuditEvent(nil), r.rows...)
}

// details decodes the details of ev.
func details(t *testing.T, ev index.AuditEvent) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(ev.Details, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// blockFixture is an authenticator whose Blocks writes into rows, and a context of a request from
// 2001:db8:1:2::7.
func blockFixture(t *testing.T) (*authFixture, *auditRows, context.Context) {
	rows := &auditRows{}
	f := newAuthFixture(t, Options{})
	f.a.blocks = &Blocks{Index: rows, Now: f.clock.Now}
	ctx := WithRequest(WithClientAddress(context.Background(), "2001:db8:1:2::7"), Request{ID: "req-1", Address: "2001:db8:1:2::7"})
	return f, rows, ctx
}

// TestLoginBlock is #1284's 2C (rbac.md §5): the failure that fills the budget of an account's
// throttle writes one login.block row, by anonymous, the account by its uid; the failures the
// throttle then refuses write no more rows within the window, and a single failure writes none.
func TestLoginBlock(t *testing.T) {
	f, rows, ctx := blockFixture(t)
	start := f.clock.now
	_, _, _ = f.authenticateFrom(ctx, "alice", "wrong")
	if n := len(rows.all()); n != 0 {
		t.Fatalf("one failure wrote %d rows", n)
	}
	for range 2 * LoginFailures {
		f.clock.advance(time.Second)
		_, _, _ = f.authenticateFrom(ctx, "alice", "wrong")
	}
	got := rows.all()
	if len(got) != 1 {
		t.Fatalf("rows %+v, want one", got)
	}
	ev, d := got[0], details(t, got[0])
	want := map[string]any{"limit": LimitAccount, "credential": "password", "failures": float64(LoginFailures), "window": "1m0s",
		"until": start.Add(LoginWindow).Format(time.RFC3339), "requestId": "req-1", "suppressed": nil}
	for k, v := range want {
		if d[k] != v {
			t.Errorf("%s = %v, want %v", k, d[k], v)
		}
	}
	if ev.Action != ActionLoginBlock || ev.Principal != "anonymous" || ev.Subject != "user:"+testUID(1) || ev.IP != "2001:db8:1:2::7" {
		t.Errorf("row %+v", ev)
	}
}

// TestLoginBlockUnknown: a name that no user has is blocked as unknown, never by the name.
func TestLoginBlockUnknown(t *testing.T) {
	f, rows, _ := blockFixture(t)
	typed := "hunter2-typed-as-a-name"
	other := WithClientAddress(context.Background(), "192.0.2.9")
	for range LoginFailures {
		_, _, _ = f.authenticateFrom(other, typed, "wrong")
	}
	got := rows.all()
	if len(got) != 1 || got[0].Subject != AccountUnknown || strings.Contains(string(got[0].Details), typed) {
		t.Fatalf("rows %+v, want one of the account unknown, without the name", got)
	}
}

// TestLoginBlockAddress: failures across names from one /64 trip the address's throttle, whose row
// names the /64.
func TestLoginBlockAddress(t *testing.T) {
	f, rows, _ := blockFixture(t)
	near := WithClientAddress(context.Background(), "2001:db8:1:2::8")
	for i := range AddressFailures {
		_, _, _ = f.authenticateFrom(near, fmt.Sprintf("u%d", i), "x-password")
	}
	got := rows.all()
	if len(got) != 1 || got[0].Subject != "2001:db8:1:2::/64" || details(t, got[0])["limit"] != LimitAddress ||
		details(t, got[0])["failures"] != float64(AddressFailures) {
		t.Fatalf("rows %+v, want one of the address's /64", got)
	}
}

// TestBlocksBound: a replica writes at most BlockRows rows within BlockWindow, whatever the keys;
// the next row after carries suppressed, the trips left out; a row that cannot be written is
// logged and changes nothing else.
func TestBlocksBound(t *testing.T) {
	c := &clock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	rows := &auditRows{}
	bs := &Blocks{Index: rows, Now: c.Now}
	trip := func(i int) {
		bs.record(context.Background(), block{limit: LimitAddress, credential: "password", subject: fmt.Sprintf("192.0.2.%d", i),
			failures: AddressFailures, window: LoginWindow, until: c.Now().Add(LoginWindow)})
	}
	for i := range BlockRows + 5 {
		trip(i)
	}
	if n := len(rows.all()); n != BlockRows {
		t.Fatalf("%d rows within a window, want %d", n, BlockRows)
	}
	c.advance(BlockWindow)
	trip(99)
	got := rows.all()
	if len(got) != BlockRows+1 || details(t, got[BlockRows])["suppressed"] != float64(5) {
		t.Fatalf("the row after the bound: %s, want suppressed 5", got[len(got)-1].Details)
	}
	trip(100)
	if d := details(t, rows.all()[BlockRows+1]); d["suppressed"] != nil {
		t.Errorf("suppressed counted twice: %v", d)
	}

	rows.err = errors.New("the database is down")
	trip(101) // logged, no panic
	var nilBlocks *Blocks
	nilBlocks.record(context.Background(), block{}) // writes nothing
}

// TestBootstrapBlock: the wrong bootstrap token that fills an address's budget writes a
// login.block row of the limit bootstrap.
func TestBootstrapBlock(t *testing.T) {
	bs, _, _, _ := newBootstrap(t, 1)
	b := bs[0]
	secret := strings.Repeat("s", 40)
	arm(t, b, secret)
	rows := &auditRows{}
	b.Blocks = &Blocks{Index: rows}
	ctx := WithClientAddress(t.Context(), "192.0.2.7")
	for range BootstrapFailures + 3 {
		_, _ = b.Exchange(ctx, secret+"x", "root", "root-password", Origin{})
	}
	got := rows.all()
	if len(got) != 1 || got[0].Subject != "192.0.2.7" || details(t, got[0])["limit"] != LimitBootstrap ||
		details(t, got[0])["credential"] != RefusedBootstrap {
		t.Fatalf("rows %+v, want one of the limit bootstrap", got)
	}
}

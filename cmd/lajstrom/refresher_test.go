package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/oidc"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

// testIssuer is the IdP of the refresh's tests; refreshSecret is in every refresh token of
// theirs, so that a test finds one anywhere.
const (
	testIssuer      = "https://idp.example"
	refreshInterval = 15 * time.Minute
	refreshSecret   = "rt-SERVE-SECRET"
)

// fakeIDP answers each refresh with answer, and counts the calls by refresh token and the calls
// at once.
type fakeIDP struct {
	mu            sync.Mutex
	answer        func(rt string) (*oidc.Refreshed, error)
	calls         map[string]int
	running, most int
	delay         time.Duration
}

func (f *fakeIDP) Refresh(_ context.Context, rt string) (*oidc.Refreshed, error) {
	f.mu.Lock()
	f.calls[rt]++
	f.running++
	f.most = max(f.most, f.running)
	answer, delay := f.answer, f.delay
	f.mu.Unlock()
	time.Sleep(delay)
	f.mu.Lock()
	f.running--
	f.mu.Unlock()
	return answer(rt)
}

func (f *fakeIDP) set(answer func(rt string) (*oidc.Refreshed, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = answer
}

func (f *fakeIDP) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

// refreshFixture is an index, the master key that seals its refresh tokens, the fake IdP and a
// clock, for serve's refresher.
type refreshFixture struct {
	t      *testing.T
	st     *sqlite.Store
	m      *keys.Master
	idp    *fakeIDP
	now    atomic.Pointer[time.Time]
	logs   *bytes.Buffer
	log    *slog.Logger
	users  *auth.Users
	tokens *auth.Tokens
	logins atomic.Int64
}

func newRefreshFixture(t *testing.T) *refreshFixture {
	t.Helper()
	st, err := sqlite.Open(t.Context(), "file:"+filepath.Join(t.TempDir(), "refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	m, err := keys.ParseMaster(strings.Repeat("ef", 32))
	if err != nil {
		t.Fatal(err)
	}
	f := &refreshFixture{t: t, st: st, m: m, logs: &bytes.Buffer{}, idp: &fakeIDP{calls: map[string]int{}}}
	f.log = slog.New(slog.NewTextHandler(syncWriter{w: f.logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	f.now.Store(&now)
	f.users = &auth.Users{Index: st, Now: f.clock, Log: f.log}
	f.tokens = &auth.Tokens{Index: st, Now: f.clock, Log: f.log}
	f.idp.set(func(string) (*oidc.Refreshed, error) { return &oidc.Refreshed{}, nil })
	return f
}

// syncWriter serializes the writes of the refresher's goroutines to the log's buffer.
type syncWriter struct{ w *bytes.Buffer }

var logMu sync.Mutex

func (s syncWriter) Write(p []byte) (int, error) {
	logMu.Lock()
	defer logMu.Unlock()
	return s.w.Write(p)
}

func (f *refreshFixture) clock() time.Time { return *f.now.Load() }

func (f *refreshFixture) advance(d time.Duration) {
	n := f.clock().Add(d)
	f.now.Store(&n)
}

// refresher is serve's refresher on the fixture for the replica.
func (f *refreshFixture) refresher(replica string) *refresher {
	return &refresher{st: f.st, idp: f.idp, keys: f.m, replica: replica, interval: refreshInterval,
		now: f.clock, log: f.log}
}

// login makes the OIDC login of name, its identity's subject sub, with groups, as the callback
// and the mint do: its refresh token rt sealed into a row, joined to its token. It returns the
// user and the token.
func (f *refreshFixture) login(name, sub string, groups []string, rt string) (*index.User, *auth.Token) {
	f.t.Helper()
	ctx := context.Background()
	id := f.logins.Add(1)
	keep := func(ctx context.Context, w index.Writer, identity int64) (bool, error) {
		row := index.OIDCRefresh{LoginID: &id, IdentityID: identity, Sealed: []byte{0}, CreatedAt: f.clock(),
			NextAt: f.clock().Add(oidc.LoginTTL)}
		if err := w.CreateOIDCRefresh(ctx, &row); err != nil {
			return false, err
		}
		sealed, err := f.m.Seal(auth.RefreshAAD(row.ID), []byte(rt))
		if err != nil {
			return false, err
		}
		_, err = w.Reseal(ctx, index.SealedRefresh, row.ID, []byte{0}, sealed)
		return true, err
	}
	u, err := f.users.LoginOIDC(ctx, auth.OIDCLogin{Issuer: testIssuer, Subject: sub, Username: name, Groups: groups,
		Lifetime: 24 * time.Hour, Refresh: keep}, auth.Origin{})
	if err != nil {
		f.t.Fatal(err)
	}
	tok, _, err := f.tokens.LoginToken(ctx, u, 24*time.Hour, auth.LoginRefresh{Login: id, Interval: refreshInterval}, auth.Origin{})
	if err != nil {
		f.t.Fatal(err)
	}
	return u, tok
}

// rows returns every row of oidc_refresh, by due time, its claim released as it was.
func (f *refreshFixture) rows() []index.OIDCRefresh {
	f.t.Helper()
	ctx := context.Background()
	far := f.clock().Add(1000 * time.Hour)
	rows, err := f.st.ClaimOIDCRefresh(ctx, "peek", far, far, 100)
	if err != nil {
		f.t.Fatal(err)
	}
	for i := range rows {
		release := rows[i]
		release.Sealed = nil
		if err := f.st.FinishOIDCRefresh(ctx, &release, "peek"); err != nil {
			f.t.Fatal(err)
		}
		rows[i].ClaimedBy, rows[i].ClaimedUntil = "", nil
	}
	return rows
}

// rowOf returns the row of the token, or nil.
func (f *refreshFixture) rowOf(tokenID int64) *index.OIDCRefresh {
	for _, r := range f.rows() {
		if r.TokenID != nil && *r.TokenID == tokenID {
			return &r
		}
	}
	return nil
}

// opened returns the refresh token of the row, opened with the master key.
func (f *refreshFixture) opened(r *index.OIDCRefresh) string {
	f.t.Helper()
	plain, err := f.m.Open(auth.RefreshAAD(r.ID), r.Sealed)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(plain)
}

// live reports whether the token is neither revoked nor expired.
func (f *refreshFixture) live(id int64) bool {
	page, err := f.st.ListTokenPage(context.Background(), index.TokenFilter{After: id - 1, Limit: 1, LiveAt: f.clock()})
	return err == nil && len(page) == 1 && page[0].ID == id
}

// audit returns the audit rows of action.
func (f *refreshFixture) audit(action string) []index.AuditEvent {
	evs, err := f.st.ListAudit(context.Background(), index.AuditFilter{Action: action})
	if err != nil {
		f.t.Fatal(err)
	}
	return evs
}

// due checks that r is due within the tenth of d before d after base.
func due(t *testing.T, what string, r *index.OIDCRefresh, base time.Time, d time.Duration) {
	t.Helper()
	if r == nil || r.NextAt.Before(base.Add(d-d/10)) || r.NextAt.After(base.Add(d)) {
		t.Errorf("%s: the row %+v, want due %v less up to %v", what, r, base.Add(d), d/10)
	}
}

// TestRefreshRotation: a success keeps the new refresh token when the IdP rotated it, in the
// transaction that frees the claim, records the IdP's confirmation and the next due time, and
// moves neither the token's expiry nor the user's last activity; the next refresh sends the new
// token, and an IdP that does not rotate leaves the token as it is.
func TestRefreshRotation(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	u, tok := f.login("alice", "a", []string{"dev"}, refreshSecret+"-1")
	n := 0
	f.idp.set(func(rt string) (*oidc.Refreshed, error) {
		if rt != fmt.Sprintf("%s-%d", refreshSecret, n+1) {
			return nil, fmt.Errorf("%w: a spent token", oidc.ErrRefused)
		}
		n++
		return &oidc.Refreshed{RefreshToken: fmt.Sprintf("%s-%d", refreshSecret, n+1)}, nil
	})
	before := testutil.ToFloat64(metrics.OIDCRefreshes.WithLabelValues(refreshOK))
	r := f.refresher("r1")
	r.round(ctx)
	if f.idp.total() != 0 {
		t.Fatal("a login refreshed before it is due")
	}
	for _, want := range []string{"-2", "-3"} {
		f.advance(refreshInterval)
		r.round(ctx)
		f.confirmedWith(tok.ID, refreshSecret+want)
	}
	f.idp.set(func(string) (*oidc.Refreshed, error) { return &oidc.Refreshed{}, nil })
	f.advance(refreshInterval)
	r.round(ctx)
	f.confirmedWith(tok.ID, refreshSecret+"-3")
	if got := testutil.ToFloat64(metrics.OIDCRefreshes.WithLabelValues(refreshOK)) - before; got != 3 {
		t.Errorf("lajstrom_oidc_refreshes_total{outcome=ok} rose by %v, want 3", got)
	}
	f.checkUnmoved(u, tok)
	if evs := f.audit(""); len(evs) != 3 { // user.create, user.login, token.create
		t.Errorf("a success without a change of groups wrote audit rows: %d", len(evs))
	}
}

// confirmedWith checks that the row of the token was confirmed now, keeps rt, has no error and
// no claim, and is due the interval after now less the jitter.
func (f *refreshFixture) confirmedWith(tokenID int64, rt string) {
	f.t.Helper()
	row := f.rowOf(tokenID)
	if row == nil || f.opened(row) != rt || !row.RefreshedAt.Equal(f.clock()) || row.Failures != 0 || row.ClaimedBy != "" {
		f.t.Fatalf("the row after a success: %+v, want %s kept", row, rt)
	}
	due(f.t, "after a success", row, f.clock(), refreshInterval)
}

// checkUnmoved checks that the token's expiry and the user's last activity are as at the login.
func (f *refreshFixture) checkUnmoved(u *index.User, tok *auth.Token) {
	f.t.Helper()
	toks, _ := f.st.ListTokens(context.Background(), &u.ID, nil)
	again, _ := f.st.GetUserByID(context.Background(), u.ID)
	if len(toks) != 1 || !toks[0].ExpiresAt.Equal(*tok.ExpiresAt) || again.LastSeenAt == nil || !again.LastSeenAt.Equal(*u.LastSeenAt) {
		f.t.Errorf("a success moved the token's expiry or the user's activity: %+v, %+v", toks, again)
	}
}

// TestRefreshErrorEndsNothing: every other failure ends nothing: the token stays live, the row
// is due again after 1, 2, 4 … minutes up to the interval, the error is logged by its kind, and
// a success clears the count. An ID token that names another identity is such an error.
func TestRefreshErrorEndsNothing(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	_, tok := f.login("alice", "a", nil, refreshSecret+"-a")
	f.login("bob", "b", nil, refreshSecret+"-b")
	down := &oidc.RefreshError{Kind: oidc.RefreshUnavailable}
	f.idp.set(func(string) (*oidc.Refreshed, error) { return nil, down })
	r := f.refresher("r1")
	f.advance(refreshInterval)
	for i, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, refreshInterval, refreshInterval} {
		r.round(ctx)
		row := f.rowOf(tok.ID)
		if row == nil || row.Failures != i+1 || !f.live(tok.ID) {
			t.Fatalf("after error %d: %+v", i+1, row)
		}
		due(t, fmt.Sprintf("after error %d", i+1), row, f.clock(), wait)
		f.advance(wait)
	}
	if evs := f.audit(auth.ActionTokenRevoke); len(evs) != 0 || !strings.Contains(f.logs.String(), "kind=unavailable") {
		t.Errorf("an error revoked %+v, or was not logged by its kind", evs)
	}
	f.idp.set(func(string) (*oidc.Refreshed, error) {
		return &oidc.Refreshed{Identity: &oidc.Identity{Issuer: testIssuer, Subject: "b"}}, nil
	})
	r.round(ctx)
	if row := f.rowOf(tok.ID); row == nil || row.Failures != 7 || !strings.Contains(f.logs.String(), "kind=identity") {
		t.Errorf("an ID token of another identity: %+v", row)
	}
	f.advance(refreshInterval)
	f.idp.set(func(string) (*oidc.Refreshed, error) { return &oidc.Refreshed{}, nil })
	r.round(ctx)
	if row := f.rowOf(tok.ID); row == nil || row.Failures != 0 {
		t.Errorf("a success after errors: %+v", row)
	}
}

// TestRefreshDropped: a due row whose token is revoked or expired, or that has none 10 minutes
// after its login, is deleted without a call.
func TestRefreshDropped(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	_, revoked := f.login("alice", "a", nil, refreshSecret+"-a")
	if err := f.tokens.Revoke(ctx, revoked.ID, auth.Origin{Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	id, _ := f.st.GetIdentity(ctx, testIssuer, "a")
	orphan := index.OIDCRefresh{LoginID: ptrTo(int64(999)), IdentityID: id.ID, Sealed: []byte("s"), NextAt: f.clock().Add(oidc.LoginTTL)}
	if err := f.st.CreateOIDCRefresh(ctx, &orphan); err != nil {
		t.Fatal(err)
	}
	_, expiring := f.login("bob", "b", nil, refreshSecret+"-b")
	f.advance(refreshInterval)
	r := f.refresher("r1")
	r.round(ctx)
	if f.idp.total() != 1 || len(f.rows()) != 1 || f.rowOf(expiring.ID) == nil {
		t.Errorf("after the round: calls %v, rows %+v; want bob's only", f.idp.calls, f.rows())
	}
	f.advance(24 * time.Hour)
	r.round(ctx)
	if f.idp.total() != 1 || len(f.rows()) != 0 {
		t.Errorf("an expired login: calls %v, rows %+v", f.idp.calls, f.rows())
	}
}

// TestRefreshClaims: two replicas that refresh at once ask the IdP once per login, each at most
// refreshCalls at once; a replica that seals nothing claims none.
func TestRefreshClaims(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	for i := range 12 {
		f.login(fmt.Sprintf("user%d", i), fmt.Sprintf("s%d", i), nil, fmt.Sprintf("%s-%d", refreshSecret, i))
	}
	held, _ := f.m.Sealing()
	f.m.SealWith("0123456789abcdef")
	f.advance(refreshInterval)
	f.refresher("r0").round(ctx)
	if f.idp.total() != 0 {
		t.Fatalf("a replica that seals nothing refreshed: %v", f.idp.calls)
	}
	f.m.SealWith(held)
	f.idp.delay = 20 * time.Millisecond
	var wg sync.WaitGroup
	for _, replica := range []string{"r1", "r2"} {
		wg.Go(func() { f.refresher(replica).round(ctx) })
	}
	wg.Wait()
	if len(f.idp.calls) != 12 || f.idp.total() != 12 || f.idp.most > 2*refreshCalls {
		t.Errorf("the calls: %v, at most %d at once; want each login once", f.idp.calls, f.idp.most)
	}
	single := newRefreshFixture(t)
	for i := range 9 {
		single.login(fmt.Sprintf("user%d", i), fmt.Sprintf("s%d", i), nil, fmt.Sprintf("%s-%d", refreshSecret, i))
	}
	single.idp.delay = 20 * time.Millisecond
	single.advance(refreshInterval)
	single.refresher("r1").round(ctx)
	if single.idp.total() != 9 || single.idp.most > refreshCalls {
		t.Errorf("one replica: %d calls, at most %d at once; want 9, at most %d", single.idp.total(), single.idp.most, refreshCalls)
	}
}

// TestStartRefreshOff: with auth.oidc.refresh.interval 0, serve deletes every row at its start.
func TestStartRefreshOff(t *testing.T) {
	f := newRefreshFixture(t)
	f.login("alice", "a", nil, refreshSecret)
	var wg sync.WaitGroup
	startRefresh(context.Background(), &config.Config{}, f.st, nil, f.m, "r1", &wg, f.log)
	wg.Wait()
	if rows := f.rows(); len(rows) != 0 || !strings.Contains(f.logs.String(), "count=1") {
		t.Errorf("the rows after a start with the setting off: %+v; %s", rows, f.logs)
	}
}

func ptrTo[T any](v T) *T { return &v }

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// lockoutMsg is the message of every refused password with a lockout of 30 minutes.
const lockoutMsg = "the user name or the password was refused: 10 wrong passwords in a row lock a password for 30m"

// lockoutFixture is an Authenticator with auth.lockout.duration 30m over a SQLite index with the
// users admin and alice, a clock the test moves, and the hashes each verification was made
// against.
type lockoutFixture struct {
	st   *sqlite.Store
	a    *Authenticator
	logs interface{ String() string }
	now  time.Time
	ctx  context.Context // of the requests: WithRequest

	mu       sync.Mutex
	verified []string
}

func newLockoutFixture(t *testing.T, o Options) *lockoutFixture {
	t.Helper()
	users, st, logs := newUsers(t)
	createAdminAndAlice(t, users)
	f := &lockoutFixture{st: st, logs: logs, now: usersNow,
		ctx: WithRequest(t.Context(), Request{ID: "req-1", Address: "192.0.2.7"})}
	if o.Index == nil {
		o.Index = st
	}
	o.Now, o.Log = func() time.Time { return f.now }, users.Log
	f.a = NewAuthenticator(o)
	f.a.verify = func(hash, password string) (bool, error) {
		f.mu.Lock()
		f.verified = append(f.verified, hash)
		f.mu.Unlock()
		return VerifyPassword(hash, password)
	}
	return f
}

// user returns the row of the user called name.
func (f *lockoutFixture) user(t *testing.T, name string) *index.User {
	t.Helper()
	u, err := f.st.GetUser(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// lastVerified returns the hash of the last verification.
func (f *lockoutFixture) lastVerified() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.verified[len(f.verified)-1]
}

// refused checks that user and secret are refused in Basic credentials with the message of the
// lockout.
func (f *lockoutFixture) refused(t *testing.T, user, secret string) {
	t.Helper()
	_, _, err := f.a.Authenticate(f.ctx, user, secret)
	if !errors.Is(err, ErrPasswordRefused) || f.a.RefusalMessage(err, "fallback") != lockoutMsg {
		t.Fatalf("Authenticate(%s) = %v, message %q; want a refusal saying what locks a password", user, err,
			f.a.RefusalMessage(err, "fallback"))
	}
}

// lockoutRows returns the login.block rows of the lockout.
func (f *lockoutFixture) lockoutRows(t *testing.T) []map[string]any {
	t.Helper()
	evs, err := f.st.ListAudit(t.Context(), index.AuditFilter{Action: ActionLoginBlock})
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, ev := range evs {
		var d map[string]any
		if err := json.Unmarshal(ev.Details, &d); err != nil {
			t.Fatal(err)
		}
		if d["limit"] == LimitLockout {
			d["subject"], d["ip"], d["principal"] = ev.Subject, ev.IP, ev.Principal
			rows = append(rows, d)
		}
	}
	return rows
}

// lockAlice locks alice's password with 10 wrong passwords at usersNow, and returns her row and
// the end of the lock.
func (f *lockoutFixture) lockAlice(t *testing.T) (*index.User, time.Time) {
	t.Helper()
	for range LockoutFailures {
		f.refused(t, "alice", "a wrong password")
	}
	return f.user(t, "alice"), usersNow.Add(30 * time.Minute)
}

// TestLockout: with auth.lockout.duration, wrong passwords are counted, and the 10th in a row
// locks the password for the duration, with one login.block row of the limit lockout; neither
// moves the user's revision (docs/spec/rbac.md §1, Local passwords; §5).
func TestLockout(t *testing.T) {
	f := newLockoutFixture(t, Options{Lockout: 30 * time.Minute})
	revision := f.user(t, "alice").Revision
	for range LockoutFailures - 1 {
		f.refused(t, "alice", "a wrong password")
	}
	if u := f.user(t, "alice"); u.PasswordFailures != LockoutFailures-1 || u.LockedUntil != nil {
		t.Fatalf("after 9 wrong passwords: %d failures, locked until %v", u.PasswordFailures, u.LockedUntil)
	}
	f.refused(t, "alice", "a wrong password")
	alice := f.user(t, "alice")
	until := usersNow.Add(30 * time.Minute)
	if alice.PasswordFailures != 0 || alice.LockedUntil == nil || !alice.LockedUntil.Equal(until) || alice.Revision != revision {
		t.Fatalf("after 10: %d failures, locked until %v, revision %d; want 0, %v, %d", alice.PasswordFailures, alice.LockedUntil,
			alice.Revision, until, revision)
	}
	rows := f.lockoutRows(t)
	want := map[string]any{"limit": "lockout", "credential": "password", "failures": float64(10), "until": until.Format(time.RFC3339),
		"requestId": "req-1", "subject": UserPrincipal(alice).String(), "ip": "192.0.2.7", "principal": "anonymous"}
	if len(rows) != 1 || !jsonEqual(rows[0], want) {
		t.Errorf("the lockout's login.block rows: %v, want one %v", rows, want)
	}
	if !strings.Contains(f.logs.String(), `"msg":"password locked"`) {
		t.Errorf("no line of the lock:\n%s", f.logs)
	}
}

// TestLockoutLocked: while locked, the right password is refused as a wrong one is, after one
// verification against the dummy hash, and a wrong one neither lengthens the lock nor counts; the
// user's tokens keep working; the lock ends by itself, and a verified password sets the count
// back to 0.
func TestLockoutLocked(t *testing.T) {
	f := newLockoutFixture(t, Options{Lockout: 30 * time.Minute})
	alice, until := f.lockAlice(t)
	f.now = usersNow.Add(2 * time.Minute) // past the per-replica throttle
	for _, secret := range []string{"alice-password", "a wrong password"} {
		f.refused(t, "alice", secret)
		if f.lastVerified() != dummyHash() {
			t.Errorf("a locked password is verified against %q, want the dummy hash", f.lastVerified())
		}
	}
	if _, _, err := f.a.AuthenticatePassword(f.ctx, "alice", "alice-password"); !errors.Is(err, ErrPasswordRefused) {
		t.Errorf("AuthenticatePassword of a locked password = %v, want a refusal", err)
	}
	if u := f.user(t, "alice"); u.PasswordFailures != 0 || !u.LockedUntil.Equal(until) || len(f.lockoutRows(t)) != 1 {
		t.Errorf("refusals while locked counted: %d failures, locked until %v", u.PasswordFailures, u.LockedUntil)
	}
	if !strings.Contains(f.logs.String(), `"reason":"locked"`) {
		t.Errorf("no refusal line with the reason locked:\n%s", f.logs)
	}
	secret := newSecret(PATPrefix)
	exp := f.now.Add(time.Hour)
	if err := f.st.CreateToken(f.ctx, &index.Token{Kind: TokenPAT, UserID: &alice.ID, Name: "t", Hash: HashSecret(secret), ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	if p, _, err := f.a.Authenticate(f.ctx, "alice", secret); err != nil || p != UserPrincipal(alice) {
		t.Errorf("alice's token while her password is locked = %v, %v", p, err)
	}

	f.now = until
	f.refused(t, "alice", "a wrong password")
	if u := f.user(t, "alice"); u.PasswordFailures != 1 {
		t.Errorf("a wrong password once the lock ended: %d failures, want 1", u.PasswordFailures)
	}
	if _, _, err := f.a.Authenticate(f.ctx, "alice", "alice-password"); err != nil {
		t.Fatalf("the right password once the lock ended = %v", err)
	}
	if u := f.user(t, "alice"); u.PasswordFailures != 0 || u.Revision != alice.Revision {
		t.Errorf("after a verified password: %d failures, revision %d; want 0, %d", u.PasswordFailures, u.Revision, alice.Revision)
	}
}

// TestLockoutCache: a remembered verification does not get past a lock, and a remembered
// password sets a count that is not 0 back to 0.
func TestLockoutCache(t *testing.T) {
	f := newLockoutFixture(t, Options{Lockout: 30 * time.Minute})
	ctx := f.ctx
	if _, _, err := f.a.Authenticate(ctx, "alice", "alice-password"); err != nil {
		t.Fatal(err)
	}
	f.refused(t, "alice", "a wrong password")
	if _, _, err := f.a.Authenticate(ctx, "alice", "alice-password"); err != nil || f.user(t, "alice").PasswordFailures != 0 {
		t.Fatalf("the remembered password = %v, %d failures; want it, and the count back to 0", err, f.user(t, "alice").PasswordFailures)
	}
	f.now = usersNow.Add(2 * time.Minute) // past the per-replica throttle
	for range LockoutFailures {
		f.refused(t, "alice", "a wrong password")
	}
	f.now = usersNow.Add(4 * time.Minute)
	f.refused(t, "alice", "alice-password")
}

// staleUsers reads the users without their lock, as a replica that read a user just before
// another replica locked it.
type staleUsers struct{ index.Store }

func (s staleUsers) GetUser(ctx context.Context, name string) (*index.User, error) {
	u, err := s.Store.GetUser(ctx, name)
	if u != nil {
		u.LockedUntil = nil
	}
	return u, err
}

// TestLockoutRace: a password verified while another replica locked the user is refused as a
// locked one's, not answered as a success.
func TestLockoutRace(t *testing.T) {
	f := newLockoutFixture(t, Options{Lockout: 30 * time.Minute})
	ctx := t.Context()
	alice := f.user(t, "alice")
	if _, err := f.st.FailPassword(ctx, alice.ID, usersNow, usersNow.Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	stale := NewAuthenticator(Options{Index: staleUsers{f.st}, Lockout: 30 * time.Minute, Now: func() time.Time { return usersNow }})
	if _, _, err := stale.Authenticate(ctx, "alice", "alice-password"); !errors.Is(err, ErrPasswordRefused) {
		t.Errorf("a password verified while another replica locked it = %v, want a refusal", err)
	}
	if _, _, err := stale.AuthenticatePassword(ctx, "alice", "alice-password"); !errors.Is(err, ErrPasswordRefused) {
		t.Errorf("AuthenticatePassword of it = %v, want a refusal", err)
	}
}

// TestLockoutUncounted: names that no user has, disabled users and users without a password are
// not counted in the index; every refusal has the same message, and a token's the caller's.
func TestLockoutUncounted(t *testing.T) {
	f := newLockoutFixture(t, Options{Lockout: 30 * time.Minute})
	ctx := t.Context()
	users := &Users{Index: f.st, Now: func() time.Time { return usersNow }}
	if err := users.Disable(ctx, "alice", cli); err != nil {
		t.Fatal(err)
	}
	if _, err := users.CreateService(ctx, "robot", "group:ops", cli); err != nil {
		t.Fatal(err)
	}
	for range 2 * LockoutFailures {
		for _, name := range []string{"alice", "robot", "nobody", "Not A Name"} {
			f.refused(t, name, "a wrong password")
		}
		f.now = f.now.Add(time.Minute) // past the per-replica throttle
	}
	for _, name := range []string{"alice", "robot"} {
		if u := f.user(t, name); u.PasswordFailures != 0 || u.LockedUntil != nil {
			t.Errorf("%s: %d failures, locked until %v; want none counted", name, u.PasswordFailures, u.LockedUntil)
		}
	}
	if rows := f.lockoutRows(t); len(rows) != 0 {
		t.Errorf("lockout rows %v, want none", rows)
	}
	_, _, err := f.a.Authenticate(ctx, "admin", PATPrefix+"unknown")
	if got := f.a.RefusalMessage(err, "fallback"); !errors.Is(err, ErrBadCredentials) || got != "fallback" {
		t.Errorf("a token refused = %v, message %q; want the caller's", err, got)
	}
}

// TestLockoutOff: with auth.lockout.duration 0 nothing is counted and nothing locks; while local
// passwords are off the lockout does nothing, and a password's message says they are off.
func TestLockoutOff(t *testing.T) {
	f := newLockoutFixture(t, Options{})
	ctx := t.Context()
	for range 2 * LockoutFailures {
		_, _, err := f.a.Authenticate(ctx, "alice", "a wrong password")
		if !errors.Is(err, ErrBadCredentials) || errors.Is(err, ErrPasswordRefused) || f.a.RefusalMessage(err, "fallback") != "fallback" {
			t.Fatalf("a wrong password without the lockout = %v", err)
		}
		f.now = f.now.Add(10 * time.Second)
	}
	if u := f.user(t, "alice"); u.PasswordFailures != 0 || u.LockedUntil != nil || f.a.LockoutMessage() != "" {
		t.Errorf("without the lockout: %d failures, locked until %v, message %q", u.PasswordFailures, u.LockedUntil, f.a.LockoutMessage())
	}
	off := newLockoutFixture(t, Options{Lockout: 30 * time.Minute, PasswordsOff: true})
	_, _, err := off.a.Authenticate(ctx, "alice", "a wrong password")
	if !errors.Is(err, ErrPasswordsOff) || off.a.RefusalMessage(err, "") != MsgPasswordsOff || off.a.LockoutMessage() != "" {
		t.Errorf("passwords off with a lockout = %v, %q", err, off.a.LockoutMessage())
	}
}

// jsonEqual reports whether a and b have the same keys and values.
func jsonEqual(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

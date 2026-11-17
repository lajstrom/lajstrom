package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// passwordFixture is a lockoutFixture whose authenticator has its Users, over the same index and
// clock, for the user's own change (docs/spec/rbac.md §1, Local passwords).
type passwordFixture struct {
	*lockoutFixture
	users *Users
}

func newPasswordFixture(t *testing.T, o Options) *passwordFixture {
	t.Helper()
	f := newLockoutFixture(t, o)
	f.a.users = &Users{Index: f.st, Now: func() time.Time { return f.now }, Log: f.a.log}
	return &passwordFixture{f, f.a.users}
}

// mustChange sets the mustChange of the user called name, as an admin's PATCH does.
func (f *passwordFixture) mustChange(t *testing.T, name string) {
	t.Helper()
	yes := true
	if _, err := f.users.Change(context.Background(), name, UserChange{MustChange: &yes}, cli); err != nil {
		t.Fatal(err)
	}
}

// change is alice's own change from current to password.
func (f *passwordFixture) change(current, password string) error {
	return f.a.ChangeOwnPassword(f.ctx, "alice", current, password, Origin{Source: "api"})
}

// TestMustChangeRefused: under mustChange a user's password is ErrMustChange, which is no
// refusal of credentials (403), once it has verified, on every path that takes a password: Basic
// credentials, verified and then remembered, and npm login and POST /api/v1/login
// (AuthenticatePassword); a wrong password is a refusal as always, and counts; the user's tokens
// keep working (docs/spec/rbac.md §1, Change at first use).
func TestMustChangeRefused(t *testing.T) {
	f := newPasswordFixture(t, Options{Lockout: 30 * time.Minute})
	_, pat, err := (&Tokens{Index: f.st, Now: func() time.Time { return f.now }}).CreatePAT(context.Background(), "alice", "laptop", nil,
		Expiry{}, cli)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.a.Authenticate(f.ctx, "alice", "alice-password"); err != nil {
		t.Fatalf("alice before mustChange: %v", err) // and remembered
	}
	f.mustChange(t, "alice")
	for i := range 2 {
		if _, _, err := f.a.Authenticate(f.ctx, "alice", "alice-password"); !errors.Is(err, ErrMustChange) || errors.Is(err, ErrBadCredentials) {
			t.Errorf("Authenticate %d with the right password = %v, want ErrMustChange", i, err)
		}
	}
	if _, _, err := f.a.AuthenticatePassword(f.ctx, "alice", "alice-password"); !errors.Is(err, ErrMustChange) {
		t.Errorf("AuthenticatePassword with the right password = %v, want ErrMustChange", err)
	}
	f.refused(t, "alice", "wrong-password")
	f.a.counting.Wait()
	if u := f.user(t, "alice"); u.PasswordFailures != 1 {
		t.Errorf("failures after a wrong password = %d, want 1: a right one under mustChange is none", u.PasswordFailures)
	}
	if !strings.Contains(f.logs.String(), `"reason":"must change"`) {
		t.Errorf("no refusal line with the reason must change:\n%s", f.logs.String())
	}
	if p, _, err := f.a.Authenticate(f.ctx, "alice", pat); err != nil || p != UserPrincipal(f.user(t, "alice")) {
		t.Errorf("alice's personal access token under mustChange = %v, %v", p, err)
	}
	if _, _, err := f.a.Authenticate(f.ctx, "admin", "admin-password"); err != nil {
		t.Errorf("admin, who need not change, = %v", err)
	}
}

// TestChangeOwnPassword: a user changes its password with its current one, under mustChange
// too, which the change clears: the current hash goes into the history, the user.password row is
// by the user, and the new password works and the old one does not (docs/spec/rbac.md §1, A
// user's own password; §5).
func TestChangeOwnPassword(t *testing.T) {
	f := newPasswordFixture(t, Options{})
	f.mustChange(t, "alice")
	before := f.user(t, "alice")
	if err := f.change("alice-password", "alice-password-2"); err != nil {
		t.Fatal(err)
	}
	after := f.user(t, "alice")
	if after.MustChange || after.PasswordHash == before.PasswordHash || len(after.PasswordHistory) != 1 ||
		after.PasswordHistory[0] != before.PasswordHash {
		t.Errorf("alice after her change: mustChange %v, history %q; want cleared, her old hash", after.MustChange, after.PasswordHistory)
	}
	if _, _, err := f.a.Authenticate(f.ctx, "alice", "alice-password-2"); err != nil {
		t.Errorf("the new password = %v", err)
	}
	if _, _, err := f.a.Authenticate(f.ctx, "alice", "alice-password"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("the old password = %v, want ErrBadCredentials", err)
	}
	wantAudit(t, f.st,
		`user:alice user.password user:alice {"name":"alice","source":"api"}`,
		`anonymous user.mustchange user:alice {"mustChange":true,"name":"alice","source":"cli"}`,
		`anonymous user.create user:alice {"admin":false,"name":"alice","source":"cli"}`,
		`anonymous user.create user:admin {"admin":true,"name":"admin","source":"cli"}`)
}

// TestChangeOwnPasswordLastFour: the user's own change refuses its last four passwords, the
// current one and the three before it, an admin's among them, and takes the fifth; nothing is
// written for a refused one (docs/spec/rbac.md §1, A user's own password).
func TestChangeOwnPasswordLastFour(t *testing.T) {
	f := newPasswordFixture(t, Options{})
	used := func(current, password string) {
		t.Helper()
		revision := f.user(t, "alice").Revision
		if err := f.change(current, password); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "password: was used before") {
			t.Errorf("a change to %q = %v, want ErrInvalid saying it was used before", password, err)
		}
		if f.user(t, "alice").Revision != revision {
			t.Errorf("a refused change to %q changed alice", password)
		}
	}
	used("alice-password", "alice-password")
	if err := f.users.SetPassword(context.Background(), "alice", "set-by-admin", cli); err != nil {
		t.Fatal(err)
	}
	used("set-by-admin", "alice-password")
	used("set-by-admin", "set-by-admin")
	for i, password := range []string{"second-pass", "third-pass", "fourth-pass"} {
		current := []string{"set-by-admin", "second-pass", "third-pass"}[i]
		if err := f.change(current, password); err != nil {
			t.Fatalf("a change to %q: %v", password, err)
		}
	}
	// the last four: fourth-pass, third-pass, second-pass, set-by-admin
	for _, password := range []string{"fourth-pass", "third-pass", "second-pass", "set-by-admin"} {
		used("fourth-pass", password)
	}
	if err := f.change("fourth-pass", "alice-password"); err != nil {
		t.Errorf("a change to the fifth password back = %v, want it taken", err)
	}
}

// TestChangeOwnPasswordRefused: the current password is the request's credential: a wrong one,
// a name that no user has, a user without a password and a token are refusals, after the same
// work, and a wrong one counts toward the lockout; a locked password is refused, the right one
// too; a new password that breaks a rule is ErrInvalid; while local passwords are off nothing is
// checked (ErrInvalid naming the setting). None writes anything.
func TestChangeOwnPasswordRefused(t *testing.T) {
	f := newPasswordFixture(t, Options{Lockout: 30 * time.Minute})
	if _, err := f.users.CreateService(context.Background(), "robot", "user:admin", cli); err != nil {
		t.Fatal(err)
	}
	before := f.user(t, "alice")
	for _, tt := range []struct{ name, user, current string }{
		{"a wrong password", "alice", "wrong-password"},
		{"a name that no user has", "nobody", "alice-password"},
		{"a service account", "robot", "alice-password"},
		{"a token", "alice", PATPrefix + "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"},
		{"a malformed name", "Alice", "alice-password"},
	} {
		err := f.a.ChangeOwnPassword(f.ctx, tt.user, tt.current, "a new password", Origin{Source: "api"})
		if !errors.Is(err, ErrPasswordRefused) || f.a.RefusalMessage(err, "fallback") != lockoutMsg {
			t.Errorf("%s: %v, want a refusal with the message of every refused password", tt.name, err)
		}
	}
	f.a.counting.Wait()
	if u := f.user(t, "alice"); u.PasswordFailures != 1 || u.Revision != before.Revision {
		t.Errorf("alice after the refusals: failures %d, revision %d; want 1, %d", u.PasswordFailures, u.Revision, before.Revision)
	}
	f.users.PasswordPolicy = PasswordPolicy{MinLength: 12}
	if err := f.change("alice-password", "short-pw-1"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "auth.passwords.minLength") {
		t.Errorf("a change to a short password = %v, want ErrInvalid naming the rule", err)
	}
	lock(t, f.st, "alice", f.now.Add(time.Hour))
	if err := f.change("alice-password", "a new password 1"); !errors.Is(err, ErrPasswordRefused) {
		t.Errorf("a change of a locked password = %v, want it refused", err)
	}
	f.users.PasswordsOff = true
	if err := f.change("alice-password", "a new password 1"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "auth.localPasswords: false") {
		t.Errorf("a change while passwords are off = %v, want ErrInvalid naming the setting", err)
	}
	if u := f.user(t, "alice"); u.PasswordHash != before.PasswordHash {
		t.Error("a refused change changed alice's password")
	}
	wantAudit(t, f.st,
		`anonymous user.create user:robot {"kind":"service","name":"robot","owner":"user:admin","source":"cli"}`,
		`anonymous user.create user:alice {"admin":false,"name":"alice","source":"cli"}`,
		`anonymous user.create user:admin {"admin":true,"name":"admin","source":"cli"}`)
}

// TestChangeOwnPasswordChangedMeanwhile: a password changed after the current one verified, by an
// admin or another change, fails the change, which writes nothing.
func TestChangeOwnPasswordChangedMeanwhile(t *testing.T) {
	f := newPasswordFixture(t, Options{})
	verified := f.user(t, "alice")
	if err := f.users.SetPassword(context.Background(), "alice", "set-by-admin", cli); err != nil {
		t.Fatal(err)
	}
	if err := f.users.changeOwn(f.ctx, verified, "a new password", cli); !errors.Is(err, errPasswordChanged) {
		t.Errorf("a change after the admin's = %v, want errPasswordChanged", err)
	}
	if u := f.user(t, "alice"); len(u.PasswordHistory) != 1 {
		t.Errorf("alice's history = %q, want the admin's change only", u.PasswordHistory)
	}
	if err := (&Authenticator{}).ChangeOwnPassword(f.ctx, "alice", "set-by-admin", "a new password", cli); err == nil {
		t.Error("ChangeOwnPassword without Users = nil")
	}
}

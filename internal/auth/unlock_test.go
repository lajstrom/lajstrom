package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// lock locks the password of the user called name until until, as its 10th wrong password does.
func lock(t *testing.T, st index.Store, name string, until time.Time) {
	t.Helper()
	u, err := st.GetUser(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if locked, err := st.FailPassword(context.Background(), u.ID, usersNow, until, 1); err != nil || !locked {
		t.Fatalf("FailPassword = %v, %v", locked, err)
	}
}

// TestUnlock: an unlock ends the lock of a user and writes user.unlock with the end of the lock
// it ended; unlocking a user that is not locked writes nothing; a user that does not exist is
// ErrNotFound; a locked user is answered with lockedUntil while the lockout is set, and without
// it otherwise (docs/spec/rbac.md §1, Local passwords; §5).
func TestUnlock(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	createAdminAndAlice(t, users)
	until := usersNow.Add(30 * time.Minute)
	lock(t, st, "alice", until)
	if u, err := users.Get(ctx, "alice"); err != nil || u.LockedUntil != nil {
		t.Errorf("without the lockout, alice = %+v, %v; want no lockedUntil", u, err)
	}
	users.Lockout = 30 * time.Minute
	if u, err := users.Get(ctx, "alice"); err != nil || u.LockedUntil == nil || !u.LockedUntil.Equal(until) {
		t.Errorf("alice = %+v, %v; want lockedUntil %v", u, err, until)
	}
	revision := mustGet(t, users, "alice").Revision
	if err := users.Unlock(ctx, "alice", cli); err != nil {
		t.Fatal(err)
	}
	u := mustGet(t, users, "alice")
	if u.LockedUntil != nil || u.Revision != revision {
		t.Errorf("after the unlock: lockedUntil %v, revision %d; want none, %d", u.LockedUntil, u.Revision, revision)
	}
	if err := users.Unlock(ctx, "alice", cli); err != nil {
		t.Errorf("an unlock of a user not locked = %v", err)
	}
	wantAudit(t, st,
		`anonymous user.unlock user:alice {"lockedUntil":"2026-11-07T12:30:00Z","name":"alice","source":"cli"}`,
		`anonymous user.create user:alice {"admin":false,"name":"alice","source":"cli"}`,
		`anonymous user.create user:admin {"admin":true,"name":"admin","source":"cli"}`)
	if err := users.Unlock(ctx, "nobody", cli); !errors.Is(err, index.ErrNotFound) || err.Error() != "user nobody does not exist" {
		t.Errorf("an unlock of nobody = %v, want ErrNotFound", err)
	}
}

// TestSetPasswordUnlocks: an admin's new password ends a lock, with user.unlock beside
// user.password; a new password of a user not locked writes user.password only.
func TestSetPasswordUnlocks(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	createAdminAndAlice(t, users)
	users.Lockout = 30 * time.Minute
	lock(t, st, "alice", usersNow.Add(time.Hour))
	if err := users.SetPassword(ctx, "alice", "a new password", cli); err != nil {
		t.Fatal(err)
	}
	if err := users.SetPassword(ctx, "alice", "another password", cli); err != nil {
		t.Fatal(err)
	}
	if u := mustGet(t, users, "alice"); u.LockedUntil != nil {
		t.Errorf("after a new password: lockedUntil %v", u.LockedUntil)
	}
	wantAudit(t, st,
		`anonymous user.password user:alice {"name":"alice","source":"cli"}`,
		`anonymous user.unlock user:alice {"lockedUntil":"2026-11-07T13:00:00Z","name":"alice","source":"cli"}`,
		`anonymous user.password user:alice {"name":"alice","source":"cli"}`,
		`anonymous user.create user:alice {"admin":false,"name":"alice","source":"cli"}`,
		`anonymous user.create user:admin {"admin":true,"name":"admin","source":"cli"}`)
}

// mustGet returns the user called name.
func mustGet(t *testing.T, users *Users, name string) *User {
	t.Helper()
	u, err := users.Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

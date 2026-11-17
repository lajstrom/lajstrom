package storetest

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The lockout (#1280; docs/spec/rbac.md §1, Local passwords; docs/spec/index-schema.md, The
// lockout): users.password_failures and users.locked_until, which FailPassword,
// ResetPasswordFailures and UnlockUser write, and nothing else.

// lockLimit is the limit of the tests, the constant of rbac.md §1.
const lockLimit = 10

// lockoutUser is a user of the tests of the lockout, with the checks they make of it.
type lockoutUser struct {
	t *testing.T
	s index.Store
	u index.User
}

// state checks the count and the lock of the user, and that its revision has not moved.
func (l lockoutUser) state(what string, failures int, until *time.Time) {
	l.t.Helper()
	got, err := l.s.GetUserByID(l.t.Context(), l.u.ID)
	must(l.t, err)
	if got.PasswordFailures != failures || !sameTime(got.LockedUntil, until) {
		l.t.Errorf("%s: PasswordFailures %d, LockedUntil %v; want %d, %v", what, got.PasswordFailures, got.LockedUntil, failures, until)
	}
	if got.Revision != l.u.Revision {
		l.t.Errorf("%s: Revision = %d, want %d: the lockout is no change of the user", what, got.Revision, l.u.Revision)
	}
}

// fail counts a wrong password at at(sec), with until the end of a lock, and checks whether it
// locked.
func (l lockoutUser) fail(what string, sec int, until time.Time, want bool) {
	l.t.Helper()
	locked, err := l.s.FailPassword(l.t.Context(), l.u.ID, at(sec), until, lockLimit)
	if err != nil || locked != want {
		l.t.Errorf("FailPassword %s = %v, %v; want %v", what, locked, err, want)
	}
}

// lock counts lockLimit wrong passwords from at(sec), the last of which locks until until.
func (l lockoutUser) lock(what string, sec int, until time.Time) {
	l.t.Helper()
	for i := 1; i <= lockLimit; i++ {
		l.fail(what+" "+strconv.Itoa(i), sec+i, until, i == lockLimit)
	}
}

// unlock unlocks the user at at(sec) and checks the lock it ended.
func (l lockoutUser) unlock(what string, sec int, want *time.Time) {
	l.t.Helper()
	ended, err := l.s.UnlockUser(l.t.Context(), l.u.ID, at(sec))
	if err != nil || !sameTime(ended, want) {
		l.t.Errorf("UnlockUser %s = %v, %v; want %v", what, ended, err, want)
	}
	l.state(what, 0, nil)
}

// testUsersLockout: FailPassword counts wrong passwords in a row and the one that reaches the
// limit locks; a lock in force counts nothing and refuses a reset; a lock that has ended counts
// again from 0; UnlockUser ends a lock and says which; none of the three moves the revision or
// auth_version.
func testUsersLockout(t *testing.T, s index.Store) {
	ctx := t.Context()
	bob := lockoutUser{t, s, newUser(t, s, "bob")}
	version, err := s.AuthVersion(ctx)
	must(t, err)
	bob.state("a new user", 0, nil)

	for i := 1; i < lockLimit; i++ {
		bob.fail("failure "+strconv.Itoa(i), i, at(1000), false)
	}
	bob.state("9 failures", lockLimit-1, nil)
	must(t, s.ResetPasswordFailures(ctx, bob.u.ID, at(20)))
	bob.state("a verified password", 0, nil)
	must(t, s.ResetPasswordFailures(ctx, bob.u.ID, at(21)))
	bob.state("a verified password, with nothing counted", 0, nil)

	bob.lock("failure", 30, at(1000))
	bob.state("locked", 0, ptr(at(1000)))
	bob.fail("while locked", 50, at(2000), false)
	bob.state("a failure while locked", 0, ptr(at(1000)))
	wantErr(t, "ResetPasswordFailures while locked", s.ResetPasswordFailures(ctx, bob.u.ID, at(999)), index.ErrConflict)
	bob.state("a reset refused", 0, ptr(at(1000)))

	// The lock ends at locked_until: the count starts again, and locked_until stays the end of
	// the last lock.
	bob.fail("when the lock ends", 1000, at(3000), false)
	bob.state("a failure after the lock", 1, ptr(at(1000)))
	must(t, s.ResetPasswordFailures(ctx, bob.u.ID, at(1001)))
	bob.state("a verified password after the lock", 0, ptr(at(1000)))
	bob.unlock("of a lock that has ended", 1002, nil)

	bob.lock("failure of the second lock", 1100, at(5000))
	bob.fail("one more", 1200, at(6000), false)
	bob.unlock("while locked", 1300, ptr(at(5000)))
	bob.fail("after the unlock", 1301, at(7000), false)
	bob.state("a failure after the unlock", 1, nil)
	bob.unlock("of a user not locked", 1302, nil)
	if v, err := s.AuthVersion(ctx); err != nil || v != version {
		t.Errorf("AuthVersion after the lockout = %d, %v, want %d", v, err, version)
	}

	_, err = s.FailPassword(ctx, 999999, at(1), at(2), lockLimit)
	wantErr(t, "FailPassword missing", err, index.ErrNotFound)
	wantErr(t, "ResetPasswordFailures missing", s.ResetPasswordFailures(ctx, 999999, at(1)), index.ErrNotFound)
	_, err = s.UnlockUser(ctx, 999999, at(1))
	wantErr(t, "UnlockUser missing", err, index.ErrNotFound)
}

// testUsersLockoutColumns: every read of a user answers the columns of the lockout, CreateUser
// and UpdateUser leave them whatever the struct holds, and the three methods work in a
// transaction, as the lock and its audit row are written.
func testUsersLockoutColumns(t *testing.T, s index.Store) {
	ctx := t.Context()
	bob := lockoutUser{t, s, newUser(t, s, "bob")}
	bob.lock("failure", 0, at(8000))
	bob.u.Name, bob.u.PasswordFailures, bob.u.LockedUntil = "robert", 3, nil
	must(t, s.UpdateUser(ctx, &bob.u))
	byName, err := s.GetUser(ctx, "robert")
	must(t, err)
	byUID, err := s.GetUserByUID(ctx, bob.u.UID)
	must(t, err)
	all, err := s.ListUsers(ctx)
	must(t, err)
	for what, u := range map[string]*index.User{"GetUser": byName, "GetUserByUID": byUID, "ListUsers": &all[0]} {
		if u.PasswordFailures != 0 || !sameTime(u.LockedUntil, ptr(at(8000))) {
			t.Errorf("%s after UpdateUser: PasswordFailures %d, LockedUntil %v; want 0, %v", what, u.PasswordFailures, u.LockedUntil, at(8000))
		}
	}

	carol := index.User{Name: "carol", Kind: "local", PasswordFailures: 4, LockedUntil: ptr(at(9000))}
	must(t, s.CreateUser(ctx, &carol))
	if got, err := s.GetUser(ctx, "carol"); err != nil || got.PasswordFailures != 0 || got.LockedUntil != nil {
		t.Errorf("CreateUser of a user with a count and a lock: %+v, %v; want neither stored", got, err)
	}
	must(t, s.Tx(ctx, func(w index.Writer) error {
		if _, err := w.UnlockUser(ctx, carol.ID, at(10)); err != nil {
			return err
		}
		if err := w.ResetPasswordFailures(ctx, carol.ID, at(10)); err != nil {
			return err
		}
		_, err := w.FailPassword(ctx, carol.ID, at(11), at(12), 1)
		return err
	}))
	if got, err := s.GetUserByID(ctx, carol.ID); err != nil || !sameTime(got.LockedUntil, ptr(at(12))) {
		t.Errorf("carol after a lock in Tx: %+v, %v; want locked until %v", got, err, at(12))
	}
}

// testUsersLockoutRace: of replicas that count failures of one user at once, exactly one locks
// it, whatever the count they start from; the failures beyond the lock count nothing.
func testUsersLockoutRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	for _, start := range []int{0, lockLimit - 1} {
		u := newUser(t, s, "racer"+strconv.Itoa(start))
		for i := range start {
			if _, err := s.FailPassword(ctx, u.ID, at(i), at(1000), lockLimit); err != nil {
				t.Fatal(err)
			}
		}
		const replicas = 2 * lockLimit
		locks := make([]bool, replicas)
		errs := make([]error, replicas)
		var wg sync.WaitGroup
		for i := range replicas {
			wg.Go(func() { locks[i], errs[i] = s.FailPassword(ctx, u.ID, at(100), at(1000), lockLimit) })
		}
		wg.Wait()
		n := 0
		for i := range replicas {
			must(t, errs[i])
			if locks[i] {
				n++
			}
		}
		got, err := s.GetUserByID(ctx, u.ID)
		must(t, err)
		if n != 1 || got.PasswordFailures != 0 || !sameTime(got.LockedUntil, ptr(at(1000))) {
			t.Errorf("from %d failures, %d replicas at once: %d locked, PasswordFailures %d, LockedUntil %v; want 1, 0, %v",
				start, replicas, n, got.PasswordFailures, got.LockedUntil, at(1000))
		}
	}
}

// sameTime reports whether a and b are both nil or both the same time.
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The lockout (docs/spec/rbac.md §1, Local passwords; #1280, ADR 0028): with
// auth.lockout.duration set, the wrong passwords of a user are counted in the index, across the
// replicas, and the LockoutFailures-th in a row locks its password for that duration. A locked
// password is refused as a wrong one is; the user's tokens keep working. The throttles of failed
// logins stay beside it.

// LockoutFailures is how many wrong passwords in a row lock a user's password while the lockout
// is set: a constant, the most PCI DSS 8.3.4 allows.
const LockoutFailures = 10

// ErrPasswordRefused is the refusal of a user name and a password while the lockout is set,
// whatever its reason, a locked password among them: the message of its 401 says what locks a
// password and for how long (LockoutMessage), the same for every account and every reason. It
// wraps ErrBadCredentials.
var ErrPasswordRefused = fmt.Errorf("%w: a password refused while the lockout is set", ErrBadCredentials)

// LockoutMessage returns the message of the 401 of every refused user name and password while
// the lockout is set, "" while it is not: that LockoutFailures wrong passwords in a row lock a
// password, and for how long. It tells nobody more than the configuration does.
func (a *Authenticator) LockoutMessage() string {
	return a.lockoutMsg
}

// lockoutMessage is LockoutMessage for the duration d, "" for 0.
func lockoutMessage(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return fmt.Sprintf("the user name or the password was refused: %d wrong passwords in a row lock a password for %s",
		LockoutFailures, shortDuration(d))
}

// locked reports whether the password of u is locked now: the lockout is set and the lock of u,
// read with it, is in force. The authentication asks it before the verification cache, so that a
// remembered verification does not get past a lock.
func (a *Authenticator) locked(u *index.User) bool {
	return a.lockout > 0 && u.LockedUntil != nil && u.LockedUntil.After(a.now())
}

// failPassword counts the wrong password of u in the index while the lockout is set
// (index.Writer.FailPassword). The failure that locks writes its login.block row, with the limit
// lockout and no window, in the transaction of the lock: one row per lock, whatever the replicas,
// and none of the BlockRows of a minute. A count that cannot be written is logged at error and
// changes no answer.
func (a *Authenticator) failPassword(ctx context.Context, u *index.User) {
	if a.lockout <= 0 {
		return
	}
	now := a.now()
	until := now.Add(a.lockout)
	var locked bool
	err := a.index.Tx(ctx, func(w index.Writer) (err error) {
		if locked, err = w.FailPassword(ctx, u.ID, now, until, LockoutFailures); err != nil || !locked {
			return err
		}
		ev := block{limit: LimitLockout, credential: string(CredPassword), subject: UserPrincipal(u).String(),
			failures: LockoutFailures, until: until}.event(ctx, now, 0)
		return w.AppendAudit(ctx, &ev)
	})
	r := RequestFrom(ctx)
	switch {
	case err != nil:
		a.log.ErrorContext(ctx, "auth: a wrong password is not counted toward the lockout", "user", UserPrincipal(u).String(),
			"error", err.Error(), "requestId", r.ID)
	case locked:
		a.log.InfoContext(ctx, "password locked", "user", UserPrincipal(u).String(), "failures", LockoutFailures,
			"until", until.UTC().Format(time.RFC3339), "address", r.Address, "requestId", r.ID)
	}
}

// resetFailures sets the count of wrong passwords of u back to 0 while the lockout is set
// (index.Writer.ResetPasswordFailures), after its password verified (checked: a read when the
// count is 0 already) or was remembered with a count that is not 0. A lock in force, which
// another replica may have set since u was read, refuses the password as a locked one's, a
// failure of the throttles: a password verified while another replica locked it is not answered
// as a success.
func (a *Authenticator) resetFailures(ctx context.Context, u *index.User, checked bool, account, addr string) error {
	if a.lockout <= 0 || !checked && u.PasswordFailures == 0 {
		return nil
	}
	switch err := a.index.ResetPasswordFailures(ctx, u.ID, a.now()); {
	case errors.Is(err, index.ErrConflict):
		a.failed(ctx, account, addr)
		return a.refuse(ctx, CredPassword, u, "locked")
	case err != nil:
		return fmt.Errorf("auth: authenticate %s: the count of wrong passwords: %w", UserPrincipal(u), err)
	}
	return nil
}

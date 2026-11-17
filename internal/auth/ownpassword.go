package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/index"
)

// A user's own password and the change at first use (docs/spec/rbac.md §1, Local passwords;
// #1281). A user changes its password with its current one, which is the request's credential,
// checked as a Basic password is, with the throttles and the lockout. The new password follows the
// rules (PasswordPolicy) and is none of the user's last four, the current one and the three of its
// history, each compared by argon2id. Under mustChange, a user's password authenticates its own
// change and nothing else: once it has verified, every other use of it is ErrMustChange (403),
// which mints no token; a wrong password is refused as always, and the user's tokens are no
// password and keep working.

// ErrMustChange means that a password verified, and that its user must change it before it is
// used for anything else (index.User.MustChange): 403 with MsgMustChange, on every route that
// takes a password but the user's own change. It does not wrap ErrBadCredentials: only the holder
// of the password learns it.
var ErrMustChange = errors.New("auth: the password must be changed before it is used for anything else")

// MsgMustChange is the message of the 403 of ErrMustChange: what to do, the same for every user
// and every route, never a value of the request.
const MsgMustChange = "the password must be changed before it is used for anything else: change it with " +
	"lajstrom change-password, or POST /api/v1/users/{user}/password with currentPassword and password"

// errPasswordChanged rolls back a user's own change whose user's password changed, or whose user
// was disabled or deleted, after its current password verified.
var errPasswordChanged = errors.New("auth: the password changed since it verified")

// mustChange refuses the password of u, which verified, because u must change it: the line of
// the refusal, with the reason must change, and ErrMustChange. Nothing counts it as a failure.
func (a *Authenticator) mustChange(ctx context.Context, u *index.User) error {
	LogRefusal(ctx, a.log, string(CredPassword), UserPrincipal(u).String(), 0, "must change")
	return ErrMustChange
}

// ChangeOwnPassword makes password the password of the user called username, whose current
// password current is: the user's own change (docs/spec/rbac.md §1, Local passwords). While local
// passwords are off it changes none (Users.CheckPasswordSet, ErrInvalid naming the setting). The
// current password is checked as AuthenticatePassword checks a password, every attempt verified:
// a refusal wraps ErrBadCredentials, a user without a password included, after the same argon2id
// work, and a wrong password counts toward the throttles and the lockout; a throttled account or
// address is a *ThrottledError; a user inactive beyond auth.disableInactiveUsersAfter is disabled
// and refused. A user that must change its password is not refused: this is the one use of it.
// A new password that breaks a rule of the PasswordPolicy of Options.Users, or that is one of the
// user's last four passwords, wraps ErrInvalid, and nothing is written. The change is one
// user.password audit event by the user itself; the current hash goes to the head of the history,
// the oldest beyond PasswordHistoryLen leaves it, and mustChange is cleared; the user's tokens are
// not touched. It needs Options.Users.
func (a *Authenticator) ChangeOwnPassword(ctx context.Context, username, current, password string, o Origin) error {
	if a.users == nil {
		return errors.New("auth: change a password: the authenticator has no Users")
	}
	if err := a.users.CheckPasswordSet(); err != nil {
		return err
	}
	u, err := a.checkedPassword(ctx, username, current)
	if err != nil {
		return err
	}
	err = a.users.changeOwn(WithPrincipal(ctx, UserPrincipal(u)), u, password, o)
	if errors.Is(err, errPasswordChanged) {
		return a.refuse(ctx, CredPassword, u, "password changed meanwhile")
	}
	return err
}

// changeOwn makes password the password of u, whose current password the request verified, with
// the user.password event of its own change (ChangeOwnPassword): the rules of PasswordPolicy,
// then the last four passwords, then, in one transaction, the row as it is then, which must still
// be the live, enabled user with the hash that verified (errPasswordChanged otherwise).
func (s *Users) changeOwn(ctx context.Context, u *index.User, password string, o Origin) error {
	if err := s.PasswordPolicy.Check(password); err != nil {
		return err
	}
	if err := usedBefore(u, password); err != nil {
		return err
	}
	hash := hashWith(hashParams, password)
	var ev index.AuditEvent
	err := s.Index.Tx(index.WithRepeatableRead(ctx), func(w index.Writer) error {
		row, err := w.GetUserByID(ctx, u.ID)
		if err != nil {
			return err
		}
		if row.PasswordHash != u.PasswordHash || row.Disabled || row.DeletedAt != nil {
			return errPasswordChanged
		}
		row.PasswordHistory = withHistory(row.PasswordHistory, row.PasswordHash)
		row.PasswordHash, row.MustChange = hash, false
		if err := w.UpdateUser(ctx, row); err != nil {
			return err
		}
		ev, err = s.record(ctx, w, ActionUserPassword, row, o, map[string]any{})
		return err
	})
	switch {
	case errors.Is(err, errPasswordChanged), errors.Is(err, index.ErrNotFound):
		return errPasswordChanged
	case err != nil:
		return fmt.Errorf("auth: change the password of %s: %w", UserPrincipal(u), err)
	}
	s.writeAudit(ctx, ev)
	return nil
}

// usedBefore returns an error wrapping ErrInvalid when password is one of the last four passwords
// of u, its current one and those of its PasswordHistory, each compared by argon2id
// (docs/spec/rbac.md §1, A user's own password). A hash that does not parse is a fault of the
// stored data, not a refusal.
func usedBefore(u *index.User, password string) error {
	for _, hash := range append([]string{u.PasswordHash}, u.PasswordHistory...) {
		same, err := VerifyPassword(hash, password)
		switch {
		case err != nil:
			return fmt.Errorf("auth: the password history of %s: %w", UserPrincipal(u), err)
		case same:
			return invalidf("password: was used before: choose one that is none of your last %d passwords", PasswordHistoryLen+1)
		}
	}
	return nil
}

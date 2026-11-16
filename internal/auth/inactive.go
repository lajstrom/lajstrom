package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Inactive users (docs/spec/rbac.md §1, Inactive users; #1283): a user's last activity is its
// last successful authentication, users.last_seen_at; with auth.disableInactiveUsersAfter set,
// the authentication of a user inactive beyond it refuses the credential and disables the user.
// No task looks for inactive users.

// SourceInactivity is the source of the user.disable row of a user that its inactivity disables
// (docs/spec/rbac.md §5).
const SourceInactivity = "inactivity"

// ReasonInactive is the reason in the details of that row.
const ReasonInactive = "inactive"

// LastActivity returns the time the inactivity of u counts from: its last activity
// (index.User.LastSeenAt), or its creation when it has none.
func LastActivity(u *index.User) time.Time {
	if u.LastSeenAt != nil {
		return *u.LastSeenAt
	}
	return u.CreatedAt
}

// Inactive reports whether u is inactive at now under after, auth.disableInactiveUsersAfter: a
// live, enabled user whose last activity (LastActivity) is more than after before now. With
// after zero nobody is.
func Inactive(u *index.User, after time.Duration, now time.Time) bool {
	return after > 0 && !u.Disabled && u.DeletedAt == nil && now.Sub(LastActivity(u)) > after
}

// inactivityOrigin is the Origin of the disabling of an inactive user by the request of ctx: the
// source inactivity, and the client's address and the request's id of WithRequest.
func inactivityOrigin(ctx context.Context) Origin {
	r := RequestFrom(ctx)
	return Origin{Source: SourceInactivity, IP: r.Address, RequestID: r.ID}
}

// errLastAdmin rolls back the disabling of the last enabled user with an admin binding, which
// inactivity never disables.
var errLastAdmin = errors.New("auth: the last enabled user with an admin binding")

// DisableInactive disables the user id, whose credential an authentication found inactive
// (Inactive under InactiveAfter), and reports whether the credential is refused. In one
// transaction at repeatable read, it reads the user again and disables it only while it is still
// live, enabled and inactive, with one user.disable row by anonymous: the source inactivity and
// the client's address and request id of o, and in its details the reason inactive, its
// lastSeenAt (absent without one) and the setting as after; with DisableWithOwner, the enabled
// service accounts it owns go with it, each with a row of its own (docs/spec/rbac.md §1, §5).
//
// A user found disabled or deleted meanwhile is refused without a row. A user found active again
// (an admin enabled it meanwhile, which restarts its clock) is not refused. The last enabled user
// with an admin binding is not disabled and not refused, with a warning: the rule of §1, which a
// service account's and a group's admin bindings do not count for. An error of the index refuses
// the credential and is returned.
func (s *Users) DisableInactive(ctx context.Context, id int64, o Origin) (refused bool, err error) {
	o.Source = SourceInactivity
	ctx = WithPrincipal(ctx, Anonymous)
	var (
		row  *index.User
		seen *time.Time
		evs  []index.AuditEvent
	)
	err = s.Index.Tx(index.WithRepeatableRead(ctx), func(w index.Writer) (err error) {
		evs, refused = nil, false
		if row, err = w.GetUserByID(ctx, id); err != nil {
			return err
		}
		if row.Disabled || row.DeletedAt != nil {
			refused = true
			return nil
		}
		if !Inactive(row, s.InactiveAfter, s.now()) {
			return nil
		}
		seen = row.LastSeenAt
		evs, err = s.disableInactive(ctx, w, row, o)
		refused = err == nil
		return err
	})
	switch {
	case errors.Is(err, errLastAdmin):
		s.log().WarnContext(ctx, "the last enabled user with an admin binding is inactive beyond auth.disableInactiveUsersAfter, "+
			"and is never disabled: its credential is taken; create another admin, or let it sign in more often",
			inactiveAttrs(row, seen, s.InactiveAfter)...)
		return false, nil
	case err != nil:
		return true, fmt.Errorf("auth: disable the inactive user %d: %w", id, err)
	}
	for _, ev := range evs {
		s.writeAudit(ctx, ev)
	}
	if len(evs) > 0 {
		s.log().InfoContext(ctx, "user disabled: inactive beyond auth.disableInactiveUsersAfter, at the authentication of its credential",
			inactiveAttrs(row, seen, s.InactiveAfter)...)
	}
	return refused, nil
}

// disableInactive disables row, inactive, on w, and returns its user.disable event, then those
// of the service accounts it owns that go with it (DisableInactive); errLastAdmin when it is the
// last enabled user with an admin binding.
func (s *Users) disableInactive(ctx context.Context, w index.Writer, row *index.User, o Origin) ([]index.AuditEvent, error) {
	seen := row.LastSeenAt
	row.Disabled = true
	if err := w.UpdateUser(ctx, row); err != nil {
		return nil, err
	}
	if err := keepInactiveAdmin(ctx, w, row); err != nil {
		return nil, err
	}
	details := map[string]any{"reason": ReasonInactive, "after": shortDuration(s.InactiveAfter)}
	if seen != nil {
		details["lastSeenAt"] = seen.UTC().Format(time.RFC3339Nano)
	}
	ev, err := s.record(ctx, w, ActionUserDisable, row, o, details)
	if err != nil || !s.DisableWithOwner || row.Kind == KindService {
		return []index.AuditEvent{ev}, err
	}
	owned, err := s.disableOwned(ctx, w, row, o)
	return append([]index.AuditEvent{ev}, owned...), err
}

// keepInactiveAdmin returns errLastAdmin when u, just disabled on w, has an admin binding and no enabled
// user with one is left (anyAdmin); a service account counts for neither.
func keepInactiveAdmin(ctx context.Context, w index.Writer, u *index.User) error {
	if u.Kind == KindService {
		return nil
	}
	admin, err := hasAdminBinding(ctx, w, u)
	if err != nil || !admin {
		return err
	}
	switch ok, err := anyAdmin(ctx, w); {
	case err != nil:
		return err
	case !ok:
		return errLastAdmin
	}
	return nil
}

// inactiveAttrs are the attributes of a log line about the inactive user u: its principal, its
// last activity seen (none: its creation) and the setting.
func inactiveAttrs(u *index.User, seen *time.Time, after time.Duration) []any {
	attrs := []any{"user", UserPrincipal(u).String(), "after", shortDuration(after)}
	if seen != nil {
		return append(attrs, "lastSeenAt", seen.UTC().Format(time.RFC3339))
	}
	return append(attrs, "createdAt", u.CreatedAt.UTC().Format(time.RFC3339))
}

// InactiveUsers counts the users inactive now (Inactive under InactiveAfter), for the warning of
// serve at its start (docs/spec/rbac.md §1, What is shown), and returns the name of the last
// enabled user with an admin binding when it is among them: the one enabled user with an admin
// binding, not a service account, which inactivity never disables. With InactiveAfter zero it
// counts none.
func (s *Users) InactiveUsers(ctx context.Context) (n int, lastAdminName string, err error) {
	if s.InactiveAfter <= 0 {
		return 0, "", nil
	}
	users, err := s.Index.ListUsers(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("auth: count the inactive users: %w", err)
	}
	bindings, err := s.Index.ListBindings(ctx, "")
	if err != nil {
		return 0, "", fmt.Errorf("auth: count the inactive users: %w", err)
	}
	admins := map[string]bool{}
	for _, b := range bindings {
		if b.Role == RoleAdmin {
			admins[b.Principal] = true
		}
	}
	now := s.now()
	var enabledAdmins []*index.User
	for i := range users {
		u := &users[i]
		if Inactive(u, s.InactiveAfter, now) {
			n++
		}
		if !u.Disabled && u.Kind != KindService && admins[UserPrincipal(u).String()] {
			enabledAdmins = append(enabledAdmins, u)
		}
	}
	if len(enabledAdmins) == 1 && Inactive(enabledAdmins[0], s.InactiveAfter, now) {
		lastAdminName = enabledAdmins[0].Name
	}
	return n, lastAdminName, nil
}

// shortDuration is d as a configuration writes it: 2160h, not 2160h0m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// admit decides on the user u, whose credential has verified (docs/spec/rbac.md §1, Inactive
// users). A user inactive beyond the InactiveAfter of Options.Users is disabled
// (DisableInactive) and its credential refused, ErrBadCredentials, unless DisableInactive keeps
// it: the last enabled admin, or a user an admin enabled meanwhile. Otherwise the activity is
// recorded (recordSeen) and admit returns nil.
func (a *Authenticator) admit(ctx context.Context, cred CredentialKind, u *index.User) error {
	if a.users != nil && Inactive(u, a.users.InactiveAfter, a.now()) {
		refused, err := a.users.DisableInactive(ctx, u.ID, inactivityOrigin(ctx))
		if err != nil {
			a.log.WarnContext(ctx, "an inactive user's credential is refused, and the user is not disabled",
				"user", UserPrincipal(u).String(), "error", err.Error())
		}
		if refused {
			return a.refuse(ctx, cred, u, "inactive")
		}
	}
	a.recordSeen(ctx, u)
	return nil
}

// admitPassword is admit for a password of the user u, whose refusal counts as a failed login
// of account and addr, as a disabled user's does; it returns the principal user:<uid>.
func (a *Authenticator) admitPassword(ctx context.Context, u *index.User, account, addr string) (Principal, error) {
	if err := a.admit(ctx, CredPassword, u); err != nil {
		a.failed(account, addr)
		return Principal{}, err
	}
	return UserPrincipal(u), nil
}

// recordSeen records the activity of the user u now (TouchUser) as touch records a token's use:
// when its last activity, as read with it on this request, is a minute old or more, or none, so
// at most once a minute per user on all replicas together. This replica's last record, by user,
// keeps requests that read the same old value at once from writing it each. A record that fails
// is logged at debug, refuses nothing, and is forgotten, so that the next request records it.
func (a *Authenticator) recordSeen(ctx context.Context, u *index.User) {
	now := a.now()
	if u.LastSeenAt != nil && now.Sub(*u.LastSeenAt) < touchInterval {
		return
	}
	if !a.seen.putUnless(u.ID, now, func(last time.Time) bool { return now.Sub(last) < touchInterval }) {
		return
	}
	if err := a.index.TouchUser(ctx, u.ID, now); err != nil {
		a.seen.removeIf(u.ID, now.Equal) // a record made since stays
		a.log.DebugContext(ctx, "the activity of a user is not recorded", "user", UserPrincipal(u).String(), "error", err.Error())
	}
}

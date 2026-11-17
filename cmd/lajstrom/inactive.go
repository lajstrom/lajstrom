package main

import (
	"context"
	"log/slog"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// usersOf is the auth.Users of serve's authentications and OIDC logins: what disabling a user
// owner does to its service accounts, and the users inactive beyond
// auth.disableInactiveUsersAfter, whose authentication disables them (docs/spec/rbac.md §1).
func usersOf(c *config.Config, st index.Store, log *slog.Logger) *auth.Users {
	return &auth.Users{Index: st, Log: log, DisableWithOwner: c.Auth.ServiceAccounts.OnOwnerDisabled == config.OwnerDisable,
		InactiveAfter: c.Auth.DisableInactiveUsersAfter, PasswordsOff: !c.Auth.LocalPasswords}
}

// warnInactiveUsers logs at warn, at serve's start, how many users are inactive beyond
// auth.disableInactiveUsersAfter, when there are any, and the last enabled admin among them when
// it is (docs/spec/rbac.md §1, What is shown): whom the setting reaches before the next
// authentication of each disables it. A failure to count them is logged and stops nothing.
func warnInactiveUsers(ctx context.Context, users *auth.Users, log *slog.Logger) {
	n, admin, err := users.InactiveUsers(ctx)
	switch {
	case err != nil:
		log.WarnContext(ctx, "the users inactive beyond auth.disableInactiveUsersAfter are not counted", "error", err.Error())
		return
	case n == 0:
		return
	}
	attrs := []any{"users", n, "auth.disableInactiveUsersAfter", users.InactiveAfter.String()}
	msg := "users are inactive beyond auth.disableInactiveUsersAfter: the next authentication of each disables it " +
		"(lajstrom admin user list marks them)"
	if admin != "" {
		attrs = append(attrs, "lastEnabledAdmin", admin)
		msg += "; the last enabled admin is among them, and is never disabled: its credential is taken"
	}
	log.WarnContext(ctx, msg, attrs...)
}

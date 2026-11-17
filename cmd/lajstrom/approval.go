package main

import (
	"context"
	"log/slog"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// warnApprovers logs at warn, at start under auth.approval second-admin (docs/spec/rbac.md §13),
// when fewer than two users could decide a request (auth.Approvers): under the admin mode the
// enabled OIDC users with an admin binding, else the enabled users with one, service accounts
// apart. A request needs an admin other than its requester; the command line makes the change at
// once, and is the way to a second admin. It says at info that the setting is on. A count that
// fails is logged and refuses nothing.
func warnApprovers(ctx context.Context, a config.Auth, r index.Reader, log *slog.Logger) {
	if a.Approval != config.ApprovalSecondAdmin {
		return
	}
	log.InfoContext(ctx, "the changes that grant the most wait for a second admin: an admin binding, a binding on */* and a deploy "+
		"token over a mask with write, made through the API", "auth.approval", a.Approval)
	n, err := auth.Approvers(ctx, r, a.Admin.RequireMFA)
	switch {
	case err != nil:
		log.WarnContext(ctx, "the users who could approve a request are not counted", "error", err.Error())
	case n < 2:
		log.WarnContext(ctx, "fewer than two enabled users have an admin binding that counts: no request for a second admin could be "+
			"approved while auth.approval is second-admin. With the database, make the change at once, or the second admin, with "+
			"lajstrom admin binding create", "approvers", n, "auth.admin.requireMFA", a.Admin.RequireMFA)
	}
}

package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// errAdminMFAWithoutOIDC is serve's refusal of auth.admin.requireMFA without an IdP.
var errAdminMFAWithoutOIDC = errors.New("auth.admin.requireMFA is true and auth.oidc.issuer is empty: no request could reach the " +
	"admin role, which counts only for a web UI's sign-in through OIDC. Configure auth.oidc, or set auth.admin.requireMFA to false")

// checkAuthModes is serve's check of auth at start, before anything starts: auth.localPasswords
// (checkLocalPasswords), then auth.admin.requireMFA (checkAdminMFA).
func checkAuthModes(a config.Auth, log *slog.Logger) error {
	if err := checkLocalPasswords(a, log); err != nil {
		return err
	}
	return checkAdminMFA(a, log)
}

// checkAdminMFA is serve's check of auth.admin.requireMFA at start (docs/spec/rbac.md §12): true
// needs auth.oidc.issuer, since the admin role then counts only for a sign-in through the IdP.
// It says at info when the mode is on.
func checkAdminMFA(a config.Auth, log *slog.Logger) error {
	switch {
	case !a.Admin.RequireMFA:
		return nil
	case a.OIDC.Issuer == "":
		return errAdminMFAWithoutOIDC
	}
	log.Info("the admin mode is on: the admin role counts only for a web UI's sign-in through OIDC with a second factor",
		"auth.admin.requireMFA", true, "auth.oidc.mfa.amr", a.OIDC.MFA.AMR, "auth.oidc.mfa.acr", a.OIDC.MFA.ACR)
	return nil
}

// warnAdminMFA logs at warn, at start under the admin mode (docs/spec/rbac.md §12), how many
// admin bindings of service accounts and local users the mode leaves without effect, when there
// are any, and, when no enabled OIDC user and no group has an admin binding, that no request can
// reach the admin role and how to give one. A count that fails is logged and refuses nothing.
func warnAdminMFA(ctx context.Context, a config.Auth, r index.Reader, log *slog.Logger) {
	if !a.Admin.RequireMFA {
		return
	}
	idle, reachable, err := auth.AdminBindingsUnderMFA(ctx, r)
	if err != nil {
		log.WarnContext(ctx, "the admin bindings under auth.admin.requireMFA are not counted", "error", err.Error())
		return
	}
	if idle > 0 {
		log.WarnContext(ctx, "admin bindings of service accounts and local users count for nothing while auth.admin.requireMFA is on: "+
			"they never sign in through OIDC; they count again when it is off", "bindings", idle)
	}
	if !reachable {
		log.WarnContext(ctx, "no enabled OIDC user and no group has an admin binding: no request can reach the admin role while "+
			"auth.admin.requireMFA is on. With the database, run "+
			"lajstrom admin binding create --principal user:<name> --role admin --mask '*/*' for a person who logs in through the IdP")
	}
}

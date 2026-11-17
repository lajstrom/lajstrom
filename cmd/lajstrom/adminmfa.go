package main

import (
	"errors"
	"log/slog"

	"github.com/lajstrom/lajstrom/internal/config"
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

package main

import (
	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/config"
)

// accessConfigOf is what the access report reads of the configuration c (docs/spec/rbac.md §14,
// #1279): serve's GET /api/v1/access-report and lajstrom admin access-report both take it from
// here, so that the same rows at the same time are the same document from either.
func accessConfigOf(c *config.Config) api.AccessConfig {
	return api.AccessConfig{AdminMFA: c.Auth.Admin.RequireMFA, AnonymousRead: c.Auth.AnonymousRead,
		InactiveAfter: c.Auth.DisableInactiveUsersAfter, Lockout: lockoutOf(c.Auth), DeployMaxTTL: c.Auth.DeployTokens.MaxTTL,
		ServiceMaxTTL: c.Auth.ServiceAccounts.MaxTokenTTL, CIPolicies: c.Auth.CI.Policies}
}

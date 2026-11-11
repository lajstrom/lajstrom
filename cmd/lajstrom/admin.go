package main

import "github.com/spf13/cobra"

// newAdminCmd returns the admin group: repo, key, deb, user, token, binding, bootstrap and
// retention.
func newAdminCmd(a *app) *cobra.Command {
	admin := &cobra.Command{
		Use:   "admin",
		Short: "Manage repos, signing keys, users, tokens and bindings; apply a retention policy",
	}
	admin.AddCommand(newAdminRepoCmd(a), newAdminKeyCmd(a), newAdminDebCmd(), newAdminUserCmd(a), newAdminTokenCmd(a),
		newAdminBindingCmd(a), newAdminBootstrapCmd(a), newAdminRetentionCmd(a))
	return admin
}

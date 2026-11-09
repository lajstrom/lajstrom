package main

import "github.com/spf13/cobra"

// newAdminCmd returns the admin group: repo, key, deb, user, token and binding.
func newAdminCmd(a *app) *cobra.Command {
	admin := &cobra.Command{
		Use:   "admin",
		Short: "Manage repos, signing keys, users, tokens and bindings",
	}
	admin.AddCommand(newAdminRepoCmd(a), newAdminKeyCmd(a), newAdminDebCmd(), newAdminUserCmd(a), newAdminTokenCmd(a),
		newAdminBindingCmd(a))
	return admin
}

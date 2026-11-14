package main

import "github.com/spf13/cobra"

// newAdminCmd returns the admin group: repo, key, deb, user, token, binding, bootstrap,
// retention and trusted-root.
func newAdminCmd(a *app) *cobra.Command {
	admin := &cobra.Command{
		Use:   "admin",
		Short: "Manage repos, signing keys, users, tokens and bindings; apply a retention policy",
	}
	admin.AddCommand(newAdminRepoCmd(a), newAdminKeyCmd(a), newAdminDebCmd(), newAdminRpmCmd(), newAdminUserCmd(a), newAdminTokenCmd(a),
		newAdminBindingCmd(a), newAdminBootstrapCmd(a), newAdminRetentionCmd(a), newAdminTrustedRootCmd())
	return admin
}

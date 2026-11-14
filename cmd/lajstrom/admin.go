package main

import "github.com/spf13/cobra"

// newAdminCmd returns the admin group: repo, key, deb, user, token, binding, bootstrap, retention
// and snapshot.
func newAdminCmd(a *app) *cobra.Command {
	admin := &cobra.Command{
		Use:   "admin",
		Short: "Manage repos, signing keys, users, tokens and bindings; apply a retention policy; snapshot the control plane",
	}
	admin.AddCommand(newAdminRepoCmd(a), newAdminKeyCmd(a), newAdminDebCmd(), newAdminRpmCmd(), newAdminUserCmd(a), newAdminTokenCmd(a),
		newAdminBindingCmd(a), newAdminBootstrapCmd(a), newAdminRetentionCmd(a), newAdminSnapshotCmd(a))
	return admin
}

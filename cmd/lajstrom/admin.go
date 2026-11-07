package main

import "github.com/spf13/cobra"

// newAdminCmd returns the admin group: repo, key, deb, user, token, and the placeholder binding
// until #47 implements it.
func newAdminCmd(a *app) *cobra.Command {
	admin := &cobra.Command{
		Use:   "admin",
		Short: "Manage repos, signing keys, users, tokens and bindings",
	}
	admin.AddCommand(newAdminRepoCmd(a), newAdminKeyCmd(a), newAdminDebCmd(), newAdminUserCmd(a), newAdminTokenCmd(a))
	for _, sub := range []struct {
		use, short string
		issue      int
	}{
		{"binding", "Manage role bindings", 47},
	} {
		issue := sub.issue
		admin.AddCommand(&cobra.Command{
			Use:   sub.use,
			Short: sub.short + " (not implemented yet)",
			RunE: func(*cobra.Command, []string) error {
				return notImplemented(issue)
			},
		})
	}
	return admin
}

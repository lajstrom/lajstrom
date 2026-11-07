package main

import "github.com/spf13/cobra"

// newAdminCmd returns the admin group: repo, and the placeholders user, token and binding
// until #47 implements them.
func newAdminCmd(a *app) *cobra.Command {
	admin := &cobra.Command{
		Use:   "admin",
		Short: "Manage repos, users, tokens and bindings",
	}
	admin.AddCommand(newAdminRepoCmd(a))
	for _, sub := range []struct {
		use, short string
		issue      int
	}{
		{"user", "Manage local users", 47},
		{"token", "Manage personal access and deploy tokens", 47},
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

package main

import "github.com/spf13/cobra"

// newMigrateCmd returns migrate, which will apply the index migrations (issue #10). For now it
// loads and validates the configuration and fails.
func newMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:         "migrate",
		Short:       "Apply the index database migrations (not implemented yet)",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(*cobra.Command, []string) error {
			return notImplemented(10)
		},
	}
}

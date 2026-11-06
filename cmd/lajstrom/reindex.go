package main

import "github.com/spf13/cobra"

// newReindexCmd returns reindex, which will rebuild the derived index tables from the
// registries (issue #29). For now it loads and validates the configuration and fails.
func newReindexCmd() *cobra.Command {
	return &cobra.Command{
		Use:         "reindex",
		Short:       "Rebuild the index from the manifests in the registries (not implemented yet)",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(*cobra.Command, []string) error {
			return notImplemented(29)
		},
	}
}

package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/version"
)

// newVersionCmd returns version: "lajstrom <version> (<commit>, <date>)", or the JSON of
// /-/version with --json.
func newVersionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version, commit and build date",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := version.Get()
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(v)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "lajstrom %s (%s, %s)\n", v.Version, orUnknown(v.Commit), orUnknown(v.Date))
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON: version, commit, date")
	return cmd
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

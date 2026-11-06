package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// newMigrateCmd returns migrate: apply the embedded index migrations the database does not have
// yet, under the migration lock (index.ApplyMigrations), and print "applied NNNN" for each, or
// "schema is current at version NNNN" when none was pending. Running it again is a no-op.
func newMigrateCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "migrate",
		Short:       "Apply the index database migrations",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			ctx := cmd.Context()
			st, err := openStore(ctx, a.cfg.Database)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, st.Close()) }()

			out := cmd.OutOrStdout()
			applied, err := st.ApplyMigrations(ctx)
			for _, v := range applied {
				if _, werr := fmt.Fprintf(out, "applied %04d\n", v); werr != nil {
					return errors.Join(err, werr)
				}
			}
			if err != nil || len(applied) > 0 {
				return err
			}
			versions, err := st.AppliedMigrations(ctx)
			if err != nil {
				return err
			}
			current := 0
			if len(versions) > 0 {
				current = versions[len(versions)-1]
			}
			_, err = fmt.Fprintf(out, "schema is current at version %04d\n", current)
			return err
		},
	}
}

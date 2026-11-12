package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/index"
)

// newMigrateCmd returns migrate: apply the embedded index migrations the database does not have
// yet, under the migration lock (index.ApplyMigrations), and print "applied NNNN" for each, or
// "schema is current at version NNNN" when none was pending. Running it again is a no-op. On a
// schema of a newer binary that this one runs on, it says so and applies nothing; on one that
// it does not run on, and on a pending migration that would break the binaries serving on the
// schema, unless --allow-breaking, it fails and applies nothing (docs/operations.md, Upgrades).
func newMigrateCmd(a *app) *cobra.Command {
	var breaking bool
	cmd := &cobra.Command{
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
			rep, err := st.ApplyMigrations(ctx, index.MigrateOptions{Breaking: breaking})
			for _, v := range rep.Applied {
				if _, werr := fmt.Fprintf(out, "applied %04d\n", v); werr != nil {
					return errors.Join(err, werr)
				}
			}
			switch {
			case err != nil:
				return migrationHint(inFile(a.cfg.Database, err))
			case len(rep.Applied) > 0:
				return nil
			case rep.Version > rep.Known:
				_, err = fmt.Fprintf(out, "schema is at version %04d, newer than this binary's %04d, and compatible with it\n",
					rep.Version, rep.Known)
			default:
				_, err = fmt.Fprintf(out, "schema is current at version %04d\n", rep.Version)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&breaking, "allow-breaking", false,
		"apply a migration that the binaries serving on the current schema cannot run on: stop them first")
	return cmd
}

// migrationHint adds to an error of ApplyMigrations what the operator can do about it.
func migrationHint(err error) error {
	switch {
	case errors.Is(err, index.ErrSchemaTooNew):
		return fmt.Errorf("%w; run a newer lajstrom (docs/operations.md, Upgrades)", err)
	case errors.Is(err, index.ErrBreakingMigration):
		return fmt.Errorf("%w; upgrade through the release in between, or stop every replica and run "+
			"lajstrom migrate --allow-breaking (docs/operations.md, Upgrades)", err)
	}
	return err
}

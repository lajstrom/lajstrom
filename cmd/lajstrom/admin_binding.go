package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
)

// newAdminBindingCmd returns admin binding: create, list and delete the bindings of
// docs/spec/rbac.md §3 in the index database, without HTTP and without credentials, through the
// bindings service (auth.Bindings), so that the rules and the audit events are those of the
// bindings API (#47). A change counts from the next request on, on every replica. It is made by
// the anonymous principal with the source cli. Like admin user it assumes that lajstrom migrate
// ran.
func newAdminBindingCmd(a *app) *cobra.Command {
	b := &cobra.Command{
		Use:   "binding",
		Short: "Manage the bindings: which role a user has on which repos",
	}
	b.AddCommand(newBindingCreateCmd(a), newBindingListCmd(a), newBindingDeleteCmd(a))
	return b
}

// newBindingCreateCmd returns admin binding create: create a binding and print it as JSON on
// one line.
func newBindingCreateCmd(a *app) *cobra.Command {
	var principal, role, mask string
	cmd := &cobra.Command{
		Use:         "create --principal user:<name> --role reader|writer|maintainer|admin --mask <format>/<repo>",
		Short:       "Give a user a role on the repos a mask matches, and print the binding as JSON",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withBindings(cmd.Context(), func(ctx context.Context, bindings *auth.Bindings) error {
				b, err := bindings.Create(ctx, principal, role, mask, auth.Origin{Source: api.SourceCLI})
				if err != nil {
					return err
				}
				return writeJSONLine(cmd.OutOrStdout(), b)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&principal, "principal", "", "who: user:<name> of an existing user")
	f.StringVar(&role, "role", "", "reader (read), writer (and write), maintainer (and delete, manage) or admin (everything, and the system)")
	f.StringVar(&mask, "mask", "", "which repos: apt/ubuntu, apt/* (a format), */payments (a name in every format), */* (all); admin is on */*")
	for _, name := range []string{"principal", "role", "mask"} {
		_ = cmd.MarkFlagRequired(name) // fails only for a flag that does not exist
	}
	return cmd
}

// newBindingListCmd returns admin binding list: the bindings, or those of one principal, as a
// table.
func newBindingListCmd(a *app) *cobra.Command {
	var principal string
	cmd := &cobra.Command{
		Use:         "list [--principal user:<name>]",
		Short:       "List the bindings, or those of one principal",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withBindings(cmd.Context(), func(ctx context.Context, bindings *auth.Bindings) error {
				list, err := bindings.List(ctx, principal)
				if err != nil {
					return err
				}
				return writeBindingTable(cmd.OutOrStdout(), list)
			})
		},
	}
	cmd.Flags().StringVar(&principal, "principal", "", "only the bindings of this principal")
	return cmd
}

// newBindingDeleteCmd returns admin binding delete: the binding grants nothing from the next
// request on; the last admin binding of an enabled user is refused.
func newBindingDeleteCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "delete <id>",
		Short:       "Delete a binding: it grants nothing from the next request on",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return fmt.Errorf("binding id %q: want the number of the ID column of admin binding list", args[0])
			}
			return a.withBindings(cmd.Context(), func(ctx context.Context, bindings *auth.Bindings) error {
				return bindings.Delete(ctx, id, auth.Origin{Source: api.SourceCLI})
			})
		},
	}
}

// withBindings opens the index database of the configuration, runs fn with the bindings service
// over it, and closes the database.
func (a *app) withBindings(ctx context.Context, fn func(context.Context, *auth.Bindings) error) (err error) {
	st, err := openStore(ctx, a.cfg.Database)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	return fn(ctx, &auth.Bindings{Index: st, Formats: api.RepoFormat, Log: a.log})
}

// writeBindingTable prints bindings as a table with the columns ID, USER, PRINCIPAL, ROLE, MASK and
// CREATED.
func writeBindingTable(out io.Writer, bindings []auth.Binding) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	_, _ = io.WriteString(tw, "ID\tUSER\tPRINCIPAL\tROLE\tMASK\tCREATED\n") // a bytes.Buffer does not fail
	for _, b := range bindings {
		name := b.Name
		if name == "" {
			name = "-"
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", b.ID, name, b.Principal, b.Role, b.RepoMask, b.CreatedAt.Format(time.RFC3339))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := out.Write(buf.Bytes())
	return err
}

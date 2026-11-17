package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// newAdminApprovalCmd returns admin approval: list the requests that wait for a second admin
// (docs/spec/rbac.md §13) in the index database, without HTTP and without credentials, as GET
// /api/v1/approvals answers them. The command line decides none: an approval is a second admin's,
// through the API; the operator makes the change on the command line instead, at once (admin
// binding create, admin token create --mask), and a request left waits until it expires.
func newAdminApprovalCmd(a *app) *cobra.Command {
	c := &cobra.Command{
		Use:   "approval",
		Short: "List the requests for a second admin: the changes of the API that wait for one",
	}
	c.AddCommand(newApprovalListCmd(a))
	return c
}

// newApprovalListCmd returns admin approval list: the requests, newest first, of a state or of
// every state, as a table.
func newApprovalListCmd(a *app) *cobra.Command {
	var (
		state string
		limit int
	)
	cmd := &cobra.Command{
		Use:         "list [--state pending|approved|rejected|expired] [--limit <n>]",
		Short:       "List the requests for a second admin, newest first",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if limit < 1 {
				return usageError("--limit %d: want 1 or more", limit)
			}
			ctx := cmd.Context()
			st, err := a.openMigrated(ctx)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, st.Close()) }()
			list, err := (&auth.Approvals{Index: st, Log: a.log}).List(ctx, state, 0, limit)
			if err != nil {
				return err
			}
			return writeApprovalTable(ctx, cmd.OutOrStdout(), st, list)
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "only the requests of this state; pending are those that wait")
	cmd.Flags().IntVar(&limit, "limit", 100, "at most this many, the newest")
	return cmd
}

// writeApprovalTable prints the requests as a table with the columns ID, KIND, STATE, CHANGE,
// REQUESTED BY, REQUESTED, EXPIRES and DECIDED BY, a user by its name when r has it.
func writeApprovalTable(ctx context.Context, out io.Writer, r index.Reader, list []auth.Approval) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	_, _ = io.WriteString(tw, "ID\tKIND\tSTATE\tCHANGE\tREQUESTED BY\tREQUESTED\tEXPIRES\tDECIDED BY\n") // a bytes.Buffer does not fail
	for _, a := range list {
		change, err := changeText(ctx, r, a)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Kind, a.State, change, userText(ctx, r, a.RequestedBy),
			a.RequestedAt.Format(time.RFC3339), a.ExpiresAt.Format(time.RFC3339), userText(ctx, r, a.DecidedBy))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := out.Write(buf.Bytes())
	return err
}

// changeText is the change of a for a person: a binding's principal, role and mask, a deploy
// token's mask, name and scopes.
func changeText(ctx context.Context, r index.Reader, a auth.Approval) (string, error) {
	var c struct {
		Principal, Role, RepoMask, Name string
		Scopes                          []string
	}
	if err := json.Unmarshal(a.Change, &c); err != nil {
		return "", fmt.Errorf("the change of request %d: %w", a.ID, err)
	}
	if a.Kind == index.ApprovalDeployToken {
		return fmt.Sprintf("%s %s %s", c.RepoMask, c.Name, strings.Join(c.Scopes, ",")), nil
	}
	return fmt.Sprintf("%s %s %s", userText(ctx, r, c.Principal), c.Role, c.RepoMask), nil
}

// userText is principal for a person: user:<name> for a user's user:<uid> that r has, the
// principal as it is otherwise, and - for none.
func userText(ctx context.Context, r index.Reader, principal string) string {
	p, err := auth.ParsePrincipal(principal)
	switch {
	case principal == "":
		return "-"
	case err != nil || p.Kind != auth.KindUser || !index.IsUID(p.Name):
		return principal
	}
	u, err := r.GetUserByUID(ctx, p.Name)
	if err != nil {
		return principal
	}
	name := u.Name
	if u.DeletedAt != nil {
		name = u.DeletedName
	}
	return "user:" + name
}

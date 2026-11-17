package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// accessConfigOf is what the access report reads of the configuration c (docs/spec/rbac.md §14,
// #1279): serve's GET /api/v1/access-report and lajstrom admin access-report both take it from
// here, so that the same rows at the same time are the same document from either.
func accessConfigOf(c *config.Config) api.AccessConfig {
	return api.AccessConfig{AdminMFA: c.Auth.Admin.RequireMFA, AnonymousRead: c.Auth.AnonymousRead,
		InactiveAfter: c.Auth.DisableInactiveUsersAfter, Lockout: lockoutOf(c.Auth), DeployMaxTTL: c.Auth.DeployTokens.MaxTTL,
		ServiceMaxTTL: c.Auth.ServiceAccounts.MaxTokenTTL, CIPolicies: c.Auth.CI.Policies}
}

// readAccess reads the rows of the access report on r; a variable for the tests, which fix the
// time of the read.
var readAccess = func(ctx context.Context, r index.Reader) (*index.Access, error) { return r.ReadAccess(ctx) }

// The formats of admin access-report.
const (
	accessJSON = "json"
	accessCSV  = "csv"
)

// newAdminAccessReportCmd returns admin access-report: the access report of the database, as GET
// /api/v1/access-report answers it, and its sha256 (docs/spec/rbac.md §14). It reads the
// database without HTTP, as every lajstrom admin command does, so that it works under the admin
// mode and while the IdP is down; it writes nothing, no audit row either, and records no review,
// which is a person's, through the API.
func newAdminAccessReportCmd(a *app) *cobra.Command {
	var (
		scope, format string
		unusedDays    int
	)
	cmd := &cobra.Command{
		Use:   "access-report [--scope all|people|systems] [--unused-days <n>] [--format json|csv]",
		Short: "Write who has what as one JSON document, the same bytes as GET /api/v1/access-report, and its sha256",
		Long: "Read from the index, in one read-only transaction at the database's time, every live user with its groups,\n" +
			"bindings and tokens, the groups that have a binding, the cert: and k8s: principals, the deploy tokens, the CI\n" +
			"trust policies and what anonymous reads, each with the flags a reviewer looks for, and write it on standard\n" +
			"output in the canonical form of GET /api/v1/access-report: the same rows at the same time are the same bytes.\n" +
			"Standard error gets sha256:<hex> of the document, which POST /api/v1/access-reviews records. --format csv\n" +
			"writes a table for a spreadsheet instead, one line per binding, token and grant, and the sha256 of the JSON\n" +
			"document of the same read. --scope people keeps the local and OIDC users and the groups (PCI DSS 7.2.4),\n" +
			"systems the service accounts, workloads, deploy tokens and CI trust policies (7.2.5.1); --unused-days, 90 by\n" +
			"default, is when a user or a token is unused. It writes nothing, and records no review.",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			q := api.AccessQuery{Scope: scope, UnusedDays: unusedDays}
			if qerr := q.Check(); qerr != nil {
				return usageError("--%s", flagOf(qerr.Error()))
			}
			if !slices.Contains([]string{accessJSON, accessCSV}, format) {
				return usageError("--format %q: json or csv", format)
			}
			ctx := cmd.Context()
			st, err := a.openMigrated(ctx) // compares the clocks, as every command against the database
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, st.Close()) }()
			rows, err := readAccess(ctx, st)
			if err != nil {
				return fmt.Errorf("read the access report: %w", err)
			}
			rep := api.NewAccessReport(rows, accessConfigOf(a.cfg), q)
			doc, err := rep.JSON()
			if err != nil {
				return err
			}
			if format == accessCSV {
				err = rep.WriteCSV(cmd.OutOrStdout())
			} else {
				_, err = cmd.OutOrStdout().Write(doc)
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.ErrOrStderr(), api.AccessDigest(doc))
			return err
		},
	}
	cmd.Flags().StringVar(&scope, "scope", auth.AccessAll, "all, people (local and OIDC users, groups) or systems "+
		"(service accounts, workloads, deploy tokens, CI trust policies)")
	cmd.Flags().IntVar(&unusedDays, "unused-days", api.DefaultUnusedDays, fmt.Sprintf("a user or a token without a use for more "+
		"days than this is unused, %d to %d", api.MinUnusedDays, api.MaxUnusedDays))
	cmd.Flags().StringVar(&format, "format", accessJSON, "json, the document, or csv, a table for a spreadsheet")
	return cmd
}

// flagOf turns a message of api.AccessQuery.Check, which names the field of the API (scope,
// unusedDays), into one that names the flag (scope, unused-days).
func flagOf(msg string) string {
	if rest, ok := strings.CutPrefix(msg, "unusedDays"); ok {
		return "unused-days" + rest
	}
	return msg
}

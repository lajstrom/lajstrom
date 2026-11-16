package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/ci"
	"github.com/lajstrom/lajstrom/internal/index"
)

// newAdminCIPolicyCmd returns admin ci-policy: create, list and delete the CI trust policies of
// the API (docs/spec/rbac.md §11) in the index database, without HTTP and without credentials,
// through ci.Service, so that the rules, the audit rows and the events are those of
// /api/v1/ci/policies. The issuers and the policies of the configuration are read from the
// configuration, whose policies list shows and never changes. A change counts from the next
// exchange on, on every replica. It is made by the anonymous principal with the source cli,
// which is no sign-in: a policy that grants write is made, renewed and approved in a web UI, or
// declared in the configuration; the command line deletes one.
func newAdminCIPolicyCmd(a *app) *cobra.Command {
	c := &cobra.Command{
		Use:   "ci-policy",
		Short: "Manage the CI trust policies of the API that read: which CI jobs get a CI token with which grants",
	}
	c.AddCommand(newCIPolicyCreateCmd(a), newCIPolicyListCmd(a), newCIPolicyDeleteCmd(a))
	return c
}

// newCIPolicyCreateCmd returns admin ci-policy create: make a policy and print it as JSON on one
// line. One that grants write is refused: the command line is no fresh sign-in.
func newCIPolicyCreateCmd(a *app) *cobra.Command {
	var name, issuer string
	var claims, grants []string
	cmd := &cobra.Command{
		Use:         "create --name <name> --issuer <issuer> --claim <claim>=<value> ... --grant <role>:<mask> ...",
		Short:       "Make a CI trust policy of the API that reads, and print it as JSON",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := ci.Policy{Name: name, Issuer: issuer, Claims: map[string][]string{}}
			for _, c := range claims {
				k, v, ok := strings.Cut(c, "=")
				if !ok || k == "" {
					return fmt.Errorf("--claim %q: want <claim>=<value>", c)
				}
				p.Claims[k] = append(p.Claims[k], v)
			}
			for _, g := range grants {
				role, mask, ok := strings.Cut(g, ":")
				if !ok {
					return fmt.Errorf("--grant %q: want <role>:<mask>", g)
				}
				p.Grants = append(p.Grants, index.CIGrant{Role: role, Mask: mask})
			}
			return a.withCIPolicies(cmd.Context(), func(ctx context.Context, s *ci.Service) error {
				made, err := s.Create(ctx, p, auth.Origin(cliOrigin()))
				if err != nil {
					return err
				}
				return writeJSONLine(cmd.OutOrStdout(), made)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "the policy's name: lowercase letters and digits with single hyphens")
	f.StringVar(&issuer, "issuer", "", "the name of an issuer of auth.ci.issuers")
	f.StringArrayVar(&claims, "claim", nil, "a claim and one of its values, repeated: repository_id=123, ref=refs/tags/v*")
	f.StringArrayVar(&grants, "grant", nil, "a role on a mask, repeated: reader:oci/*; writer and maintainer need a web UI's "+
		"sign-in with a second factor, or the configuration")
	for _, n := range []string{"name", "issuer", "claim", "grant"} {
		_ = cmd.MarkFlagRequired(n) // fails only for a flag that does not exist
	}
	return cmd
}

// newCIPolicyListCmd returns admin ci-policy list: the policies of the configuration and of the
// API, as a table.
func newCIPolicyListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List the CI trust policies, of the configuration and of the API",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withCIPolicies(cmd.Context(), func(ctx context.Context, s *ci.Service) error {
				list, err := s.List(ctx)
				if err != nil {
					return err
				}
				return writeCIPolicyTable(cmd.OutOrStdout(), list)
			})
		},
	}
}

// newCIPolicyDeleteCmd returns admin ci-policy delete: the policy grants nothing from the next
// request on, one that writes too; one of the configuration is refused.
func newCIPolicyDeleteCmd(a *app) *cobra.Command {
	var etag string
	cmd := &cobra.Command{
		Use:         "delete <name> [--if-match <etag>]",
		Short:       "Delete a CI trust policy of the API: its CI tokens grant nothing from the next request on",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withCIPolicies(cmd.Context(), func(ctx context.Context, s *ci.Service) error {
				ctx, err := ifMatch(ctx, etag, api.TableCIPolicies, "CI trust policy "+args[0])
				if err != nil {
					return err
				}
				return ifMatched(s.Delete(ctx, args[0], auth.Origin(cliOrigin())))
			})
		},
	}
	ifMatchFlag(cmd, &etag)
	return cmd
}

// withCIPolicies opens the index database of the configuration, runs fn with the service of the
// CI trust policies over it and auth.ci, whose events go to the outbox of the webhooks, and
// closes the database.
func (a *app) withCIPolicies(ctx context.Context, fn func(context.Context, *ci.Service) error) (err error) {
	st, err := a.openMigrated(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	return fn(ctx, newCIPolicies(a.cfg, st))
}

// writeCIPolicyTable prints policies as a table with the columns NAME, SOURCE, STATE, EXPIRES,
// ISSUER, CLAIMS and GRANTS; EXPIRES is - for a policy that does not expire.
func writeCIPolicyTable(out io.Writer, policies []ci.Policy) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	_, _ = io.WriteString(tw, "NAME\tSOURCE\tSTATE\tEXPIRES\tISSUER\tCLAIMS\tGRANTS\n") // a bytes.Buffer does not fail
	for _, p := range policies {
		var claims, grants []string
		for _, c := range slices.Sorted(maps.Keys(p.Claims)) {
			claims = append(claims, c+"="+strings.Join(p.Claims[c], "|"))
		}
		for _, g := range p.Grants {
			grants = append(grants, g.Role+":"+g.Mask)
		}
		expires := "-"
		if p.ExpiresAt != nil {
			expires = p.ExpiresAt.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, p.Source, p.State, expires, p.Issuer, strings.Join(claims, " "),
			strings.Join(grants, " "))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := out.Write(buf.Bytes())
	return err
}

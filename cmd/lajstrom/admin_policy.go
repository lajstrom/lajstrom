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

// newAdminPolicyCmd returns admin policy: the policy rules of docs/spec/policy.md, against the
// database, as the admin role through the API.
func newAdminPolicyCmd(a *app) *cobra.Command {
	p := &cobra.Command{
		Use:   "policy",
		Short: "Manage the policy rules: the versions a repo refuses or admits",
	}
	rule := &cobra.Command{Use: "rule", Short: "Create, list and delete policy rules"}
	rule.AddCommand(newPolicyRuleCreateCmd(a), newPolicyRuleListCmd(a), newPolicyRuleDeleteCmd(a))
	p.AddCommand(rule)
	return p
}

// newPolicyRuleCreateCmd returns admin policy rule create: print the rule as JSON on one line.
func newPolicyRuleCreateCmd(a *app) *cobra.Command {
	var req api.RuleRequest
	var version, variant string
	var expires time.Duration
	cmd := &cobra.Command{
		Use:         "create --mask <format>/<repo> --name <pattern> --reason <text> [--version <constraint>] [--variant <pattern>] [--allow] [--expires <duration>]",
		Short:       "Create a policy rule, and print it",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f := cmd.Flags()
			if f.Changed("version") {
				req.Version = &version
			}
			if f.Changed("variant") {
				req.Variant = &variant
			}
			if allow, _ := f.GetBool("allow"); allow {
				req.Effect = "allow"
			}
			if expires > 0 {
				t := time.Now().Add(expires).UTC().Truncate(time.Second)
				req.ExpiresAt = &t
			}
			return a.withRules(cmd.Context(), func(ctx context.Context, s *api.Rules) error {
				r, err := s.Create(ctx, req, auth.PrincipalFrom(ctx).String(), cliOrigin())
				if err != nil {
					return err
				}
				return writeJSONLine(cmd.OutOrStdout(), r)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.RepoMask, "mask", "", "which repos: pypi/proxy-pypi, npm/* (a format), */payments, */* (all)")
	f.StringVar(&req.Name, "name", "", "the package name, * any run of characters: requests, @evil/*")
	f.StringVar(&req.Reason, "reason", "", "why, shown to clients")
	f.StringVar(&version, "version", "*", "the versions: ==2.32.0, ==2.32.*, >=1.0,<2.0 (a range on a format that orders its versions)")
	f.StringVar(&variant, "variant", "*", "the variant, * any; empty for the empty variant only")
	f.Bool("allow", false, "an exception to the deny rules and to quarantine instead of a deny")
	f.DurationVar(&expires, "expires", 0, "how long it holds, 72h; 0: until it is deleted")
	for _, name := range []string{"mask", "name", "reason"} {
		_ = cmd.MarkFlagRequired(name) // fails only for a flag that does not exist
	}
	return cmd
}

// newPolicyRuleListCmd returns admin policy rule list: the rules as a table.
func newPolicyRuleListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List the policy rules",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withRules(cmd.Context(), func(ctx context.Context, s *api.Rules) error {
				rules, err := s.List(ctx, nil)
				if err != nil {
					return err
				}
				return writeRuleTable(cmd.OutOrStdout(), rules)
			})
		},
	}
}

// newPolicyRuleDeleteCmd returns admin policy rule delete.
func newPolicyRuleDeleteCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "delete <id>",
		Short:       "Delete a policy rule",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("rule %q: want its number", args[0])
			}
			return a.withRules(cmd.Context(), func(ctx context.Context, s *api.Rules) error {
				return s.Delete(ctx, id, auth.PrincipalFrom(ctx).String(), cliOrigin(), nil)
			})
		},
	}
}

// withRules runs fn with the rules service over the migrated database of the configuration.
func (a *app) withRules(ctx context.Context, fn func(context.Context, *api.Rules) error) (err error) {
	st, err := a.openMigrated(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	return fn(ctx, newRules(a.cfg, st, a.log))
}

// writeRuleTable prints rules as a table: ID, MASK, NAME, VERSION, VARIANT, EFFECT, EXPIRES, REASON.
func writeRuleTable(out io.Writer, rules []api.Rule) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	_, _ = io.WriteString(tw, "ID\tMASK\tNAME\tVERSION\tVARIANT\tEFFECT\tEXPIRES\tREASON\n") // a bytes.Buffer does not fail
	for _, r := range rules {
		expires := "never"
		if r.ExpiresAt != nil {
			expires = r.ExpiresAt.UTC().Format(time.RFC3339)
			if r.Expired {
				expires += " (expired)"
			}
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%q\t%s\t%s\t%s\n", r.ID, r.RepoMask, r.Name, r.Version, r.Variant, r.Effect, expires, r.Reason)
	}
	_ = tw.Flush()
	_, err := out.Write(buf.Bytes())
	return err
}

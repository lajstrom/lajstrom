package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
)

// newAdminTokenCmd returns admin token: create, list and revoke the personal access tokens of
// a user (--user), the deploy tokens of a repo (--repo) and the deploy tokens over a mask
// (--mask, docs/spec/rbac.md §1) in the index database, without HTTP,
// through the tokens service (auth.Tokens), so that the validation and the audit events are
// those of the tokens API. A change is made by the anonymous principal with the source cli.
// Like admin user it assumes that lajstrom migrate ran.
func newAdminTokenCmd(a *app) *cobra.Command {
	tok := &cobra.Command{
		Use:   "token",
		Short: "Manage personal access tokens and deploy tokens",
	}
	tok.AddCommand(newTokenCreateCmd(a), newTokenListCmd(a), newTokenRevokeCmd(a))
	return tok
}

// ownerFlags adds --user, --repo and --mask to cmd, exactly one of them required: the kind of
// the token follows from it, a personal access token of the user, a deploy token of the repo,
// or a deploy token over the mask.
func ownerFlags(cmd *cobra.Command, user, repo, mask *string) {
	cmd.Flags().StringVar(user, "user", "", "the user of personal access tokens")
	cmd.Flags().StringVar(repo, "repo", "", "the repo of deploy tokens, <format>/<name>")
	cmd.Flags().StringVar(mask, "mask", "", "the mask of deploy tokens over a mask, <format>/<repo> with * for a whole segment: oci/*, */talos; "+
		"it matches the repos created later too")
	cmd.MarkFlagsMutuallyExclusive("user", "repo", "mask")
	cmd.MarkFlagsOneRequired("user", "repo", "mask")
}

// newTokenCreateCmd returns admin token create: create a token and print it as JSON on one line
// with its secret, the one time the secret is shown.
func newTokenCreateCmd(a *app) *cobra.Command {
	var user, repo, mask, name, scopes, expires string
	cmd := &cobra.Command{
		Use: "create (--user <name> | --repo <format>/<name> | --mask <format>/<repo>) --name <label> [--scopes read,write] [--expires 90d|<RFC 3339>]",
		Short: "Create a personal access token of a user, a deploy token of a repo or a deploy token over a mask, and print it as JSON " +
			"with its secret; the secret is shown this once",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exp, err := parseExpires(expires)
			if err != nil {
				return err
			}
			var list []string
			if scopes != "" {
				list = strings.Split(scopes, ",")
			}
			return a.withTokens(cmd.Context(), func(ctx context.Context, tokens *auth.Tokens) error {
				o := auth.Origin(cliOrigin())
				var (
					t      *auth.Token
					secret string
					err    error
				)
				switch {
				case user != "":
					t, secret, err = tokens.CreatePAT(ctx, user, name, list, exp, o)
				case mask != "":
					t, secret, err = tokens.CreateMaskDeploy(ctx, mask, name, list, exp, o)
				default:
					t, secret, err = tokens.CreateDeploy(ctx, repo, name, list, exp, o)
				}
				if err != nil {
					return err
				}
				return writeJSONLine(cmd.OutOrStdout(), auth.CreatedToken{Token: *t, Secret: secret})
			})
		},
	}
	ownerFlags(cmd, &user, &repo, &mask)
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "a label for the token, unique among the owner's live tokens, at most 64 bytes")
	f.StringVar(&scopes, "scopes", "", "read, write or read,write; required for a deploy token; none for a personal access token is its user's full rights")
	f.StringVar(&expires, "expires", "", "a lifetime (30d, 12h) or an RFC 3339 time; a personal access token defaults to 90d and lives at most 365d, a deploy token without it never expires")
	_ = cmd.MarkFlagRequired("name") // fails only for a flag that does not exist
	return cmd
}

// parseExpires reads --expires: an RFC 3339 time, else a lifetime (auth.ParseLifetime); empty
// is the default of the kind.
func parseExpires(s string) (auth.Expiry, error) {
	if s == "" {
		return auth.Expiry{}, nil
	}
	if at, err := time.Parse(time.RFC3339, s); err == nil {
		return auth.Expiry{At: at}, nil
	}
	d, err := auth.ParseLifetime(s)
	if err != nil {
		return auth.Expiry{}, fmt.Errorf("--expires: %w, or an RFC 3339 time", err)
	}
	return auth.Expiry{In: d}, nil
}

// newTokenListCmd returns admin token list: the tokens of a user, of a repo or over a mask (as
// it was written, not matched) as a table, the revoked and expired ones included, never a
// secret.
func newTokenListCmd(a *app) *cobra.Command {
	var user, repo, mask string
	cmd := &cobra.Command{
		Use:         "list (--user <name> | --repo <format>/<name> | --mask <format>/<repo>)",
		Short:       "List the personal access tokens of a user, the deploy tokens of a repo or the deploy tokens over a mask",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withTokens(cmd.Context(), func(ctx context.Context, tokens *auth.Tokens) error {
				var (
					list []auth.Token
					err  error
				)
				switch {
				case user != "":
					list, err = tokens.List(ctx, user)
				case mask != "":
					list, err = tokens.ListMaskDeploy(ctx, mask)
				default:
					list, err = tokens.ListDeploy(ctx, repo)
				}
				if err != nil {
					return err
				}
				return writeTokenTable(cmd.OutOrStdout(), list)
			})
		},
	}
	ownerFlags(cmd, &user, &repo, &mask)
	return cmd
}

// newTokenRevokeCmd returns admin token revoke: the token is refused from the next request on,
// on every replica.
func newTokenRevokeCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "revoke <id>",
		Short:       "Revoke a token: it is refused from the next request on",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return fmt.Errorf("token id %q: want the number of the ID column of admin token list", args[0])
			}
			return a.withTokens(cmd.Context(), func(ctx context.Context, tokens *auth.Tokens) error {
				return tokens.Revoke(ctx, id, auth.Origin(cliOrigin()))
			})
		},
	}
}

// withTokens opens the index database of the configuration, runs fn with the tokens service
// over it, and closes the database.
func (a *app) withTokens(ctx context.Context, fn func(context.Context, *auth.Tokens) error) (err error) {
	st, err := a.openMigrated(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	return fn(ctx, &auth.Tokens{Index: st, Formats: api.RepoFormat, Log: a.log})
}

// writeTokenTable prints tokens as a table with the columns ID, KIND, NAME, SCOPES, EXPIRES,
// LAST USED, REVOKED and HASH (the first characters of the secret's sha256).
func writeTokenTable(out io.Writer, tokens []auth.Token) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	_, _ = io.WriteString(tw, "ID\tKIND\tNAME\tSCOPES\tEXPIRES\tLAST USED\tREVOKED\tHASH\n") // a bytes.Buffer does not fail
	for _, t := range tokens {
		scopes := strings.Join(t.Scopes, ",")
		if scopes == "" {
			scopes = "all" // a personal access token without scopes: its user's full rights
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Kind, t.Name, scopes,
			stampOr(t.ExpiresAt, "never"), stampOr(t.LastUsedAt, "never"), stampOr(t.RevokedAt, "-"), t.HashPrefix)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := out.Write(buf.Bytes())
	return err
}

// stampOr returns t in RFC 3339, or none when t is nil.
func stampOr(t *time.Time, none string) string {
	if t == nil {
		return none
	}
	return t.Format(time.RFC3339)
}

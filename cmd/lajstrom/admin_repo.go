package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// newAdminRepoCmd returns admin repo: create, list and delete repos in the index database,
// without HTTP, through the service of /api/v1/repos (api.Repos), so that the validation and
// the audit events are the API's. A change is made by the anonymous principal with the source
// cli. admin assumes that lajstrom migrate ran: it does not migrate, and a database without the
// schema is the store's error.
func newAdminRepoCmd(a *app) *cobra.Command {
	repo := &cobra.Command{
		Use:   "repo",
		Short: "Manage repos",
	}
	repo.AddCommand(newRepoCreateCmd(a), newRepoListCmd(a), newRepoDeleteCmd(a))
	return repo
}

// newRepoCreateCmd returns admin repo create: create a repo and print it as JSON on one line.
func newRepoCreateCmd(a *app) *cobra.Command {
	var (
		req                     api.CreateRequest
		policy, upstream, group string
	)
	cmd := &cobra.Command{
		Use:         "create <format>/<name>",
		Short:       "Create a repo and print it as JSON",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			if req.Format, req.Name, err = splitRepo(args[0]); err != nil {
				return err
			}
			req.Policy, req.Upstream, req.Group = json.RawMessage(policy), json.RawMessage(upstream), json.RawMessage(group)
			return a.withRepos(cmd.Context(), func(ctx context.Context, repos *api.Repos) error {
				repo, err := repos.Create(ctx, req, cliOrigin())
				if err != nil {
					return err
				}
				return writeJSONLine(cmd.OutOrStdout(), repo)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.Registry, "registry", "", "registry that stores the repo's artifacts, one of registries[].name; none for a group")
	f.BoolVar(&req.AnonymousRead, "anonymous-read", false, "readable without credentials, when auth.anonymousRead allows it")
	f.StringVar(&policy, "policy", "", `repo policy, a JSON object, e.g. '{"maxFileSize": 1048576}'`)
	f.StringVar(&req.Kind, "kind", "", "hosted (the default), proxy, a repo that serves an upstream of the configuration, or group")
	f.StringVar(&upstream, "upstream", "", `a proxy's settings, a JSON object, e.g. '{"name": "npmjs", "deny": ["@acme/*"]}'`)
	f.StringVar(&group, "group", "", `a group's settings, a JSON object, e.g. '{"members": ["internal", "npmjs"]}'`)
	return cmd
}

// newRepoListCmd returns admin repo list: the repos as a table, or as {"repos": […]} with --json.
func newRepoListCmd(a *app) *cobra.Command {
	var (
		formatName string
		asJSON     bool
	)
	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List the repos",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withRepos(cmd.Context(), func(ctx context.Context, repos *api.Repos) error {
				list, err := repos.List(ctx, formatName)
				if err != nil {
					return err
				}
				if asJSON {
					return writeJSONLine(cmd.OutOrStdout(), api.RepoList{Repos: list})
				}
				return writeRepoTable(cmd.OutOrStdout(), list)
			})
		},
	}
	cmd.Flags().StringVar(&formatName, "format", "", "only the repos of this format")
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"repos": […]} as JSON instead of a table`)
	return cmd
}

// newRepoDeleteCmd returns admin repo delete: delete a repo; one with versions only with
// --force, which removes its index rows and leaves the registry untouched.
func newRepoDeleteCmd(a *app) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:         "delete <format>/<name>",
		Short:       "Delete a repo; one that has versions only with --force",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			formatName, name, err := splitRepo(args[0])
			if err != nil {
				return err
			}
			return a.withRepos(cmd.Context(), func(ctx context.Context, repos *api.Repos) error {
				return repos.Delete(ctx, formatName, name, force, cliOrigin())
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"delete a repo that has versions: its index rows go with it, the artifacts stay in the registry")
	return cmd
}

// withRepos opens the index database of the configuration, runs fn with the repo service over
// it, and closes the database.
func (a *app) withRepos(ctx context.Context, fn func(context.Context, *api.Repos) error) (err error) {
	st, err := openStore(ctx, a.cfg.Database)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	return fn(ctx, newRepos(a.cfg, st, a.log))
}

// newRepos returns the repo service over st for serve and admin repo: the formats are the
// plugins compiled into the binary (format.Lookup, the default of api.Repos), the registries
// and the upstreams those of the configuration.
func newRepos(c *config.Config, st index.Store, log *slog.Logger) *api.Repos {
	registries := make(map[string]bool, len(c.Registries))
	for _, r := range c.Registries {
		registries[r.Name] = true
	}
	upstreams := make(map[string]string, len(c.Upstreams))
	for _, u := range c.Upstreams {
		upstreams[u.Name] = u.Format
	}
	return &api.Repos{
		Index:      st,
		Registries: func(name string) bool { return registries[name] },
		Upstreams: func(name string) (string, bool) {
			f, ok := upstreams[name]
			return f, ok
		},
		Log: log,
	}
}

// splitRepo splits the argument <format>/<name>.
func splitRepo(arg string) (formatName, name string, err error) {
	formatName, name, ok := strings.Cut(arg, "/")
	if !ok || formatName == "" || name == "" {
		return "", "", fmt.Errorf("repo %q: want <format>/<name>, e.g. raw/scratch", arg)
	}
	return formatName, name, nil
}

// cliOrigin is the origin of a change that lajstrom admin makes against the database: the
// source cli and the host name of the machine it runs on, which its audit event carries, so
// that a recovery of a lost admin names where it ran (#440).
func cliOrigin() api.Origin {
	host, _ := os.Hostname()
	return api.Origin{Source: api.SourceCLI, Host: host}
}

// writeJSONLine prints v as JSON on one line.
func writeJSONLine(out io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = out.Write(append(b, '\n'))
	return err
}

// writeRepoTable prints repos as a table with the columns FORMAT, NAME, REGISTRY,
// ANONYMOUS-READ and CREATED; a group, which has no registry, shows "-" there.
func writeRepoTable(out io.Writer, repos []api.Repo) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	_, _ = io.WriteString(tw, "FORMAT\tNAME\tREGISTRY\tANONYMOUS-READ\tCREATED\n") // a bytes.Buffer does not fail
	for _, r := range repos {
		registry := r.Registry
		if registry == "" {
			registry = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n", r.Format, r.Name, registry, r.AnonymousRead, r.CreatedAt.Format(time.RFC3339))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := out.Write(buf.Bytes())
	return err
}

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
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// newAdminWebhookCmd returns admin webhook: the webhooks of docs/spec/webhooks.md in the index
// database, without HTTP and without credentials, through the webhook service, so that the
// rules and the audit rows are those of the API. Its changes are made by the anonymous
// principal with the source cli. Creating a webhook, rotating its secret and testing it need
// the master key of the configuration; a test is sent from this host, under the configuration's
// webhooks.allowedHosts and webhooks.timeout.
func newAdminWebhookCmd(a *app) *cobra.Command {
	w := &cobra.Command{
		Use:   "webhook",
		Short: "Manage the webhooks: where the events of the repos of a mask are sent",
	}
	w.AddCommand(newWebhookCreateCmd(a), newWebhookListCmd(a), newWebhookUpdateCmd(a), newWebhookDeleteCmd(a),
		newWebhookRotateCmd(a), newWebhookTestCmd(a), newWebhookDeliveriesCmd(a))
	return w
}

// newWebhookCreateCmd returns admin webhook create: print the webhook and its secret, once, as
// JSON on one line.
func newWebhookCreateCmd(a *app) *cobra.Command {
	var mask, url, description string
	var events []string
	var inactive bool
	cmd := &cobra.Command{
		Use:         "create --mask <format>/<repo> --url https://… [--events version.published,…] [--description …] [--inactive]",
		Short:       "Create a webhook, and print it with its secret, which is shown this once",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			active := !inactive
			req := webhook.Request{Mask: mask, URL: &url, Active: &active, Description: &description}
			if cmd.Flags().Changed("events") {
				req.Events = &events
			}
			return a.withWebhooks(cmd.Context(), func(ctx context.Context, s *webhook.Service) error {
				h, secret, err := s.Create(ctx, req, auth.Origin(cliOrigin()))
				if err != nil {
					return err
				}
				return writeJSONLine(cmd.OutOrStdout(), api.CreatedWebhook{Webhook: api.WebhookOf(h), Secret: secret})
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&mask, "mask", "", "which repos: apt/ubuntu, apt/* (a format), */payments, */* (all)")
	f.StringVar(&url, "url", "", "where to send: https, or http to a host of webhooks.allowedHosts")
	f.StringSliceVar(&events, "events", nil, "the types: version.published, version.deleted, repo.created, repo.deleted; none: every type")
	f.StringVar(&description, "description", "", "a note, at most 256 bytes")
	f.BoolVar(&inactive, "inactive", false, "create it inactive: it gets no event until updated with --active")
	for _, name := range []string{"mask", "url"} {
		_ = cmd.MarkFlagRequired(name) // fails only for a flag that does not exist
	}
	return cmd
}

// newWebhookListCmd returns admin webhook list: the webhooks, or those of one mask, as a table.
func newWebhookListCmd(a *app) *cobra.Command {
	var mask string
	cmd := &cobra.Command{
		Use:         "list [--mask <format>/<repo>]",
		Short:       "List the webhooks, or those of one mask",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withWebhooks(cmd.Context(), func(ctx context.Context, s *webhook.Service) error {
				list, err := s.List(ctx, mask)
				if err != nil {
					return err
				}
				return writeWebhookTable(cmd.OutOrStdout(), list)
			})
		},
	}
	cmd.Flags().StringVar(&mask, "mask", "", "only the webhooks of this mask, exactly")
	return cmd
}

// newWebhookUpdateCmd returns admin webhook update: change the flags given.
func newWebhookUpdateCmd(a *app) *cobra.Command {
	var url, description string
	var events []string
	var active bool
	cmd := &cobra.Command{
		Use:         "update <id> [--url …] [--events …] [--active=true|false] [--description …]",
		Short:       "Change a webhook's URL, events, active or description, and print it",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := webhookID(args[0])
			if err != nil {
				return err
			}
			var req webhook.Request
			f := cmd.Flags()
			if f.Changed("url") {
				req.URL = &url
			}
			if f.Changed("events") {
				req.Events = &events
			}
			if f.Changed("active") {
				req.Active = &active
			}
			if f.Changed("description") {
				req.Description = &description
			}
			return a.withWebhooks(cmd.Context(), func(ctx context.Context, s *webhook.Service) error {
				h, err := s.Update(ctx, id, req, auth.Origin(cliOrigin()))
				if err != nil {
					return err
				}
				return writeJSONLine(cmd.OutOrStdout(), api.WebhookOf(h))
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&url, "url", "", "the new URL")
	f.StringSliceVar(&events, "events", nil, "the new types; an empty value: every type")
	f.BoolVar(&active, "active", true, "whether it gets new events")
	f.StringVar(&description, "description", "", "the new note")
	return cmd
}

// newWebhookDeleteCmd returns admin webhook delete: the webhook and its delivery log go.
func newWebhookDeleteCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "delete <id>",
		Short:       "Delete a webhook and its delivery log",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := webhookID(args[0])
			if err != nil {
				return err
			}
			return a.withWebhooks(cmd.Context(), func(ctx context.Context, s *webhook.Service) error {
				return s.Delete(ctx, id, auth.Origin(cliOrigin()))
			})
		},
	}
}

// newWebhookRotateCmd returns admin webhook rotate: print the new secret, once.
func newWebhookRotateCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "rotate <id>",
		Short:       "Give a webhook a new secret and print it, once; the old one signs nothing more",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := webhookID(args[0])
			if err != nil {
				return err
			}
			return a.withWebhooks(cmd.Context(), func(ctx context.Context, s *webhook.Service) error {
				secret, err := s.Rotate(ctx, id, auth.Origin(cliOrigin()))
				if err != nil {
					return err
				}
				return writeJSONLine(cmd.OutOrStdout(), api.Secret{Secret: secret})
			})
		},
	}
}

// newWebhookTestCmd returns admin webhook test: send a ping from this host and print its row of
// the delivery log; exit status 1 when it was not delivered.
func newWebhookTestCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "test <id>",
		Short:       "Send a webhook.ping now from this host, and print the outcome",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := webhookID(args[0])
			if err != nil {
				return err
			}
			return a.withWebhooks(cmd.Context(), func(ctx context.Context, s *webhook.Service) error {
				d, err := s.Test(ctx, id, "")
				if err != nil {
					return err
				}
				if err := writeJSONLine(cmd.OutOrStdout(), api.DeliveryOf(d)); err != nil {
					return err
				}
				if d.State != index.DeliveryDelivered {
					return fmt.Errorf("webhook %d: not delivered: %s", id, d.LastError)
				}
				return nil
			})
		},
	}
}

// newWebhookDeliveriesCmd returns admin webhook deliveries: the newest rows of the delivery log.
func newWebhookDeliveriesCmd(a *app) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:         "deliveries <id> [--limit 20]",
		Short:       "Print the newest rows of a webhook's delivery log",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := webhookID(args[0])
			if err != nil {
				return err
			}
			return a.withWebhooks(cmd.Context(), func(ctx context.Context, s *webhook.Service) error {
				ds, err := s.Deliveries(ctx, id, 0, limit)
				if err != nil {
					return err
				}
				return writeDeliveryTable(cmd.OutOrStdout(), ds)
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "how many rows, newest first")
	return cmd
}

// webhookID parses the id argument of a webhook.
func webhookID(arg string) (int64, error) {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("webhook id %q: want the number of the ID column of admin webhook list", arg)
	}
	return id, nil
}

// withWebhooks opens the index database of the configuration, runs fn with the webhook service
// over it, with the master key when one is configured, and closes the database.
func (a *app) withWebhooks(ctx context.Context, fn func(context.Context, *webhook.Service) error) (err error) {
	m, err := loadMaster(a.cfg.Keys, a.lookupEnv, a.log)
	if err != nil && !errors.Is(err, keys.ErrNoMasterKey) {
		return err
	}
	st, err := a.openMigrated(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	return fn(ctx, &webhook.Service{Index: st, Deliverer: newDeliverer(a.cfg, a.log), Master: m, Formats: api.RepoFormat,
		Replica: "cli:" + cliOrigin().Host, Source: a.cfg.Server.ExternalURL, Log: a.log})
}

// writeWebhookTable prints webhooks as a table: ID, MASK, ACTIVE, EVENTS, URL host, CREATED.
func writeWebhookTable(out io.Writer, hooks []index.Webhook) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	_, _ = io.WriteString(tw, "ID\tMASK\tACTIVE\tEVENTS\tURL\tCREATED\n") // a bytes.Buffer does not fail
	for _, h := range hooks {
		events := strings.Join(h.Events, ",")
		if events == "" {
			events = "*"
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%t\t%s\t%s\t%s\n", h.ID, h.Mask, h.Active, events, h.URL, h.CreatedAt.Format(time.RFC3339))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := out.Write(buf.Bytes())
	return err
}

// writeDeliveryTable prints rows of a delivery log as a table.
func writeDeliveryTable(out io.Writer, ds []index.WebhookDelivery) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	_, _ = io.WriteString(tw, "ID\tEVENT\tTYPE\tSTATE\tATTEMPTS\tSTATUS\tERROR\tCREATED\n")
	for _, d := range ds {
		status := "-"
		if d.LastStatus != 0 {
			status = strconv.Itoa(d.LastStatus)
		}
		errText := d.LastError
		if errText == "" {
			errText = "-"
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", d.ID, d.EventID, d.EventType, d.State, d.Attempts, status, errText,
			d.CreatedAt.Format(time.RFC3339))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := out.Write(buf.Bytes())
	return err
}

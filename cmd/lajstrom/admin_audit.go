package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// actionAuditPrune is the audit action of a prune of the audit log, subject audit
// (docs/spec/rbac.md §5, A prune; #1278).
const actionAuditPrune = "audit.prune"

// The floors of a prune's cut-off, before the database's time: pruneFloor without --force,
// pruneForceFloor with it and always (docs/spec/rbac.md §5; PCI DSS 10.5.1 asks for 12 months,
// the last three immediately available).
const (
	pruneFloor      = 365 * 24 * time.Hour
	pruneForceFloor = 90 * 24 * time.Hour
)

// pruneBatch is how many rows a prune deletes in one transaction, so that no lock is held long;
// a variable for the tests.
var pruneBatch = 10_000

// newAdminAuditCmd returns admin audit: the audit log of the index.
func newAdminAuditCmd(a *app) *cobra.Command {
	audit := &cobra.Command{
		Use:   "audit",
		Short: "The audit log of the index: prune its oldest rows",
	}
	audit.AddCommand(newAuditPruneCmd(a))
	return audit
}

// pruneReport is what admin audit prune prints, one line of JSON: the cut-off, what it counted
// to delete, --force and --dry-run, and what it deleted.
type pruneReport struct {
	Before  time.Time  `json:"before"`
	Rows    int        `json:"rows"`
	FirstID int64      `json:"firstId,omitempty"`
	LastID  int64      `json:"lastId,omitempty"`
	FirstAt *time.Time `json:"firstAt,omitempty"`
	LastAt  *time.Time `json:"lastAt,omitempty"`
	Force   bool       `json:"force"`
	DryRun  bool       `json:"dryRun,omitempty"`
	Deleted *int       `json:"deleted,omitempty"`
}

// details are the details of the prune's audit row, with the origin o of the command line:
// before, rows, firstId, lastId, firstAt and lastAt (the last four when it counted rows), force,
// and deleted once it is known.
func (p pruneReport) details(o api.Origin) json.RawMessage {
	d := map[string]any{"before": p.Before, "rows": p.Rows, "force": p.Force}
	if p.Rows > 0 {
		d["firstId"], d["lastId"], d["firstAt"], d["lastAt"] = p.FirstID, p.LastID, p.FirstAt, p.LastAt
	}
	if p.Deleted != nil {
		d["deleted"] = *p.Deleted
	}
	raw, _ := json.Marshal(o.AddTo(d)) // times, numbers, strings and a bool: never an error
	return raw
}

// newAuditPruneCmd returns admin audit prune: delete the audit rows before a cut-off, against the
// database (docs/spec/rbac.md §5, A prune; #1278).
func newAuditPruneCmd(a *app) *cobra.Command {
	var (
		before        string
		force, dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "prune --before <time>|<n>d [--force] [--dry-run]",
		Short: "Delete the audit rows older than a cut-off, a year old at the least, and print what it deleted as JSON",
		Long: "Delete from the index the audit rows whose time is before the cut-off of --before, an RFC 3339 time or an\n" +
			"age in days (400d), counted on the database's clock. A cut-off less than 365 days old is refused, exit\n" +
			"status 2, unless --force, which goes down to 90 days and no further. A row still pending and the push of a\n" +
			"referrer, which reindex reads, are never deleted. It counts the rows first and deletes up to the last of\n" +
			"them, 10000 at a time, each batch a transaction of its own, between its own audit row audit.prune, pending\n" +
			"and then ok or failed with the rows deleted. --dry-run prints the count and writes nothing. Keep the rows\n" +
			"in the log store (audit.log) or a snapshot before: a deleted row is gone from the database for good.",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			ctx := cmd.Context()
			cut, err := cutOff(before)
			if err != nil {
				return err
			}
			st, err := a.openMigrated(ctx) // compares the clocks: nothing is written past 60 s
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, st.Close()) }()
			now, err := st.DatabaseTime(ctx)
			if err != nil {
				return fmt.Errorf("read the database's clock: %w", err)
			}
			rep := pruneReport{Before: cut(now).UTC(), Force: force, DryRun: dryRun}
			if err := checkFloor(rep.Before, now, force); err != nil {
				return err
			}
			span, err := st.SpanAudit(ctx, rep.Before)
			if err != nil {
				return err
			}
			rep.Rows, rep.FirstID, rep.LastID = span.Rows, span.FirstID, span.LastID
			if span.Rows > 0 {
				rep.FirstAt, rep.LastAt = &span.FirstAt, &span.LastAt
			}
			if !dryRun {
				err = pruneAudit(ctx, st, &rep, a.cliOrigin())
			}
			return errors.Join(err, writeJSONLine(cmd.OutOrStdout(), rep))
		},
	}
	cmd.Flags().StringVar(&before, "before", "", "the cut-off: an RFC 3339 time, or an age in days such as 400d; required")
	cmd.Flags().BoolVar(&force, "force", false, "take a cut-off down to 90 days old, where 365 is the floor without it")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what it would delete, and write nothing")
	return cmd
}

// cutOff returns the cut-off of --before at the database's time: an RFC 3339 time as it is, or
// an age in days, <n>d, before it (auth.ParseLifetime, as snapshot create --audit reads an age).
// Anything else is a usage error, exit status 2.
func cutOff(s string) (func(now time.Time) time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return func(time.Time) time.Time { return t }, nil
	}
	if strings.HasSuffix(s, "d") {
		if age, err := auth.ParseLifetime(s); err == nil {
			return func(now time.Time) time.Time { return now.Add(-age) }, nil
		}
	}
	if s == "" {
		return nil, usageError("--before: required, an RFC 3339 time such as 2025-11-01T00:00:00Z or an age in days such as 400d")
	}
	return nil, usageError("--before %q: an RFC 3339 time such as 2025-11-01T00:00:00Z, or an age in days such as 400d", s)
}

// checkFloor refuses a cut-off less than pruneFloor before now, the database's time, or less than
// pruneForceFloor with force: a usage error, exit status 2, before anything is written.
func checkFloor(cut, now time.Time, force bool) error {
	floor, why := pruneFloor, "365 days before the database's time at the least, 90 with --force"
	if force {
		floor, why = pruneForceFloor, "90 days before the database's time at the least, also with --force"
	}
	if latest := now.Add(-floor); cut.After(latest) {
		return usageError("--before %s: the cut-off must be %s; the database's time is %s, the latest cut-off %s",
			cut.UTC().Format(time.RFC3339), why, now.UTC().Format(time.RFC3339), latest.UTC().Format(time.RFC3339))
	}
	return nil
}

// pruneAudit deletes the rows that rep counted, pruneBatch at a time and up to rep.LastID, and
// writes its row as a registry write's (docs/spec/rbac.md §5): pending before the first batch, in
// a transaction of its own, so that a prune that stops shows; then ok with the rows deleted, or
// failed with them and the error, also when ctx has ended. It sets rep.Deleted.
func pruneAudit(ctx context.Context, st store, rep *pruneReport, o api.Origin) error {
	ev := index.AuditEvent{Principal: auth.Anonymous.String(), Action: actionAuditPrune, Subject: "audit",
		State: index.AuditPending, Details: rep.details(o)}
	if err := st.AppendAudit(ctx, &ev); err != nil {
		return fmt.Errorf("the audit row of the prune, before it deletes: %w", err)
	}
	deleted := 0
	var err error
	for rep.Rows > 0 {
		var n int
		n, err = st.PruneAudit(ctx, rep.Before, rep.LastID, pruneBatch)
		deleted += n
		if err != nil || n < pruneBatch {
			break
		}
	}
	rep.Deleted = &deleted
	ev.State, ev.Details = index.AuditOK, rep.details(o)
	if err != nil {
		ev.State, ev.Error = index.AuditFailed, err.Error()
		err = fmt.Errorf("the prune stopped after %d rows: %w", deleted, err)
	}
	if cerr := st.CompleteAudit(context.WithoutCancel(ctx), &ev); cerr != nil {
		err = errors.Join(err, fmt.Errorf("the audit row %d of the prune is left pending, unknown after a reindex: %w", ev.ID, cerr))
	}
	return err
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// actionKeyRewrap is the audit action of a rewrap of the master key's values, subject
// master:<id> (docs/spec/rbac.md §5).
const actionKeyRewrap = "key.rewrap"

// rewrapSettle is how long a rewrap waits after the switch of the sealing key, on the database's
// clock, before it seals again: a beat of every replica (replicaBeatEvery), which reads the new
// key, and as much again for a beat that runs late. A variable, which the tests shorten.
var rewrapSettle = 2 * replicaBeatEvery

// masterKeyID is the form of a master key's id: 16 lowercase hexadecimal digits.
var masterKeyID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// rewrapReport is what admin key rewrap prints, one line of JSON: never a key nor a value, a key
// by its id and a value by its row. Sealing is the sealing row as the command read it; Held the
// keys of the command with their sources; Values the values of keys and webhooks under each key
// id, Lost those under none, Sessions the open uploads under each key id. A dry run adds the live
// replicas and, with --to, what a run would refuse; a run adds what it switched, sealed again and
// left, and its audit row.
type rewrapReport struct {
	DryRun   bool                    `json:"dryRun,omitempty"`
	To       string                  `json:"to,omitempty"`
	Sealing  *sealingView            `json:"sealing"`
	Held     []heldView              `json:"held"`
	Replicas []replicaView           `json:"replicas,omitempty"`
	Values   map[string]keyCounts    `json:"values"`
	Lost     []lostValue             `json:"lost,omitempty"`
	Sessions map[string]openSessions `json:"sessions"`
	Refusals []string                `json:"refusals,omitempty"`

	Switched bool                 `json:"switched,omitempty"`
	Resealed *keyCounts           `json:"resealed,omitempty"`
	Left     map[string]keyCounts `json:"left,omitempty"`
	State    string               `json:"state,omitempty"`
	Audit    int64                `json:"audit,omitempty"`
}

// sealingView is the sealing row of a report.
type sealingView struct {
	KeyID  string    `json:"keyId"`
	Source string    `json:"source"`
	SetAt  time.Time `json:"setAt"`
}

// heldView is a key the command holds: its id and its source.
type heldView struct {
	ID     string `json:"id"`
	Source string `json:"source"`
}

// replicaView is a live replica of a dry run: where it runs and the ids of the keys it holds.
type replicaView struct {
	Host        string    `json:"host"`
	PID         int       `json:"pid"`
	Version     string    `json:"version"`
	MasterKeys  []string  `json:"masterKeys"`
	HeartbeatAt time.Time `json:"heartbeatAt"`
}

// newKeyRewrapCmd returns admin key rewrap: make the key of --to the one every process seals
// with, and seal every value of the index sealed under another key again under it
// (docs/spec/index-schema.md, The master key and its replacement, #1288).
func newKeyRewrapCmd(a *app) *cobra.Command {
	var (
		to                string
		dryRun, allowLost bool
	)
	cmd := &cobra.Command{
		Use:   "rewrap --to <id> [--dry-run] [--allow-lost]",
		Short: "Make a master key the one that seals, and seal every value of the index again under it",
		Long: "Name, by its id, the master key that every process seals with, and seal every private key of the index and\n" +
			"every webhook's secret again under it. The key comes from the configuration only (keys.masterKeyDir,\n" +
			"keys.masterKeyFile, keys.masterKeyEnv): this command and every live replica must hold it, which --dry-run\n" +
			"shows with the ids of every key, the values under each and the open upload sessions. A run switches the\n" +
			"sealing key with its audit row key.rewrap, waits a minute for every replica to read it, seals again 100\n" +
			"rows at a time, and counts again: ok when nothing is left under another key. A value under no key this\n" +
			"command holds stops it before it writes, unless --allow-lost leaves such values as they are. Run it again\n" +
			"until it reports nothing left before the old key is removed.",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if err := checkRewrapFlags(to, dryRun); err != nil {
				return err
			}
			ctx := cmd.Context()
			m, err := loadMasters(a.cfg.Keys, a.lookupEnv, a.log)
			if err != nil {
				return fmt.Errorf("admin key rewrap needs the master keys: %w", err)
			}
			st, err := a.openMigrated(ctx) // compares the clocks: nothing is written past 60 s
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, st.Close()) }()
			rep, c, err := a.rewrapCheck(ctx, st, m, to, allowLost)
			if err != nil {
				return err
			}
			if dryRun {
				rep.DryRun = true
				return writeJSONLine(cmd.OutOrStdout(), rep)
			}
			if len(rep.Refusals) > 0 {
				return errors.New(strings.Join(rep.Refusals, "; "))
			}
			return a.rewrapRun(ctx, cmd.OutOrStdout(), st, m, rep, c)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "the id of the master key to seal with, 16 hexadecimal digits, as --dry-run lists them; "+
		"required but with --dry-run")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the keys, the replicas, the values under each key and what a run "+
		"would refuse, and write nothing")
	cmd.Flags().BoolVar(&allowLost, "allow-lost", false, "leave the values that no key of this command opens as they are, "+
		"for a master key that is lost, instead of stopping")
	return cmd
}

// checkRewrapFlags refuses, as a usage error (exit status 2), a run without --to and an id that
// is not 16 lowercase hexadecimal digits.
func checkRewrapFlags(to string, dryRun bool) error {
	switch {
	case to == "" && !dryRun:
		return usageError("--to: required, the id of the master key to seal with; lajstrom admin key rewrap --dry-run lists them")
	case to != "" && !masterKeyID.MatchString(to):
		return usageError("--to %q: the id of a master key is 16 lowercase hexadecimal digits", to)
	}
	return nil
}

// rewrapCheck reads what a rewrap to the key to starts from, and writes nothing: the sealing row,
// the keys of the command, the live replicas, the values under each key (countSealed, to first,
// else the key the row names) and, with to, what a run refuses (rewrapGuard, and the values under
// no key held unless allowLost).
func (a *app) rewrapCheck(ctx context.Context, st store, m *keys.Master, to string, allowLost bool) (*rewrapReport, census, error) {
	row, err := st.GetSealingKey(ctx)
	switch {
	case errors.Is(err, index.ErrNotFound):
		row = nil
	case err != nil:
		return nil, census{}, fmt.Errorf("the sealing key of the index: %w", err)
	}
	rep := &rewrapReport{To: to}
	if row != nil {
		rep.Sealing = &sealingView{KeyID: row.KeyID, Source: row.Source, SetAt: row.SetAt}
		m.SealWith(row.KeyID)
	}
	if to != "" && m.Holds(to) {
		m.SealWith(to) // opens try it first; nothing is sealed before the switch
	}
	for _, k := range m.Held() {
		rep.Held = append(rep.Held, heldView{ID: k.ID, Source: k.Source})
	}
	live, err := liveReplicas(ctx, st)
	if err != nil {
		return nil, census{}, err
	}
	for _, r := range live {
		rep.Replicas = append(rep.Replicas, replicaView{Host: r.Host, PID: r.PID, Version: r.Version, MasterKeys: r.MasterKeys,
			HeartbeatAt: r.HeartbeatAt})
	}
	c, err := countSealed(ctx, st, m)
	if err != nil {
		return nil, census{}, err
	}
	rep.Values, rep.Lost, rep.Sessions = c.Under, c.Lost, c.Sessions
	if to == "" {
		return rep, c, nil
	}
	if rep.Refusals, err = rewrapGuard(ctx, st, m, to); err != nil {
		return nil, census{}, err
	}
	if len(c.Lost) > 0 && !allowLost {
		rows := make([]string, 0, len(c.Lost))
		for _, v := range c.Lost {
			rows = append(rows, v.String())
		}
		rep.Refusals = append(rep.Refusals, fmt.Sprintf("%d values are under no master key this command holds: %s; add the key "+
			"that sealed them to keys.masterKeyDir, or, when it is lost, run with --allow-lost, which leaves them as they are",
			len(c.Lost), strings.Join(rows, ", ")))
	}
	return rep, c, nil
}

// rewrapRun is a rewrap to rep.To that nothing refuses (docs/spec/index-schema.md, The rewrap,
// steps 3 to 6): the switch of the sealing key with its audit row, pending, in one transaction;
// the wait; the batches; the count again, and the row ok or failed. A run that switches nothing and
// finds nothing under another key writes its row ok at once. It prints the report, also when it
// fails after the switch.
func (a *app) rewrapRun(ctx context.Context, out io.Writer, st store, m *keys.Master, rep *rewrapReport, c census) error {
	to, from := rep.To, ""
	if rep.Sealing != nil {
		from = rep.Sealing.KeyID
	}
	o := a.cliOrigin()
	details := map[string]any{"from": from, "to": to, "under": c.other(to), "lost": lostCounts(c.Lost),
		"sessions": otherSessions(c, to)}
	ev := index.AuditEvent{Principal: auth.Anonymous.String(), Action: actionKeyRewrap, Subject: "master:" + to,
		State: index.AuditPending}
	if from == to && len(c.other(to)) == 0 {
		details["resealed"] = keyCounts{}
		ev.State, ev.Details = index.AuditOK, rewrapDetails(details, o)
		if err := st.AppendAudit(ctx, &ev); err != nil {
			return fmt.Errorf("the audit row of the rewrap: %w", err)
		}
		rep.Resealed, rep.State, rep.Audit = &keyCounts{}, ev.State, ev.ID
		return writeJSONLine(out, rep)
	}
	ev.Details = rewrapDetails(details, o)
	setAt, switched, err := switchSealingKey(ctx, st, &ev, from, to)
	if err != nil {
		return err
	}
	rep.Switched, rep.Audit = switched, ev.ID
	resealed, err := a.settleAndReseal(ctx, st, m, to, setAt)
	rep.Resealed = &resealed
	end, cerr := countSealed(ctx, st, m)
	err = errors.Join(err, cerr)
	rep.Left = end.other(to)
	details["resealed"], details["sessions"] = resealed, otherSessions(end, to)
	ev.State = index.AuditOK
	if err == nil && len(rep.Left) > 0 {
		err = fmt.Errorf("values are left under another master key (%s): a process sealed under it after its batch; run the "+
			"rewrap again", strings.Join(sortedIDs(rep.Left), ", "))
	}
	if err != nil {
		ev.State, ev.Error = index.AuditFailed, err.Error()
		details["left"] = rep.Left
	}
	ev.Details, rep.State = rewrapDetails(details, o), ev.State
	if cerr := st.CompleteAudit(context.WithoutCancel(ctx), &ev); cerr != nil {
		err = errors.Join(err, fmt.Errorf("the audit row %d of the rewrap is left pending, unknown after a reindex: %w", ev.ID, cerr))
	}
	return errors.Join(err, writeJSONLine(out, rep))
}

// switchSealingKey appends ev, pending, and makes to the sealing key where the row still names
// from ("": where there is none), in one transaction (PutSealingKey, a compare-and-set): no switch
// without its record, no record of a switch not made. A row that names to already is left as it
// is, and so is one that another rewrap to the same key switched in between, read again; one of
// another key is an error, and nothing is written. It returns when the row was set, on the
// database's clock, and whether this run switched it.
func switchSealingKey(ctx context.Context, st store, ev *index.AuditEvent, from, to string) (time.Time, bool, error) {
	for {
		k := index.SealingKey{KeyID: to, Source: index.SealingRewrap, Host: hostName()}
		err := st.Tx(ctx, func(w index.Writer) error {
			if err := w.AppendAudit(ctx, ev); err != nil {
				return err
			}
			if from == to {
				return nil
			}
			k.AuditID = &ev.ID
			return w.PutSealingKey(ctx, &k, from)
		})
		if from != to && err == nil {
			return k.SetAt, true, nil
		}
		if err != nil && !errors.Is(err, index.ErrConflict) {
			return time.Time{}, false, fmt.Errorf("the switch of the sealing key: %w", err)
		}
		row, rerr := st.GetSealingKey(ctx)
		switch {
		case rerr != nil:
			return time.Time{}, false, fmt.Errorf("the sealing key of the index: %w", rerr)
		case err == nil: // from == to: the row was to's
			return row.SetAt, false, nil
		case row.KeyID != to:
			return time.Time{}, false, fmt.Errorf("another rewrap named master key %s in between: nothing was written; "+
				"run lajstrom admin key rewrap --dry-run", row.KeyID)
		}
		from = to // another run to the same key switched: record this one beside it
	}
}

// settleAndReseal waits until rewrapSettle after setAt on the database's clock, when every
// replica has read the sealing key at a beat (none for a row set long before), then seals again
// (reseal).
func (a *app) settleAndReseal(ctx context.Context, st store, m *keys.Master, to string, setAt time.Time) (keyCounts, error) {
	now, err := st.DatabaseTime(ctx)
	if err != nil {
		return keyCounts{}, fmt.Errorf("read the database's clock: %w", err)
	}
	if wait := setAt.Add(rewrapSettle).Sub(now); wait > 0 {
		a.log.InfoContext(ctx, "the index seals with the new master key: waiting for every replica to read it at its beat",
			"sealing", to, "wait", wait.Round(time.Second).String())
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return keyCounts{}, ctx.Err()
		case <-t.C:
		}
	}
	return reseal(ctx, st, m, to)
}

// lostCounts are the values of lost by table.
func lostCounts(lost []lostValue) keyCounts {
	var n keyCounts
	for _, v := range lost {
		n.add(v.Table, 1)
	}
	return n
}

// otherSessions returns the open upload sessions of c under every key but id, by id.
func otherSessions(c census, id string) map[string]int {
	out := map[string]int{}
	for k, s := range c.Sessions {
		if k != id {
			out[k] = s.Open
		}
	}
	return out
}

// sortedIDs returns the key ids of counts, in order.
func sortedIDs(counts map[string]keyCounts) []string {
	return slices.Sorted(maps.Keys(counts))
}

// rewrapDetails are the details of the rewrap's audit row, with the origin o of the command line.
func rewrapDetails(d map[string]any, o api.Origin) json.RawMessage {
	raw, _ := json.Marshal(o.AddTo(d)) // strings, counts and maps of them: never an error
	return raw
}

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// pruneRows writes into st, a migrated index, rows that a prune deletes or keeps, each by its
// action: days before now, and whether kept for its state or its subject.
func pruneRows(t *testing.T, st index.Store, now time.Time) {
	t.Helper()
	for _, ev := range []index.AuditEvent{
		{At: now.AddDate(0, 0, -400), Action: "old"},
		{At: now.AddDate(0, 0, -400), Action: "pending", State: index.AuditPending},
		{At: now.AddDate(0, 0, -400), Action: "push", Subject: digest.FromString("subject").String(), Digest: digest.FromString("sbom")},
		{At: now.AddDate(0, 0, -200), Action: "200d"},
		{At: now.AddDate(0, 0, -100), Action: "100d"},
		{At: now.AddDate(0, 0, -10), Action: "recent"},
	} {
		ev.Principal = "anonymous"
		if err := st.AppendAudit(t.Context(), &ev); err != nil {
			t.Fatal(err)
		}
	}
}

// auditActions returns the actions of the rows of st, by id.
func auditActions(t *testing.T, st index.Store) []string {
	t.Helper()
	rows, err := st.ListAudit(t.Context(), index.AuditFilter{Feed: true})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Action)
	}
	return out
}

// pruneCLI migrates the database of a new configuration, writes pruneRows into it, and returns
// the index and the function that runs admin audit prune with args, which returns what it
// printed.
func pruneCLI(t *testing.T, now time.Time) (index.Store, func(args ...string) (pruneReport, error)) {
	t.Helper()
	dir := t.TempDir()
	cfg := writeConfig(t, dir)
	st := openIndex(t, dir)
	pruneRows(t, st, now)
	return st, func(args ...string) (pruneReport, error) {
		t.Helper()
		out, _, err := run(t, nil, append([]string{"admin", "audit", "prune", "--config", cfg, "--actor", "ops@example.com"}, args...)...)
		var rep pruneReport
		if err == nil {
			if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil || strings.Count(out, "\n") != 1 {
				t.Fatalf("%v printed %q: want one line of JSON", args, out)
			}
		}
		return rep, err
	}
}

// wantActions fails the test unless the rows of st have the actions want, by id.
func wantActions(t *testing.T, what string, st index.Store, want ...string) {
	t.Helper()
	if got := auditActions(t, st); !slices.Equal(got, want) {
		t.Errorf("%s: the rows %q, want %q", what, got, want)
	}
}

// TestAdminAuditPruneRefused: no cut-off, one that is no time and no age in days, one under a
// year without --force and one under 90 days with it are refused, exit status 2, before anything
// is written.
func TestAdminAuditPruneRefused(t *testing.T) {
	now := time.Now()
	st, prune := pruneCLI(t, now)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{nil, "--before: required"},
		{[]string{"--before", "yesterday"}, `--before "yesterday": an RFC 3339 time`},
		{[]string{"--before", "36h", "--force"}, `--before "36h": an RFC 3339 time`},
		{[]string{"--before", "364d"}, "the cut-off must be 365 days before the database's time at the least, 90 with --force"},
		{[]string{"--before", now.AddDate(0, -6, 0).Format(time.RFC3339)}, "must be 365 days"},
		{[]string{"--before", "89d", "--force"}, "the cut-off must be 90 days before the database's time at the least, also with --force"},
		{[]string{"--before", now.Add(time.Hour).Format(time.RFC3339), "--force"}, "must be 90 days"},
	} {
		if _, err := prune(tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) || exitStatus(err) != 2 {
			t.Errorf("%v: %v (exit %d), want %q, exit 2", tt.args, err, exitStatus(err), tt.want)
		}
	}
	wantActions(t, "after the refusals", st, "old", "pending", "push", "200d", "100d", "recent")
}

// TestAdminAuditPruneDryRun: --dry-run prints what it counted and writes nothing.
func TestAdminAuditPruneDryRun(t *testing.T) {
	st, prune := pruneCLI(t, time.Now())
	dry, err := prune("--before", "365d", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if dry.Rows != 1 || !dry.DryRun || dry.Deleted != nil || dry.FirstID != dry.LastID || dry.FirstAt == nil {
		t.Errorf("--dry-run: %+v, want the one old row and nothing deleted", dry)
	}
	wantActions(t, "after --dry-run", st, "old", "pending", "push", "200d", "100d", "recent")
}

// TestAdminAuditPrune: a prune deletes the rows before its cut-off but the pending row and a
// referrer's push, in batches, and writes audit.prune, with the origin of the command line,
// pending then ok with what it deleted; one before every row deletes none and writes its row all
// the same.
func TestAdminAuditPrune(t *testing.T) {
	now := time.Now()
	st, prune := pruneCLI(t, now)
	pruneBatch = 1 // the batches: one row each
	t.Cleanup(func() { pruneBatch = 10_000 })
	rep, err := prune("--before", "150d", "--force")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rows != 2 || rep.Deleted == nil || *rep.Deleted != 2 || !rep.Force || rep.Before.Sub(now.Add(-150*24*time.Hour)).Abs() > time.Minute {
		t.Errorf("--before 150d --force: %+v, want two rows deleted before 150 days ago", rep)
	}
	wantActions(t, "after the prune", st, "pending", "push", "100d", "recent", "audit.prune")
	wantPruneRow(t, st, map[string]any{"deleted": 2.0, "rows": 2.0, "force": true, "source": "cli", "actor": "ops@example.com",
		"host": hostName(), "osUser": osUser(), "before": rep.Before.Format(time.RFC3339Nano)}, "firstId", "lastId", "firstAt", "lastAt")

	none, err := prune("--before", "2020-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if none.Rows != 0 || none.Deleted == nil || *none.Deleted != 0 {
		t.Errorf("a cut-off before every row: %+v, want none deleted", none)
	}
}

// wantPruneRow fails the test unless st has one audit.prune row, ok, by anonymous, of subject
// audit, whose details have the values of want and the keys of has.
func wantPruneRow(t *testing.T, st index.Store, want map[string]any, has ...string) {
	t.Helper()
	rows, err := st.ListAudit(t.Context(), index.AuditFilter{Action: actionAuditPrune})
	if err != nil {
		t.Fatal(err)
	}
	var details map[string]any
	if len(rows) != 1 || json.Unmarshal(rows[0].Details, &details) != nil {
		t.Fatalf("the rows of audit.prune: %+v", rows)
	}
	if r := rows[0]; r.State != index.AuditOK || r.Principal != "anonymous" || r.Subject != "audit" {
		t.Errorf("the row of the prune: %+v, want ok, by anonymous, of audit", r)
	}
	for k, v := range want {
		if details[k] != v {
			t.Errorf("the row's %s: %v, want %v", k, details[k], v)
		}
	}
	for _, k := range has {
		if details[k] == nil {
			t.Errorf("the row has no %s: %v", k, details)
		}
	}
}

// TestAdminAuditPruneReason: with auth.requireReason a prune without a reason is refused, exit
// status 2, before anything is written; with --reason its row has it as changeReason.
func TestAdminAuditPruneReason(t *testing.T) {
	dir := t.TempDir()
	cfg := writeConfig(t, dir)
	appendConfig(t, cfg, "auth:\n  requireReason: true\n")
	st := openIndex(t, dir)
	pruneRows(t, st, time.Now())
	_, _, err := run(t, nil, "admin", "audit", "prune", "--before", "365d", "--config", cfg)
	if err == nil || !strings.Contains(err.Error(), "--reason (LAJSTROM_REASON): required (auth.requireReason)") || exitStatus(err) != 2 {
		t.Fatalf("a prune without a reason: %v (exit %d)", err, exitStatus(err))
	}
	if _, _, err := run(t, nil, "admin", "audit", "prune", "--before", "365d", "--reason", "CHG-1278", "--config", cfg); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListAudit(t.Context(), index.AuditFilter{Action: actionAuditPrune})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !strings.Contains(string(rows[0].Details), `"changeReason":"CHG-1278"`) {
		t.Errorf("the rows of audit.prune: %+v, want one with the reason", rows)
	}
}

// TestAdminAuditPruneClock: with this host's clock 75 s off the database's (SQLite, whose clock
// is the host's), a prune refuses before it writes anything (openMigrated's checkClock).
func TestAdminAuditPruneClock(t *testing.T) {
	dir := t.TempDir()
	st := openIndex(t, dir)
	pruneRows(t, st, time.Now())
	a := &app{cfg: &config.Config{Database: config.Database{Driver: index.DialectSQLite, DSN: "file:" + filepath.Join(dir, "lajstrom.db")}},
		log: slog.New(slog.DiscardHandler), now: func() time.Time { return time.Now().Add(75 * time.Second) }}
	cmd := newAuditPruneCmd(a)
	cmd.SetArgs([]string{"--before", "365d"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the clock of this host is 1m15s ahead of the database's") {
		t.Errorf("a prune 75 s ahead: %v", err)
	}
	if got := auditActions(t, st); len(got) != 6 {
		t.Errorf("after a refused prune: %q, want the six rows", got)
	}
}

// TestPruneCutOff: the cut-off and its floors count from the time given, the database's: an age
// in days before it, an RFC 3339 time as it is; 365 days at the least, 90 with --force.
func TestPruneCutOff(t *testing.T) {
	db := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		before  string
		force   bool
		want    time.Time
		refused bool
	}{
		{"400d", false, db.AddDate(0, 0, -400), false},
		{"365d", false, db.AddDate(0, 0, -365), false},
		{"364d", false, db.AddDate(0, 0, -364), true},
		{"364d", true, db.AddDate(0, 0, -364), false},
		{"90d", true, db.AddDate(0, 0, -90), false},
		{"89d", true, db.AddDate(0, 0, -89), true},
		{"2025-11-17T12:00:00Z", false, db.AddDate(0, 0, -365), false},
		{"2025-11-17T12:00:01Z", false, db.AddDate(0, 0, -365).Add(time.Second), true},
		{"2026-08-19T14:00:00+02:00", true, db.AddDate(0, 0, -90), false},
	} {
		cut, err := cutOff(tt.before)
		if err != nil {
			t.Fatalf("%s: %v", tt.before, err)
		}
		got := cut(db)
		err = checkFloor(got, db, tt.force)
		if !got.Equal(tt.want) || (err != nil) != tt.refused || err != nil && exitStatus(err) != 2 {
			t.Errorf("%s (force %v): cut-off %v, %v; want %v, refused %v", tt.before, tt.force, got, err, tt.want, tt.refused)
		}
	}
}

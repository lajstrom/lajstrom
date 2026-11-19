package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// rewrapEnv is a configuration on a SQLite file whose master keys come from a directory, and the
// values of its index: the fixture of newRewrapFixture, and the sealing row of the old key.
type rewrapEnv struct {
	t        *testing.T
	cfg, dsn string
	dir      string // keys.masterKeyDir
	st       *sqlite.Store
	f        *rewrapFixture
	secrets  []string // what no output, error, log or row may hold
}

// newRewrapEnv writes the configuration of keys.masterKeyDir holding the keys of files (name to
// key), migrates a SQLite file and fills it as newRewrapFixture does, the old key sealing.
func newRewrapEnv(t *testing.T, files map[string]string) *rewrapEnv {
	t.Helper()
	rewrapSettle = 50 * time.Millisecond
	t.Cleanup(func() { rewrapSettle = 2 * replicaBeatEvery })
	e := &rewrapEnv{t: t}
	e.cfg, e.dsn = writeKeyConfig(t, t.TempDir())
	e.dir = keyVolume(t, files, 0o400)
	appendConfig(t, e.cfg, "  masterKeyDir: "+e.dir+"\n")
	runEnv(t, e.cfg, nil, []step{{args: []string{"migrate"}}})
	st, err := sqlite.Open(t.Context(), e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.st = st
	e.f = fillRewrapFixture(t, st)
	if err := st.PutSealingKey(t.Context(), &index.SealingKey{KeyID: e.f.a, Source: index.SealingStart, Host: "serve-1"}, ""); err != nil {
		t.Fatal(err)
	}
	e.secrets = []string{testMasterKey[:16], otherMasterKey[:16], thirdMasterKey[:16]}
	for _, p := range e.f.plain {
		e.secrets = append(e.secrets, p)
	}
	return e
}

// rewrap runs admin key rewrap with args, and checks that neither its output nor its error holds
// a key or a value.
func (e *rewrapEnv) rewrap(args ...string) (string, error) {
	e.t.Helper()
	out, stderr, err := run(e.t, nil, append(append([]string{"admin", "key", "rewrap"}, args...), "--config", e.cfg)...)
	all := out + stderr
	if err != nil {
		all += err.Error()
	}
	e.noSecret("admin key rewrap "+strings.Join(args, " "), all)
	return out, err
}

// noSecret fails the test when s holds a master key of the tests or a value sealed.
func (e *rewrapEnv) noSecret(what, s string) {
	e.t.Helper()
	for _, secret := range e.secrets {
		if strings.Contains(s, secret) {
			e.t.Errorf("%s shows a key or a value: %q in %s", what, secret, s)
		}
	}
}

// report decodes the JSON line out.
func (e *rewrapEnv) report(out string) rewrapReport {
	e.t.Helper()
	var r rewrapReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		e.t.Fatalf("not a report: %q: %v", out, err)
	}
	return r
}

// rewraps returns the key.rewrap rows, newest first, and checks that none holds a key or a value.
func (e *rewrapEnv) rewraps() []index.AuditEvent {
	e.t.Helper()
	evs, err := e.st.ListAudit(e.t.Context(), index.AuditFilter{Action: actionKeyRewrap})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, ev := range evs {
		raw, _ := json.Marshal(ev)
		e.noSecret("the audit row", string(raw))
	}
	return evs
}

// sealing returns the id of the sealing row.
func (e *rewrapEnv) sealing() string {
	e.t.Helper()
	k, err := e.st.GetSealingKey(e.t.Context())
	if err != nil {
		e.t.Fatal(err)
	}
	return k.KeyID
}

// wantFail fails the test unless err contains want and exits with status.
func wantFail(t *testing.T, what string, err error, want string, status int) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) || exitStatus(err) != status {
		t.Errorf("%s: %v (exit %d), want %q, exit %d", what, err, exitStatus(err), want, status)
	}
}

// summary is what the tests compare of a report: the state, the switch, what it sealed again and
// left, the values lost, and the refusals.
func rewrapSummary(r rewrapReport) string {
	var resealed keyCounts
	if r.Resealed != nil {
		resealed = *r.Resealed
	}
	return fmt.Sprintf("state=%s switched=%t resealed=%+v left=%v lost=%d refusals=%d", r.State, r.Switched, resealed, r.Left,
		len(r.Lost), len(r.Refusals))
}

// rowOf is what the tests compare of a sealing row: its key, source and audit row.
func rowOf(k *index.SealingKey) string {
	audit := int64(0)
	if k.AuditID != nil {
		audit = *k.AuditID
	}
	return fmt.Sprintf("%s %s %d", k.KeyID, k.Source, audit)
}

// TestKeyRewrapRefuses: no --to and an id that is not one are usage errors, exit 2; a key the
// command does not hold, a live replica that does not hold it, and a value under no key held each
// refuse the run, exit 1, before anything is written, naming the key by its id, the replica by
// its host and the value by its row; --dry-run with --to lists the same refusals, exit 0.
func TestKeyRewrapRefuses(t *testing.T) {
	e := newRewrapEnv(t, map[string]string{"old": testMasterKey, "new": otherMasterKey})
	for args, want := range map[string]string{
		"":                      "--to: required",
		"--to ABCDEF0123456789": "the id of a master key is 16 lowercase hexadecimal digits",
		"--to 0123":             "the id of a master key is 16 lowercase hexadecimal digits",
	} {
		_, err := e.rewrap(strings.Fields(args)...)
		wantFail(t, "rewrap "+args, err, want, 2)
	}
	for _, tt := range []struct {
		to, want string
	}{
		{e.f.c, "this command does not hold master key " + e.f.c},
		{e.f.b, "values are under no master key this command holds: keys id 5 (enc FP4); add the key that sealed them"},
	} {
		_, err := e.rewrap("--to", tt.to)
		wantFail(t, "rewrap --to "+tt.to, err, tt.want, 1)
		out, err := e.rewrap("--to", tt.to, "--dry-run")
		if r := e.report(out); err != nil || !r.DryRun || !strings.Contains(strings.Join(r.Refusals, "\n"), tt.want) {
			t.Errorf("rewrap --to %s --dry-run: %+v, %v; want the refusal %q", tt.to, r, err, tt.want)
		}
	}
	now, err := e.st.DatabaseTime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	old := index.Replica{ID: "old", Host: "pod-7", PID: 7, Version: "v1.0.0", StartedAt: now}
	if err := e.st.StartReplica(t.Context(), &old, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err = e.rewrap("--to", e.f.b, "--allow-lost")
	wantFail(t, "rewrap with a replica of the release before", err, "the replica on pod-7 (pid 7, v1.0.0) does not hold master key "+e.f.b, 1)
	if got := fmt.Sprint(e.sealing(), " ", len(e.rewraps())); got != e.f.a+" 0" {
		t.Errorf("a refused rewrap wrote: the sealing key and the rows key.rewrap are %s", got)
	}
}

// TestKeyRewrapDryRun: --dry-run writes nothing and prints the sealing row, the keys of the
// command with their sources, the live replicas with their keys, the values under each key and
// under none, and the open uploads under each key; never a key nor a value.
func TestKeyRewrapDryRun(t *testing.T) {
	e := newRewrapEnv(t, map[string]string{"old": testMasterKey, "new": otherMasterKey})
	now, err := e.st.DatabaseTime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	pod := index.Replica{ID: "pod", Host: "pod-1", PID: 1, Version: "v1.1.0", StartedAt: now, MasterKeys: []string{e.f.a, e.f.b}}
	if err := e.st.StartReplica(t.Context(), &pod, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	out, err := e.rewrap("--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	r := e.report(out)
	got := fmt.Sprintf("%t %s %s held=%d/%t replicas=%s:%d values=%v lost=%d sessions=%d/%d refusals=%d", r.DryRun, r.Sealing.KeyID,
		r.Sealing.Source, len(r.Held), strings.HasPrefix(r.Held[0].Source, e.dir), r.Replicas[0].Host, len(r.Replicas[0].MasterKeys),
		r.Values, len(r.Lost), r.Sessions[e.f.a].Open, r.Sessions[noKey].Open, len(r.Refusals))
	want := fmt.Sprintf("true %s start held=2/true replicas=pod-1:2 values=%v lost=1 sessions=2/1 refusals=0", e.f.a,
		map[string]keyCounts{e.f.a: {Keys: 3, Webhooks: 2, Refresh: 1}, e.f.b: {Keys: 1, Webhooks: 1, Refresh: 1}})
	if got != want {
		t.Errorf("the dry run: %s\n got %s\nwant %s", out, got, want)
	}
	if got := fmt.Sprint(e.sealing(), " ", len(e.rewraps())); got != e.f.a+" 0" {
		t.Errorf("the dry run wrote: %s", got)
	}
}

// TestKeyRewrap: the rollout of a replacement. With the old and the new key in the directory, a
// rewrap to the new one switches the sealing key with its row key.rewrap, waits, seals every value
// of keys and webhooks under the old key again, leaves the lost value with --allow-lost, and ends
// ok; the row holds from, to, what was under each key, the lost, the uploads, what it sealed
// again, the actor and the reason. Run again it finds nothing and writes ok with zeros at once.
// Then the old key leaves the directory, and every value but the lost one opens with the new key
// alone. No output, error or row holds a key or a value.
func TestKeyRewrap(t *testing.T) {
	e := newRewrapEnv(t, map[string]string{"old": testMasterKey, "new": otherMasterKey})
	out, err := e.rewrap("--to", e.f.b, "--allow-lost", "--actor", "alice@example.com", "--reason", "CHG-1288")
	if err != nil {
		t.Fatal(err)
	}
	r := e.report(out)
	if got := rewrapSummary(r); got != "state=ok switched=true resealed={Keys:3 Webhooks:2 Refresh:1} left=map[] lost=1 refusals=0" {
		t.Errorf("the rewrap: %s", got)
	}
	k, err := e.st.GetSealingKey(t.Context())
	if err != nil || rowOf(k) != fmt.Sprintf("%s rewrap %d", e.f.b, r.Audit) {
		t.Errorf("the sealing row: %+v, %v; want %s by the rewrap's row %d", k, err, e.f.b, r.Audit)
	}
	evs := e.rewraps()
	if got := fmt.Sprint(len(evs), evs[0].State, evs[0].Subject, evs[0].Principal); got != fmt.Sprint(1, "ok", "master:"+e.f.b, "anonymous") {
		t.Fatalf("the rows key.rewrap: %+v", evs)
	}
	sameDetails(t, evs[0], `{"actor":"alice@example.com","changeReason":"CHG-1288","from":"`+e.f.a+`",`+
		`"lost":{"keys":1,"refresh":0,"webhooks":0},"resealed":{"keys":3,"refresh":1,"webhooks":2},"sessions":{"`+e.f.a+`":2,"none":1},`+
		`"source":"cli","to":"`+e.f.b+`","under":{"`+e.f.a+`":{"keys":3,"refresh":1,"webhooks":2}}}`)

	out, err = e.rewrap("--to", e.f.b, "--allow-lost")
	if got := rewrapSummary(e.report(out)); err != nil || got != "state=ok switched=false resealed={Keys:0 Webhooks:0 Refresh:0} left=map[] lost=1 refusals=0" {
		t.Errorf("a run again: %s, %v; want ok with zeros", got, err)
	}
	if evs := e.rewraps(); len(evs) != 2 || !strings.Contains(string(evs[0].Details), `"resealed":{"keys":0,"webhooks":0,"refresh":0}`) {
		t.Errorf("the row of a run again: %+v", evs[0])
	}

	if err := os.Remove(filepath.Join(e.dir, "old")); err != nil {
		t.Fatal(err)
	}
	only, err := loadMasters(config.Keys{MasterKeyDir: e.dir}, noEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	only.SealWith(e.f.b)
	c, err := countSealed(t.Context(), e.st, only)
	if err != nil || fmt.Sprint(c.Under, len(c.Lost)) != fmt.Sprint(map[string]keyCounts{e.f.b: {Keys: 4, Webhooks: 3, Refresh: 2}}, 1) {
		t.Errorf("with the new key alone: %+v, %v; want every value under it but the lost one", c, err)
	}
}

// sameDetails checks the details of ev, but the host and the operating-system user, against want.
func sameDetails(t *testing.T, ev index.AuditEvent, want string) {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(ev.Details, &d); err != nil {
		t.Fatal(err)
	}
	delete(d, "host")
	delete(d, "osUser")
	if got, _ := json.Marshal(d); string(got) != want {
		t.Errorf("the details:\n%s\nwant\n%s", got, want)
	}
}

// TestKeyRewrapReason: with auth.requireReason a rewrap without a reason is refused, exit 2,
// before anything is written.
func TestKeyRewrapReason(t *testing.T) {
	e := newRewrapEnv(t, map[string]string{"old": testMasterKey, "new": otherMasterKey})
	appendConfig(t, e.cfg, "auth:\n  requireReason: true\n")
	_, err := e.rewrap("--to", e.f.b, "--allow-lost")
	if err == nil || !strings.Contains(err.Error(), "--reason (LAJSTROM_REASON): required (auth.requireReason)") || exitStatus(err) != 2 {
		t.Errorf("a rewrap without a reason: %v", err)
	}
	if e.sealing() != e.f.a {
		t.Error("a rewrap without a reason switched")
	}
}

// lateSeal is a store on which, once the batches of a rewrap have written, a late replica seals a
// new key under the old master key before the count again reads the keys: the third transaction
// is the batch of the webhooks, after the switch and the batch of the keys.
type lateSeal struct {
	store
	t      *testing.T
	txs    int
	sealed bool
}

func (s *lateSeal) Tx(ctx context.Context, fn func(index.Writer) error) error {
	s.txs++
	return s.store.Tx(ctx, fn)
}

func (s *lateSeal) ListSealed(ctx context.Context, table string, afterID int64, limit int) ([]index.Sealed, error) {
	if table == index.SealedKeys && afterID == 0 && s.txs >= 3 && !s.sealed {
		s.sealed = true
		k := index.Key{Kind: "gpg", Public: []byte("p"), PrivateEnc: sealedUnder(s.t, testMasterKey, "LATE", "late"), Fingerprint: "LATE"}
		if err := s.CreateKey(ctx, &k); err != nil {
			return nil, err
		}
	}
	return s.store.ListSealed(ctx, table, afterID, limit)
}

// TestKeyRewrapLeft: a value that a late replica sealed under the old key after the batches is
// found by the count again: the run ends failed, exit 1, with what it sealed and what is left by
// key, in its output and its row; a run again seals it and ends ok.
func TestKeyRewrapLeft(t *testing.T) {
	rewrapSettle = 0
	t.Cleanup(func() { rewrapSettle = 2 * replicaBeatEvery })
	ctx := t.Context()
	f := newRewrapFixture(t)
	if err := f.st.PutSealingKey(ctx, &index.SealingKey{KeyID: f.a, Source: index.SealingStart, Host: "h"}, ""); err != nil {
		t.Fatal(err)
	}
	a := &app{log: slog.New(slog.DiscardHandler)}
	st := &lateSeal{store: f.st, t: t}
	run := func() (rewrapReport, error) {
		m := heldOf(t, testMasterKey, otherMasterKey)
		rep, c, err := a.rewrapCheck(ctx, st, m, f.b, true)
		if err != nil || len(rep.Refusals) > 0 {
			t.Fatalf("the check: %v, %q", err, rep.Refusals)
		}
		var out bytes.Buffer
		err = a.rewrapRun(ctx, &out, st, m, rep, c)
		var r rewrapReport
		if jerr := json.Unmarshal(out.Bytes(), &r); jerr != nil {
			t.Fatalf("the report %q: %v", out.String(), jerr)
		}
		return r, err
	}
	r, err := run()
	wantFail(t, "a run with a value sealed late", err, "values are left under another master key ("+f.a+")", 1)
	if got, want := rewrapSummary(r), "state=failed switched=true resealed={Keys:3 Webhooks:2 Refresh:1} left=map["+f.a+":{1 0 0}] lost=1 refusals=0"; got != want {
		t.Errorf("a run with a value sealed late: %s, want %s", got, want)
	}
	evs, err := f.st.ListAudit(ctx, index.AuditFilter{Action: actionKeyRewrap})
	if err != nil || len(evs) != 1 || !strings.Contains(string(evs[0].Details), `"left":{"`+f.a+`":{"keys":1,"webhooks":0,"refresh":0}}`) {
		t.Errorf("the row of the failed run: %+v, %v", evs, err)
	}
	r, err = run()
	if got := rewrapSummary(r); err != nil || got != "state=ok switched=false resealed={Keys:1 Webhooks:0 Refresh:0} left=map[] lost=1 refusals=0" {
		t.Errorf("a run again: %s, %v; want ok, the late key sealed again", got, err)
	}
}

// TestSwitchSealingKey: the switch and its pending row are one transaction, a compare-and-set on
// the id read. Of two runs that read one row, one to another key switches and the other exits
// with an error that names the key, and writes nothing; one to the same key, which read the row
// before the switch, writes its row beside it and switches nothing.
func TestSwitchSealingKey(t *testing.T) {
	ctx := t.Context()
	st := newSealingStore(t)
	a, b, c := masterID(t, testMasterKey), masterID(t, otherMasterKey), masterID(t, thirdMasterKey)
	if err := st.PutSealingKey(ctx, &index.SealingKey{KeyID: a, Source: index.SealingStart, Host: "h"}, ""); err != nil {
		t.Fatal(err)
	}
	pending := func() *index.AuditEvent {
		return &index.AuditEvent{Principal: "anonymous", Action: actionKeyRewrap, State: index.AuditPending, Details: []byte(`{}`)}
	}
	first := pending()
	setAt, switched, err := switchSealingKey(ctx, st, first, a, b)
	if err != nil || !switched || setAt.IsZero() {
		t.Fatalf("the first switch: %v, %v, %v", setAt, switched, err)
	}
	if k, err := st.GetSealingKey(ctx); err != nil || rowOf(k) != fmt.Sprintf("%s rewrap %d", b, first.ID) {
		t.Errorf("the row after the switch: %+v, %v; want %s with the row %d", k, err, b, first.ID)
	}
	if _, _, err := switchSealingKey(ctx, st, pending(), a, c); err == nil || !strings.Contains(err.Error(),
		"another rewrap named master key "+b+" in between: nothing was written") {
		t.Errorf("a switch to another key from the id read before: %v", err)
	}
	same := pending()
	at, switched, err := switchSealingKey(ctx, st, same, a, b)
	if fmt.Sprint(err, switched, at.Equal(setAt), same.ID > 0) != "<nil> false true true" {
		t.Errorf("a switch to the same key from the id read before: %v, %v, %v, row %d; want the first's time, no switch, a row",
			at, switched, err, same.ID)
	}
	evs, err := st.ListAudit(ctx, index.AuditFilter{Action: actionKeyRewrap})
	if err != nil || len(evs) != 2 {
		t.Errorf("the rows key.rewrap: %d, %v; want those of the two runs to %s", len(evs), err, b)
	}
}

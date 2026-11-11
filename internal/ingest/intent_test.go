package ingest_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// errDown is the error of a database that went away.
var errDown = errors.New("connection refused")

// downStore is an index whose database goes away once down is set: its transactions fail, and
// with all the completion of an audit row on its own too. The pending row of a write comes
// before, so it is written.
type downStore struct {
	index.Store
	down *atomic.Bool
	all  bool
}

func (s downStore) Tx(ctx context.Context, fn func(index.Writer) error) error {
	if s.down.Load() {
		return errDown
	}
	return s.Store.Tx(ctx, fn)
}

func (s downStore) CompleteAudit(ctx context.Context, ev *index.AuditEvent) error {
	if s.down.Load() && s.all {
		return errDown
	}
	return s.Store.CompleteAudit(ctx, ev)
}

// TestAuditIntent runs the audit scenarios of #473 and #474 on SQLite
// (TestIntegrationAuditIntent on Postgres).
func TestAuditIntent(t *testing.T) { testAuditIntent(t, newEnv) }

// testAuditIntent: every registry write has its audit row before it, pending, and the row says
// how the write ended (#473). A publish and a delete leave one row each, ok. A publish whose
// process stops after the registry accepted the manifest, or whose database goes away then,
// leaves its row pending, and a reindex after ingest.claimTTL marks it unknown; the reindex
// indexes the manifest. An index transaction that fails after the registry write fails the row
// with the error, the delete's of #474 too; and a delete whose database goes away after the
// registry delete leaves its row pending, then unknown.
func testAuditIntent(t *testing.T, newEnv func(*testing.T) *env) {
	for _, tt := range []struct {
		name string
		run  func(t *testing.T, e *env)
	}{
		{"happy", auditHappy},
		{"publish killed after the push", auditPublishKilled},
		{"publish, the database gone after the push", func(t *testing.T, e *env) { auditPublishDown(t, e, true) }},
		{"publish, the index transaction fails after the push", func(t *testing.T, e *env) { auditPublishDown(t, e, false) }},
		{"delete, the index transaction fails after the registry delete (#474)", func(t *testing.T, e *env) { auditDeleteDown(t, e, false) }},
		{"delete, the database gone after the registry delete", func(t *testing.T, e *env) { auditDeleteDown(t, e, true) }},
	} {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, newEnv(t)) })
	}
}

// auditUpload is the upload of the scenarios of testAuditIntent, and auditLater a time past
// ingest.claimTTL, 10 minutes, after the clock of the pipeline.
var (
	auditUpload = upload("notes", "1.0", file{"notes", "hello"})
	auditLater  = now.Add(11 * time.Minute)
)

func auditHappy(t *testing.T, e *env) {
	pub, err := e.pipe.Upload(t.Context(), e.repo, auditUpload, alice)
	must(t, err)
	_, err = e.pipe.Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: pub.Tag}, alice)
	must(t, err)
	push, del := e.auditRow(t, "push"), e.auditRow(t, "delete")
	var details map[string]int // Postgres's jsonb spells it its own way
	must(t, json.Unmarshal(del.Details, &details))
	if push.State != index.AuditOK || push.Digest != pub.Digest || del.State != index.AuditOK || del.Digest != pub.Digest ||
		push.Error != "" || del.Error != "" || len(details) != 1 || details["referrers"] != 0 {
		t.Errorf("rows %+v and %+v, want both ok of %s, the delete with 0 referrers", push, del, pub.Digest)
	}
}

func auditPublishKilled(t *testing.T, e *env) {
	pipe := ingest.New(e.options(hooked{Fake: e.prov, afterPush: runtime.Goexit}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := pipe.Upload(t.Context(), e.repo, auditUpload, alice)
		t.Errorf("Upload returned %v, want the process stopped after the push", err)
	}()
	<-done
	pushed := e.tagged(t, "raw/"+e.repo.Name+"/notes", "1.0")
	e.wantRow(t, "push", index.AuditPending, pushed, "")
	e.sweep(t, now.Add(time.Minute)) // younger than ingest.claimTTL: still under way
	e.wantRow(t, "push", index.AuditPending, pushed, "")
	e.sweep(t, auditLater)
	e.wantRow(t, "push", index.AuditUnknown, pushed, "")
	if r := e.rows(t); len(r.Versions) != 1 || r.Versions[0].Digest != pushed {
		t.Errorf("versions after the reindex %+v, want the manifest the registry has, %s", r.Versions, pushed)
	}
}

// auditPublishDown fails the index transaction after the push, and with all the database.
func auditPublishDown(t *testing.T, e *env, all bool) {
	var down atomic.Bool
	opts := e.options(hooked{Fake: e.prov, afterPush: func() { down.Store(true) }})
	opts.Index = downStore{Store: e.st, down: &down, all: all}
	if _, err := ingest.New(opts).Upload(t.Context(), e.repo, auditUpload, alice); err == nil || !strings.Contains(err.Error(), errDown.Error()) {
		t.Fatalf("Upload = %v, want the database's error", err)
	}
	pushed := e.tagged(t, "raw/"+e.repo.Name+"/notes", "1.0")
	if !all {
		e.wantRow(t, "push", index.AuditFailed, pushed, "after the registry accepted it: connection refused")
		e.sweep(t, auditLater)
		e.wantRow(t, "push", index.AuditFailed, pushed, "after the registry accepted it: connection refused")
		return
	}
	e.wantRow(t, "push", index.AuditPending, pushed, "")
	e.sweep(t, auditLater)
	e.wantRow(t, "push", index.AuditUnknown, pushed, "")
}

// auditDeleteDown fails the index transaction after the registry delete, and with all the
// database.
func auditDeleteDown(t *testing.T, e *env, all bool) {
	pub, err := e.pipe.Upload(t.Context(), e.repo, auditUpload, alice)
	must(t, err)
	var down atomic.Bool
	down.Store(true)
	opts := e.options(e.prov)
	opts.Index = downStore{Store: e.st, down: &down, all: all}
	_, err = ingest.New(opts).Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: pub.Tag}, alice)
	if err == nil || !strings.Contains(err.Error(), "after the registry deleted it") {
		t.Fatalf("Delete = %v, want the index's error after the registry delete", err)
	}
	if d := e.tagged(t, "raw/"+e.repo.Name+"/notes", pub.Tag); d != "" {
		t.Fatalf("the registry still has %s", d)
	}
	if !all {
		e.wantRow(t, "delete", index.AuditFailed, pub.Digest, "after the registry deleted it: connection refused")
		return
	}
	e.wantRow(t, "delete", index.AuditPending, pub.Digest, "")
	e.sweep(t, auditLater)
	e.wantRow(t, "delete", index.AuditUnknown, pub.Digest, "")
}

// auditRow returns the one audit row of action in the env's repo.
func (e *env) auditRow(t *testing.T, action string) index.AuditEvent {
	t.Helper()
	evs, err := e.st.ListAudit(t.Context(), index.AuditFilter{Repo: e.repo.Format + "/" + e.repo.Name, Action: action})
	must(t, err)
	if len(evs) != 1 {
		t.Fatalf("%d audit rows of %s, want 1: %+v", len(evs), action, evs)
	}
	return evs[0]
}

// wantRow fails the test unless the one audit row of action has the state, the digest d and an
// error that ends with errSuffix, none when errSuffix is "".
func (e *env) wantRow(t *testing.T, action, state string, d digest.Digest, errSuffix string) {
	t.Helper()
	ev := e.auditRow(t, action)
	if ev.State != state || ev.Digest != d || !strings.HasSuffix(ev.Error, errSuffix) || (errSuffix == "") != (ev.Error == "") {
		t.Errorf("the %s row: state %q, digest %s, error %q; want %q, %s, an error ending %q", action, ev.State, ev.Digest, ev.Error,
			state, d, errSuffix)
	}
}

// sweep runs at the time at the reindex run of the reconcile loop, on the env's repo alone: it
// marks the pending audit rows older than ingest.claimTTL unknown.
func (e *env) sweep(t *testing.T, at time.Time) {
	t.Helper()
	w := reindex.New(reindex.Options{Index: e.st, Providers: provider.Map{"local": e.prov}, Inspector: e.pipe,
		Now: func() time.Time { return at }, Log: slog.New(slog.DiscardHandler)})
	req := reindex.Request{Repo: &e.repo, Prune: true, Source: reindex.SourceReconcile, Principal: auth.Anonymous.String()}
	run, err := w.Start(t.Context(), req)
	must(t, err)
	_, err = w.Run(t.Context(), run, req)
	must(t, err)
}

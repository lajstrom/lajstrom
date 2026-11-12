package reindex_test

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// auditReads is an index.Store that counts the calls of ListAudit.
type auditReads struct {
	index.Store
	calls atomic.Int64
}

func (a *auditReads) ListAudit(ctx context.Context, f index.AuditFilter) ([]index.AuditEvent, error) {
	a.calls.Add(1)
	return a.Store.ListAudit(ctx, f)
}

// through makes e publish, delete and reindex through prov, the worker on the index st.
func (e *env) through(prov provider.Provider, st index.Store) {
	e.providers = provider.Map{"local": prov}
	log := slog.New(slog.DiscardHandler)
	plugins := func(name string) (format.Plugin, bool) { return raw.Plugin{}, name == raw.Name }
	e.pipe = ingest.New(ingest.Options{Providers: e.providers, Index: e.st, Plugins: plugins, Log: log})
	e.inspect = &counting{Inspector: e.pipe}
	e.w = reindex.New(reindex.Options{Index: st, Providers: e.providers, Inspector: e.inspect, Repairer: e.pipe, Log: log, Now: e.clock.now})
}

// TestReindexRecentDelete is INV-13 on a registry that lags a delete (#446): a version deleted
// through Lajstrom, whose tag the registry still lists and whose manifest it still serves, is
// not indexed again by a run less than the grace age after the delete, which counts it in
// DeletedPending; a run past the grace age indexes a manifest the registry still lists, as it
// indexes one pushed straight to the registry. On a strongly consistent registry a run does
// not read the audit log.
func TestReindexRecentDelete(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	st := &auditReads{Store: e.st}
	e.through(&providertest.Lag{Fake: e.fake, Deletes: -1}, st)
	gone := e.publish(t, "docs/gone.txt", "gone")
	e.publish(t, "docs/kept.txt", "kept")
	_, err := e.pipe.Delete(ctx, e.repo, format.Deletion{Path: "docs/gone.txt", Reference: gone.Digest.String()}, alice)
	must(t, err)

	for _, at := range []time.Duration{0, 59 * time.Minute} {
		e.clock.later(at)
		r := e.run(t, reindex.Request{Prune: true})
		if r.Added != 0 || r.DeletedPending != 1 || r.Unchanged != 1 || r.Removed != 0 || len(r.Errors) != 0 {
			t.Errorf("a run %s after the delete: %+v; want the version deleted pending, not added", at, *r)
		}
		if _, err := e.st.GetPackage(ctx, e.repo.ID, "docs/gone.txt"); !errors.Is(err, index.ErrNotFound) {
			t.Fatalf("the package deleted after a run %s later: %v, want ErrNotFound", at, err)
		}
	}
	if st.calls.Load() == 0 {
		t.Error("the runs on an eventually consistent registry did not read the audit log")
	}

	e.clock.later(2 * time.Minute) // 61 minutes after the delete: past the grace age
	r := e.run(t, reindex.Request{Prune: true})
	if r.Added != 1 || r.DeletedPending != 0 {
		t.Errorf("a run past the grace age: %+v; want the manifest the registry lists added", *r)
	}

	strong := &auditReads{Store: e.st}
	e.through(e.fake, strong)
	again := e.publish(t, "docs/again.txt", "again")
	_, err = e.pipe.Delete(ctx, e.repo, format.Deletion{Path: "docs/again.txt", Reference: again.Digest.String()}, alice)
	must(t, err)
	if r := e.run(t, reindex.Request{Prune: true}); r.DeletedPending != 0 || r.Added != 0 || strong.calls.Load() != 0 {
		t.Errorf("a run on a strongly consistent registry: %+v, %d reads of the audit log; want none", *r, strong.calls.Load())
	}
}

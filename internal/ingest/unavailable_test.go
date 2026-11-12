package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// indexDown is a store whose transactions fail at the first call of the Writer method at, as a
// database that dropped out fails them: with index.ErrUnavailable. Later calls go through.
type indexDown struct {
	index.Store
	at   string
	done atomic.Bool
}

func (s *indexDown) Tx(ctx context.Context, fn func(index.Writer) error) error {
	return s.Store.Tx(ctx, func(w index.Writer) error { return fn(indexDownWriter{Writer: w, s: s}) })
}

// down is the error of the call method, nil but for the first call of s.at.
func (s *indexDown) down(method string) error {
	if method == s.at && s.done.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: %s: %w", index.ErrUnavailable, method, errDown)
	}
	return nil
}

type indexDownWriter struct {
	index.Writer
	s *indexDown
}

func (w indexDownWriter) UpsertJournal(ctx context.Context, e *index.JournalEntry) error {
	if err := w.s.down("UpsertJournal"); err != nil {
		return err
	}
	return w.Writer.UpsertJournal(ctx, e)
}

func (w indexDownWriter) DeleteJournal(ctx context.Context, id int64) error {
	if err := w.s.down("DeleteJournal"); err != nil {
		return err
	}
	return w.Writer.DeleteJournal(ctx, id)
}

func (w indexDownWriter) DeleteVersion(ctx context.Context, id int64) error {
	if err := w.s.down("DeleteVersion"); err != nil {
		return err
	}
	return w.Writer.DeleteVersion(ctx, id)
}

// wantUnavailable stops the test unless err wraps index.ErrUnavailable and is answered 503 with
// Retry-After: 5.
func wantUnavailable(t *testing.T, what string, err error) {
	t.Helper()
	h := http.Header{}
	format.SetRetryAfter(h, err)
	if !errors.Is(err, index.ErrUnavailable) || format.StatusOf(err) != http.StatusServiceUnavailable || h.Get("Retry-After") != "5" {
		t.Fatalf("%s: %v (status %d, Retry-After %q), want index.ErrUnavailable: 503, Retry-After: 5", what, err,
			format.StatusOf(err), h.Get("Retry-After"))
	}
}

// TestJournalIndexUnavailable: an append whose database did not answer after the registry took
// its entry (#668) is index.ErrUnavailable, 503 with Retry-After: 5, not a 500, and its audit row
// is failed; the entry stays in the registry without a row, and the client's retry adopts it and
// appends the op again, which folds to the same dist-tag and releases both claims.
func TestJournalIndexUnavailable(t *testing.T) {
	ctx := t.Context()
	e := newNpmEnv(t, "1.0.0")
	opts := e.options(e.prov)
	opts.Index = &indexDown{Store: e.st, at: "UpsertJournal"}
	e.pipe = ingest.New(opts)
	latest := []format.JournalOp{opAdd("latest", "1.0.0")}
	_, err := e.pipe.Journal(ctx, e.npm, widgetsPath, latest, alice)
	wantUnavailable(t, "Journal", err)
	if len(e.pushRefs(profile.JournalTag(1))) != 1 || len(e.entries(t)) != 0 || len(e.state(t)) != 0 {
		t.Fatalf("after the failed append: %d pushes of entry 1, rows %+v, state %+v; want the push and no row",
			len(e.pushRefs(profile.JournalTag(1))), e.entries(t), e.state(t))
	}
	if ev := e.rows(t).Audit[0]; ev.State != index.AuditFailed || !strings.Contains(ev.Error, errDown.Error()) {
		t.Errorf("the audit row of the failed append: %s %q, want failed with the database's error", ev.State, ev.Error)
	}
	seqs, err := e.pipe.Journal(ctx, e.npm, widgetsPath, latest, alice)
	must(t, err)
	same(t, "seqs of the retry", []int64{2}, seqs)
	if rows := e.entries(t); len(rows) != 2 {
		t.Errorf("rows after the retry: %+v, want entry 1 adopted and entry 2", rows)
	}
	same(t, "state after the retry", []index.JournalState{
		{PackageID: e.pkg.ID, Kind: index.JournalDistTag, Key: "latest", Value: "1.0.0", Seq: 2},
	}, e.state(t))
	for _, seq := range []int64{1, 2} {
		if c, err := e.st.GetTagClaim(ctx, e.npm.ID, widgetsPath, profile.JournalTag(seq)); !errors.Is(err, index.ErrNotFound) {
			t.Errorf("the claim of entry %d after the retry: %+v, %v; want it released", seq, c, err)
		}
	}
}

// fetchDown is the fake whose first Fetch fails as a registry behind an open circuit breaker
// does: a *provider.UnavailableError with the breaker's time left.
type fetchDown struct {
	*providertest.Fake
	done *atomic.Bool
}

func (f fetchDown) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.Fake.Repository(ctx, name)
	return fetchDownRepository{Repository: rr, done: f.done}, err
}

type fetchDownRepository struct {
	provider.Repository
	done *atomic.Bool
}

func (r fetchDownRepository) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if r.done.CompareAndSwap(false, true) {
		return nil, &provider.UnavailableError{RetryAfter: 2 * time.Second}
	}
	return r.Repository.Fetch(ctx, desc)
}

// TestJournalAdoptUnavailable: an entry to adopt that the registry does not hand over, its
// circuit breaker open (#668), is provider.ErrUnavailable, 503 with the breaker's Retry-After,
// not a 500 that says the entry is not the package's; nothing is pushed, and the retry adopts the
// entry before its own.
func TestJournalAdoptUnavailable(t *testing.T) {
	ctx := t.Context()
	e := newNpmEnv(t, "1.0.0", "2.0.0")
	e.pushEntry(t, 1, opAdd("next", "2.0.0"), nil)
	e.pipe = ingest.New(e.options(fetchDown{Fake: e.prov, done: &atomic.Bool{}}))
	latest := []format.JournalOp{opAdd("latest", "2.0.0")}
	_, err := e.pipe.Journal(ctx, e.npm, widgetsPath, latest, alice)
	h := http.Header{}
	format.SetRetryAfter(h, err)
	if !errors.Is(err, provider.ErrUnavailable) || format.StatusOf(err) != http.StatusServiceUnavailable || h.Get("Retry-After") != "2" ||
		strings.Contains(err.Error(), "is not entry") {
		t.Fatalf("Journal: %v (status %d, Retry-After %q), want provider.ErrUnavailable: 503, Retry-After: 2", err,
			format.StatusOf(err), h.Get("Retry-After"))
	}
	if len(e.pushRefs(profile.JournalTag(2))) != 0 || len(e.entries(t)) != 0 {
		t.Fatalf("the append that could not read entry 1 wrote: %d pushes of entry 2, rows %+v", len(e.pushRefs(profile.JournalTag(2))), e.entries(t))
	}
	seqs, err := e.pipe.Journal(ctx, e.npm, widgetsPath, latest, alice)
	must(t, err)
	same(t, "seqs of the retry", []int64{2}, seqs)
	same(t, "state after the retry", []index.JournalState{
		{PackageID: e.pkg.ID, Kind: index.JournalDistTag, Key: "latest", Value: "2.0.0", Seq: 2},
		{PackageID: e.pkg.ID, Kind: index.JournalDistTag, Key: "next", Value: "2.0.0", Seq: 1},
	}, e.state(t))
}

// TestDeleteIndexUnavailable: a delete whose database did not answer after the registry deleted
// the manifest (#668) is index.ErrUnavailable, 503 with Retry-After: 5, not a 500, and its audit
// row is failed; the rows stay. The client's retry finds the version in the index and the
// manifest gone, and deletes the rows: it succeeds, and the index has nothing of it left.
func TestDeleteIndexUnavailable(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	built, raw, pkgPath := e.prepare(t, upload("notes", "1.0", file{"notes", "hello"}))
	res, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	opts := e.options(e.prov)
	opts.Index = &indexDown{Store: e.st, at: "DeleteVersion"}
	e.pipe = ingest.New(opts)
	del := format.Deletion{Path: pkgPath, Reference: built.Tag}
	_, err = e.pipe.Delete(ctx, e.repo, del, alice)
	wantUnavailable(t, "Delete", err)
	gone(t, e, res.Digest.String(), built.Tag)
	if r := e.rows(t); len(r.Versions) != 1 || r.Audit[0].Action != "delete" || r.Audit[0].State != index.AuditFailed {
		t.Fatalf("after the failed delete: %d versions, newest audit row %+v; want the version and the delete failed", len(r.Versions), r.Audit[0])
	}
	got, err := e.pipe.Delete(ctx, e.repo, del, alice)
	must(t, err)
	r := e.rows(t)
	if got.Digest != res.Digest || got.Version == nil || len(r.Packages) != 0 || len(r.Versions) != 0 ||
		r.Audit[0].Action != "delete" || r.Audit[0].State != index.AuditOK {
		t.Errorf("the retry: %+v; %d packages, %d versions, newest audit row %+v; want the version deleted, ok", got,
			len(r.Packages), len(r.Versions), r.Audit[0])
	}
}

// TestJournalDeleteIndexUnavailable: an entry's delete whose database did not answer after the
// registry deleted the entry (#668) is index.ErrUnavailable, a 503; the row stays. The retry
// finds the entry in the index and not in the registry, deletes the row and folds the state
// again, as the delete of a version that is gone does; a delete after that is index.ErrNotFound.
func TestJournalDeleteIndexUnavailable(t *testing.T) {
	ctx := t.Context()
	e := newNpmEnv(t, "1.0.0", "2.0.0")
	_, err := e.pipe.Journal(ctx, e.npm, widgetsPath, []format.JournalOp{opAdd("latest", "1.0.0"), opAdd("latest", "2.0.0")}, alice)
	must(t, err)
	opts := e.options(e.prov)
	opts.Index = &indexDown{Store: e.st, at: "DeleteJournal"}
	e.pipe = ingest.New(opts)
	del := format.Deletion{Path: widgetsPath, Reference: profile.JournalTag(2)}
	_, err = e.pipe.Delete(ctx, e.npm, del, alice)
	wantUnavailable(t, "Delete of entry 2", err)
	if d := e.tagged(t, "npm/internal/"+widgetsPath, profile.JournalTag(2)); d != "" || len(e.entries(t)) != 2 {
		t.Fatalf("after the failed delete: the registry has %q under entry 2, rows %+v; want it gone and both rows", d, e.entries(t))
	}
	got, err := e.pipe.Delete(ctx, e.npm, del, alice)
	must(t, err)
	if rows := e.entries(t); got.Digest == "" || got.Version != nil || len(rows) != 1 || rows[0].Seq != 1 {
		t.Errorf("the retry: %+v; rows %+v, want entry 1 alone", got, rows)
	}
	same(t, "state without entry 2", []index.JournalState{
		{PackageID: e.pkg.ID, Kind: index.JournalDistTag, Key: "latest", Value: "1.0.0", Seq: 1},
	}, e.state(t))
	if _, err := e.pipe.Delete(ctx, e.npm, del, alice); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Delete of entry 2 once more: %v, want index.ErrNotFound", err)
	}
}

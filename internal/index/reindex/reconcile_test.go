package reindex

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestReconcile drives the loop with a fake clock: the first run after the jitter, the next an
// interval after the end of a run; a manifest pushed past Lajstrom is in the index after one
// tick with a reconcile audit row (I-03) and a tick with nothing new writes none; a tick while
// another run holds the lock is skipped; a failing registry is a report error and the loop
// goes on; the loop ends with its context.
func TestReconcile(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	l := newLoop(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.w.Reconcile(ctx, time.Hour)
	}()
	if d := <-l.waits; d != 15*time.Minute {
		t.Fatalf("the first wait: %s, want the jitter", d)
	}

	pushRaw(t, l.fake, l.repo, "pushed.txt", "past Lajstrom")
	l.tick(t)
	l.wantPackage(t, "pushed.txt")
	if ev := l.audit(t, SourceReconcile); len(ev) != 1 || ev[0].Repo != "raw/scratch" || !strings.Contains(string(ev[0].Details), `"added":1`) ||
		!strings.Contains(string(ev[0].Details), `"repositoryUnknown":0`) {
		t.Errorf("I-03: the reconcile events: %+v", ev)
	}
	if ev := l.audit(t, "repo.reindex"); len(ev) != 0 {
		t.Errorf("a run of the loop wrote repo.reindex: %+v", ev)
	}
	if first := l.latest(t); first.Source != SourceReconcile || first.State != index.ReindexDone || !first.Options.Prune {
		t.Errorf("the loop's run: %+v", first)
	}

	l.tick(t)
	if ev := l.audit(t, SourceReconcile); len(ev) != 1 {
		t.Errorf("a tick with nothing new wrote %d more events", len(ev)-1)
	}

	held, err := l.w.Start(ctx, Request{Repo: &l.repo, Source: "cli", Principal: "anonymous"})
	must(t, err)
	before := l.latest(t)
	l.tick(t)
	if after := l.latest(t); after.ID != before.ID {
		t.Errorf("a tick while run %d holds the lock started run %d", held.ID, after.ID)
	}
	held.State = index.ReindexDone
	must(t, l.st.FinishReindexRun(ctx, held))

	pushRaw(t, l.fake, l.repo, "later.txt", "while the registry fails")
	l.fake.FailFetch = errors.New("connection reset by peer")
	l.tick(t)
	if failed := l.latest(t); failed.State != index.ReindexDone || !strings.Contains(string(failed.Report), "connection reset by peer") {
		t.Errorf("a run on a failing registry: %+v", failed)
	}
	l.fake.FailFetch = nil
	l.tick(t)
	l.wantPackage(t, "later.txt")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Reconcile did not return when its context ended")
	}
}

// loop is a worker over a fake registry with one repo, whose clock the test drives: the loop
// sends every wait to waits and goes on when the test sends to ticks.
type loop struct {
	st    *sqlite.Store
	fake  *providertest.Fake
	repo  index.Repo
	w     *Worker
	waits chan time.Duration
	ticks chan time.Time
}

func newLoop(t *testing.T) *loop {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:reconcile?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	l := &loop{st: st, fake: &providertest.Fake{RegistryName: "local"}, waits: make(chan time.Duration, 1), ticks: make(chan time.Time),
		repo: index.Repo{Format: "raw", Name: "scratch", Registry: "local", Prefix: "raw/scratch", Kind: "hosted"}}
	must(t, st.CreateRepo(ctx, &l.repo))
	providers := provider.Map{"local": l.fake}
	log := slog.New(slog.DiscardHandler)
	pipe := ingest.New(ingest.Options{Providers: providers, Index: st, Log: log,
		Plugins: func(name string) (format.Plugin, bool) { return raw.Plugin{}, name == raw.Name }})
	l.w = New(Options{Index: st, Providers: providers, Inspector: pipe, Log: log})
	l.w.after = func(d time.Duration) <-chan time.Time {
		l.waits <- d
		return l.ticks
	}
	l.w.jitter = func(d time.Duration) time.Duration { return d / 4 }
	return l
}

// tick lets the loop run once and waits until it waits again, an interval.
func (l *loop) tick(t *testing.T) {
	t.Helper()
	l.ticks <- time.Now()
	if d := <-l.waits; d != time.Hour {
		t.Fatalf("the wait after a run: %s, want the interval", d)
	}
}

func (l *loop) audit(t *testing.T, action string) []index.AuditEvent {
	t.Helper()
	events, err := l.st.ListAudit(t.Context(), index.AuditFilter{Action: action})
	must(t, err)
	return events
}

// latest is the latest run of every repo.
func (l *loop) latest(t *testing.T) index.ReindexRun {
	t.Helper()
	run, err := l.st.LatestReindexRun(t.Context(), 0)
	must(t, err)
	return *run
}

func (l *loop) wantPackage(t *testing.T, name string) {
	t.Helper()
	if _, err := l.st.GetPackage(t.Context(), l.repo.ID, name); err != nil {
		t.Errorf("package %s after the tick: %v", name, err)
	}
}

// TestReconcileOff checks that an interval of 0 turns the loop off.
func TestReconcileOff(t *testing.T) {
	w := New(Options{})
	w.after = func(time.Duration) <-chan time.Time {
		t.Fatal("the loop waited")
		return nil
	}
	w.Reconcile(t.Context(), 0)
}

// pushRaw pushes the raw manifest of content at path straight to the fake registry.
func pushRaw(t *testing.T, fake *providertest.Fake, repo index.Repo, path, content string) {
	t.Helper()
	ctx := t.Context()
	built, err := raw.Plugin{}.Build(ctx, repo, format.Upload{Params: map[string]string{raw.ParamPath: path},
		Files: []format.UploadedFile{{Name: path, Size: int64(len(content)),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(content)), nil }}}})
	must(t, err)
	rr, err := fake.Repository(ctx, built.RepoPath)
	must(t, err)
	for _, b := range built.Blobs {
		rc, err := b.Open(ctx)
		must(t, err)
		must(t, rr.Push(ctx, b.Descriptor, rc))
		_ = rc.Close()
	}
	b, d, err := profile.Encode(built.Manifest)
	must(t, err)
	must(t, rr.PushReference(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: int64(len(b))},
		bytes.NewReader(b), built.Tag))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

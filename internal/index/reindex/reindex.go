// Package reindex rebuilds the derived index tables from the manifests in the registries
// (docs/architecture.md, flow E; docs/spec/index-schema.md, Invariants): the proof that the
// index is derived, the way to attach an existing prefix, and the repair of drift. A run marks
// and sweeps: every row it writes or sees gets the time of the write, a known digest is seen
// without being read again, and the rows a run did not see are looked up in the registry once
// more before they go. A run holds the lock of its scope in reindex_runs while it beats.
package reindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// The timing of runs (docs/spec/index-schema.md, Coordination): a run beats every
// heartbeatEvery, and one whose heartbeat is older than staleAfter is taken over.
const (
	heartbeatEvery = 30 * time.Second
	staleAfter     = 2 * time.Minute
)

// defaultConcurrency is Options.Concurrency when it is zero.
const defaultConcurrency = 8

// defaultClaimTTL is Options.ClaimTTL when it is zero: ingest's default.
const defaultClaimTTL = 10 * time.Minute

// fetchClaimStale is when the claim of a proxy fetch is stale: 2 minutes after its last beat
// (docs/spec/index-schema.md, Proxy and group repos). A run deletes the stale ones, as it does
// the stale tag claims.
const fetchClaimStale = 2 * time.Minute

// Options configures a Worker.
type Options struct {
	// Index is the index database.
	Index index.Store
	// Providers resolves repo.Registry to the backend registry.
	Providers provider.Set
	// Inspector checks a manifest and returns its rows: the ingest pipeline.
	Inspector Inspector
	// Audit records the start of a run; nil means the event goes to the index.
	Audit audit.Writer
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
	// Concurrency is how many manifests are read at once per registry; zero means 8.
	Concurrency int
	// ClaimTTL is ingest.claimTTL: tag claims older than that are deleted. Zero means 10
	// minutes.
	ClaimTTL time.Duration
}

// Inspector checks a manifest found in the registry and returns the rows to write; the ingest
// pipeline implements it (ingest.Pipeline.Inspect).
type Inspector interface {
	Inspect(ctx context.Context, repo index.Repo, pkgPath, tag string, m ocispec.Manifest, raw []byte, o ingest.InspectOptions) (*ingest.Rows, error)
}

var _ Inspector = (*ingest.Pipeline)(nil)

// Worker runs reindex.
type Worker struct {
	index       index.Store
	providers   provider.Set
	inspector   Inspector
	audit       audit.Writer
	now         func() time.Time
	log         *slog.Logger
	concurrency int
	claimTTL    time.Duration
	heartbeat   time.Duration                        // heartbeatEvery; shorter in the tests
	after       func(time.Duration) <-chan time.Time // time.After; the tests' clock
	jitter      func(time.Duration) time.Duration    // randomPart; the tests' choice
}

// New returns a Worker with the defaults of Options applied.
func New(o Options) *Worker {
	w := &Worker{index: o.Index, providers: o.Providers, inspector: o.Inspector, audit: o.Audit, now: o.Now, log: o.Log,
		concurrency: o.Concurrency, claimTTL: o.ClaimTTL, heartbeat: heartbeatEvery, after: time.After, jitter: randomPart}
	if w.now == nil {
		w.now = time.Now
	}
	if w.log == nil {
		w.log = slog.Default()
	}
	if w.concurrency <= 0 {
		w.concurrency = defaultConcurrency
	}
	if w.claimTTL <= 0 {
		w.claimTTL = defaultClaimTTL
	}
	return w
}

// Request is what a run does.
type Request struct {
	// Repo is the repo to reindex; nil means every repo.
	Repo *index.Repo
	// Prune sweeps the rows the run did not see (the CLI's default for a run of every repo).
	Prune bool
	// Verify reads and checks every manifest, known digests too (after a plugin fix).
	Verify bool
	// Source is cli, api or reconcile.
	Source string
	// Principal is the canonical form of who started the run.
	Principal string
	// Repositories are OCI repositories of Repo that the run reads besides those the registry
	// lists: the first attach of a prefix on a registry without a catalog, whose oci_repos has
	// no row of them yet (#37). They are names below the registry's instance prefix and under
	// the repo's path, as oci_repos.path holds them. One the run reads is recorded in oci_repos,
	// so later runs and the reconcile loop find it without the list. They need Repo.
	Repositories []string
}

// errTakenOver is the cause of a run whose row another start closed as abandoned.
var errTakenOver = errors.New("reindex: the run was taken over: its heartbeat was late and a later start closed it")

// Start takes the lock of the run's scope (index.Store.StartReindexRun) and writes the audit
// event repo.reindex, except for a run of the reconcile loop, which audits what it changed
// instead. A run that overlaps is a *index.ReindexConflictError; Repositories that are not the
// repo's are refused before. The returned run is what Run takes.
func (w *Worker) Start(ctx context.Context, req Request) (*index.ReindexRun, error) {
	if err := checkRepositories(req); err != nil {
		return nil, err
	}
	run := index.ReindexRun{Source: req.Source, Principal: req.Principal, Replica: replica(),
		Options: index.ReindexOptions{Prune: req.Prune, Verify: req.Verify}, StartedAt: w.now()}
	if req.Repo != nil {
		id := req.Repo.ID
		run.RepoID = &id
	}
	if err := w.index.StartReindexRun(ctx, &run, run.StartedAt.Add(-staleAfter)); err != nil {
		return nil, fmt.Errorf("reindex: start: %w", err)
	}
	if req.Source == SourceReconcile {
		return &run, nil
	}
	ev := index.AuditEvent{At: run.StartedAt, Principal: req.Principal, Action: "repo.reindex", Subject: "run " + itoa(run.ID)}
	if req.Repo != nil {
		ev.Repo = req.Repo.Format + "/" + req.Repo.Name
	}
	details := map[string]any{"run": run.ID, "source": req.Source, "prune": req.Prune, "verify": req.Verify}
	if len(req.Repositories) > 0 {
		details["attached"] = len(req.Repositories)
	}
	ev.Details, _ = json.Marshal(details)
	w.writeAudit(ctx, ev)
	return &run, nil
}

// checkRepositories refuses Request.Repositories without a Repo, and a name that is not below
// the repo's path.
func checkRepositories(req Request) error {
	if len(req.Repositories) == 0 {
		return nil
	}
	if req.Repo == nil {
		return errors.New("reindex: the OCI repositories to attach need the repo they belong to")
	}
	base := repoBase(*req.Repo) + "/"
	for _, name := range req.Repositories {
		if !strings.HasPrefix(name, base) || name == base {
			return fmt.Errorf("reindex: %q is not an OCI repository of %s/%s: its name must start with %s",
				name, req.Repo.Format, req.Repo.Name, base)
		}
	}
	return nil
}

// writeAudit records ev with the audit writer, or in the index without one; a failure is logged.
func (w *Worker) writeAudit(ctx context.Context, ev index.AuditEvent) {
	var err error
	if w.audit != nil {
		err = w.audit.Write(ctx, ev)
	} else {
		err = w.index.AppendAudit(ctx, &ev)
	}
	if err != nil {
		w.log.ErrorContext(ctx, "reindex: audit event not written", "action", ev.Action, "subject", ev.Subject, "error", err.Error())
	}
}

// itoa formats an id.
func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// Run reads the registry for the run that Start returned, beats while it runs, and finishes
// the run whatever happens: done when it went to the end, failed on an error of its own (the
// database, the context), and nothing when another start took it over. The error is the run's
// own; the manifests that failed are in the report.
func (w *Worker) Run(ctx context.Context, run *index.ReindexRun, req Request) (*Report, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := w.beat(ctx, run.ID, cancel)
	report, err := w.run(ctx, run, req)
	stop()
	if errors.Is(context.Cause(ctx), errTakenOver) {
		err = errTakenOver
	}
	w.finish(context.WithoutCancel(ctx), run, report, err)
	return report, err
}

// run reindexes the repos of req, one after the other.
func (w *Worker) run(ctx context.Context, run *index.ReindexRun, req Request) (*Report, error) {
	report := &Report{Repos: []RepoReport{}}
	repos := []index.Repo{}
	if req.Repo != nil {
		repos = append(repos, *req.Repo)
	} else {
		var err error
		if repos, err = w.index.ListRepos(ctx, ""); err != nil {
			return report, fmt.Errorf("reindex: list the repos: %w", err)
		}
	}
	if _, err := w.index.ExpireTagClaims(ctx, w.now().Add(-w.claimTTL)); err != nil {
		return report, fmt.Errorf("reindex: expire stale tag claims: %w", err)
	}
	if _, err := w.index.ExpireFetchClaims(ctx, w.now().Add(-fetchClaimStale)); err != nil {
		return report, fmt.Errorf("reindex: expire stale fetch claims: %w", err)
	}
	for _, repo := range repos {
		rr, err := w.repo(ctx, repo, run.StartedAt, req)
		report.Repos = append(report.Repos, rr)
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

// beat sets the run's heartbeat every w.heartbeat until stop is called, and cancels the run
// with errTakenOver when the row is not running any more.
func (w *Worker) beat(ctx context.Context, id int64, cancel context.CancelCauseFunc) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(w.heartbeat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			err := w.index.HeartbeatReindexRun(ctx, id, w.now())
			switch {
			case errors.Is(err, index.ErrNotFound):
				w.log.ErrorContext(ctx, "reindex: the run was closed by a later start; stopping", "run", id)
				cancel(errTakenOver)
				return
			case err != nil && ctx.Err() == nil:
				w.log.WarnContext(ctx, "reindex: heartbeat not written", "run", id, "error", err.Error())
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// finish stores the outcome of a run.
func (w *Worker) finish(ctx context.Context, run *index.ReindexRun, report *Report, err error) {
	run.State, run.Error = index.ReindexDone, ""
	if err != nil {
		run.State, run.Error = index.ReindexFailed, err.Error()
	}
	if report != nil {
		run.Report, _ = json.Marshal(report) // plain structs of strings and numbers
	}
	now := w.now()
	run.FinishedAt = &now
	if errors.Is(err, errTakenOver) {
		return // the row is abandoned already
	}
	if ferr := w.index.FinishReindexRun(ctx, run); ferr != nil {
		w.log.ErrorContext(ctx, "reindex: the outcome of a run not written", "run", run.ID, "state", run.State, "error", ferr.Error())
	}
}

// replica names this process for reindex_runs.replica: host name and process id.
func replica() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return host + " " + strconv.Itoa(os.Getpid())
}

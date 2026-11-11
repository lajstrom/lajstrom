package reindex_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// The answers of a registry to the manifest of a version by its digest, for TestPrune.
var (
	errManifestUnknown = fmt.Errorf("HTTP 404, MANIFEST_UNKNOWN: %w: %w", provider.ErrManifestUnknown, provider.ErrNotFound)
	errNameUnknown     = fmt.Errorf("HTTP 404, NAME_UNKNOWN: %w", provider.ErrNotFound)
	errNoCode          = fmt.Errorf("HTTP 404: %w", provider.ErrNotFound)
	errServer          = errors.New("HTTP 503")
	errDenied          = fmt.Errorf("HTTP 403, DENIED: %w", provider.ErrUnauthorized)
	errTimeout         = fmt.Errorf("stat manifest: %w", context.DeadlineExceeded)
)

// TestPrune is the prune of #446 on a registry that lags or fails: a version that the catalog
// does not list and whose tag does not resolve is removed only after the registry answered
// MANIFEST_UNKNOWN to its digest twice, at least 10 minutes apart, with no other answer in
// between, and once it is older than the grace age. Each step runs reindex with prune at a
// time after the publish, with the registry's answer to the manifest; nil is the manifest.
func TestPrune(t *testing.T) {
	type step struct {
		at      time.Duration // after the publish
		answer  error
		removed int
		pending int
		errors  int
	}
	tests := []struct {
		name  string
		grace time.Duration // reconcile.graceAge; 0 the default, 1 hour
		steps []step
	}{
		{name: "MANIFEST_UNKNOWN twice, 10 minutes apart", steps: []step{
			{at: 2 * time.Hour, answer: errManifestUnknown, pending: 1},
			{at: 2*time.Hour + 10*time.Minute, answer: errManifestUnknown, removed: 1},
		}},
		{name: "MANIFEST_UNKNOWN twice, 9 minutes apart, then a third", steps: []step{
			{at: 2 * time.Hour, answer: errManifestUnknown, pending: 1},
			{at: 2*time.Hour + 9*time.Minute, answer: errManifestUnknown, pending: 1},
			{at: 2*time.Hour + 10*time.Minute, answer: errManifestUnknown, removed: 1},
		}},
		{name: "MANIFEST_UNKNOWN once", steps: []step{
			{at: 2 * time.Hour, answer: errManifestUnknown, pending: 1},
		}},
		{name: "a version younger than the grace age", steps: []step{
			{at: 10 * time.Minute, answer: errManifestUnknown, pending: 1},
			{at: 59 * time.Minute, answer: errManifestUnknown, pending: 1},
			{at: 61 * time.Minute, answer: errManifestUnknown, removed: 1},
		}},
		{name: "a grace age of 24 hours", grace: 24 * time.Hour, steps: []step{
			{at: 2 * time.Hour, answer: errManifestUnknown, pending: 1},
			{at: 23 * time.Hour, answer: errManifestUnknown, pending: 1},
			{at: 24*time.Hour + time.Minute, answer: errManifestUnknown, removed: 1},
		}},
		{name: "a lagging registry: the manifest in between", steps: []step{
			{at: 2 * time.Hour, answer: errManifestUnknown, pending: 1},
			{at: 2*time.Hour + 30*time.Minute, answer: nil, errors: 1}, // seen: the grace age starts again
			{at: 3*time.Hour + 20*time.Minute, answer: errManifestUnknown, pending: 1},
			{at: 3*time.Hour + 40*time.Minute, answer: errManifestUnknown, removed: 1},
		}},
		{name: "a failure in between starts over", steps: []step{
			{at: 2 * time.Hour, answer: errManifestUnknown, pending: 1},
			{at: 2*time.Hour + 5*time.Minute, answer: errServer, errors: 1},
			{at: 2*time.Hour + 12*time.Minute, answer: errManifestUnknown, pending: 1},
			{at: 2*time.Hour + 22*time.Minute, answer: errManifestUnknown, removed: 1},
		}},
		{name: "NAME_UNKNOWN", steps: []step{
			{at: 2 * time.Hour, answer: errNameUnknown, errors: 1},
			{at: 3 * time.Hour, answer: errNameUnknown, errors: 1},
		}},
		{name: "a 404 without a code", steps: []step{
			{at: 2 * time.Hour, answer: errNoCode, errors: 1},
			{at: 3 * time.Hour, answer: errNoCode, errors: 1},
		}},
		{name: "a 5xx", steps: []step{
			{at: 2 * time.Hour, answer: errServer, errors: 1},
			{at: 3 * time.Hour, answer: errServer, errors: 1},
		}},
		{name: "a 403", steps: []step{
			{at: 2 * time.Hour, answer: errDenied, errors: 1},
			{at: 3 * time.Hour, answer: errDenied, errors: 1},
		}},
		{name: "a timeout", steps: []step{
			{at: 2 * time.Hour, answer: errTimeout, errors: 1},
			{at: 3 * time.Hour, answer: errTimeout, errors: 1},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			e := newEnv(t)
			if tt.grace != 0 {
				e.w = reindex.New(reindex.Options{Index: e.st, Providers: provider.Map{"local": e.prov}, Inspector: e.inspect,
					Log: slog.New(slog.DiscardHandler), Now: e.clock.now, GraceAge: tt.grace})
			}
			pub := e.publish(t, "lagging", "l")
			e.publish(t, "steady", "s")
			var answer error
			e.prov.hide, e.prov.resolveIn = pub.RepoPath, pub.RepoPath
			e.prov.failResolve = fmt.Errorf("HTTP 404, MANIFEST_UNKNOWN: %w", provider.ErrNotFound)
			e.fake.FailStatManifest = func(name string, d digest.Digest) error {
				if name != pub.RepoPath || d != pub.Digest {
					t.Errorf("StatManifest of %s@%s, want %s@%s", name, d, pub.RepoPath, pub.Digest)
				}
				return answer
			}
			ahead := time.Duration(0)
			for i, s := range tt.steps {
				e.clock.later(s.at - ahead)
				ahead, answer = s.at, s.answer
				r := e.run(t, reindex.Request{Prune: true})
				if r.Removed != s.removed || r.Pending != s.pending || len(r.Errors) != s.errors || r.Unchanged != 1 ||
					len(r.RepositoryUnknown) != 0 { // the repository's tag list answers: NAME_UNKNOWN alone does not make it unknown
					t.Errorf("step %d, %s after the publish: %+v; want removed %d, pending %d, errors %d", i+1, s.at,
						*r, s.removed, s.pending, s.errors)
				}
				_, err := e.st.GetPackage(ctx, e.repo.ID, "lagging")
				if gone := errors.Is(err, index.ErrNotFound); gone != (s.removed == 1) || (err != nil && !gone) {
					t.Errorf("step %d: the package after the run: %v", i+1, err)
				}
			}
		})
	}
}

// TestRepositoryUnknown is #567: the versions of an OCI repository deleted in the registry
// (Zot answers NAME_UNKNOWN to its manifests and its tag list, as the fake does) are not
// removed by prune, past the grace age and on every run, and not reported as errors either: each
// is listed in RepositoryUnknown with the request of the API that deletes it, and the run logs
// one warning for the repository. Deleted through ingest, as that request does, they are gone,
// and so is the report.
func TestRepositoryUnknown(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	var logs bytes.Buffer
	e.w = reindex.New(reindex.Options{Index: e.st, Providers: e.providers, Inspector: e.inspect,
		Log: slog.New(slog.NewTextHandler(&logs, nil)), Now: e.clock.now})
	one, two := e.publish(t, "docs/gone.txt", "one"), e.publish(t, "docs/gone.txt", "two")
	e.publish(t, "docs/kept.txt", "kept")
	must(t, e.fake.DeleteRepository(ctx, one.RepoPath))
	want := []reindex.RepositoryUnknown{}
	for _, p := range []*format.Published{one, two} {
		want = append(want, reindex.RepositoryUnknown{Repository: "docs/gone.txt", Package: "docs/gone.txt", Version: p.Entry.Version,
			Tag: p.Tag, Digest: p.Digest, Delete: "DELETE /api/v1/repos/raw/scratch/packages/docs%2Fgone.txt/versions/" + p.Entry.Version})
	}
	byTag := func(a, b reindex.RepositoryUnknown) int { return strings.Compare(a.Tag, b.Tag) }
	slices.SortFunc(want, byTag)

	if r := e.run(t, reindex.Request{}); len(r.RepositoryUnknown) != 0 || len(r.Errors) != 0 {
		t.Errorf("a run without prune, which looks up nothing it did not see: %+v", *r)
	}
	for _, later := range []time.Duration{2 * time.Hour, time.Hour} {
		e.clock.later(later)
		logs.Reset()
		r := e.run(t, reindex.Request{Prune: true})
		if r.Removed != 0 || r.Pending != 0 || len(r.Errors) != 0 || r.Unchanged != 1 {
			t.Errorf("a run %s later: %+v; want the versions kept, neither pending nor errors", later, *r)
		}
		got := slices.SortedFunc(slices.Values(r.RepositoryUnknown), byTag)
		same(t, "the versions of the repository the registry does not know", want, got)
		if n := strings.Count(logs.String(), "the registry does not know the OCI repository"); n != 1 ||
			!strings.Contains(logs.String(), "versions=2") {
			t.Errorf("logged %d warnings, want one for the repository with its 2 versions:\n%s", n, logs.String())
		}
	}
	if _, err := e.st.GetPackage(ctx, e.repo.ID, "docs/gone.txt"); err != nil {
		t.Fatalf("the package after the runs: %v, want it kept", err)
	}

	for _, p := range []*format.Published{one, two} {
		_, err := e.pipe.Delete(ctx, e.repo, format.Deletion{Path: "docs/gone.txt", Reference: p.Digest.String(),
			Condition: &format.Condition{Version: p.Entry.Version}}, alice) // as the API's DELETE of the version does
		must(t, err)
	}
	if r := e.run(t, reindex.Request{Prune: true}); len(r.RepositoryUnknown) != 0 || len(r.Errors) != 0 || r.Unchanged != 1 {
		t.Errorf("the run after the deletes: %+v", *r)
	}
	if _, err := e.st.GetPackage(ctx, e.repo.ID, "docs/gone.txt"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the package after the deletes: %v, want ErrNotFound", err)
	}
}

// TestRepositoryUnknownAskedOnce: a run asks the tag list of an OCI repository whose manifests
// are a 404 without MANIFEST_UNKNOWN once, whatever it answers, and not again for its next
// version: a 404 lists both versions in RepositoryUnknown, a failure keeps both errors (#567).
func TestRepositoryUnknownAskedOnce(t *testing.T) {
	for _, tt := range []struct {
		name            string
		failTags        error
		unknown, errors int
	}{
		{name: "a 404", unknown: 2},
		{name: "a 5xx", failTags: errServer, errors: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			gone := e.publish(t, "docs/gone.txt", "one")
			e.publish(t, "docs/gone.txt", "two")
			e.publish(t, "docs/kept.txt", "kept")
			must(t, e.fake.DeleteRepository(t.Context(), gone.RepoPath))
			e.prov.failTags, e.prov.tagsIn = tt.failTags, gone.RepoPath
			e.clock.later(2 * time.Hour)
			r := e.run(t, reindex.Request{Prune: true})
			if r.Removed != 0 || r.Pending != 0 || len(r.RepositoryUnknown) != tt.unknown || len(r.Errors) != tt.errors {
				t.Errorf("the run: %+v; want %d in RepositoryUnknown, %d errors, nothing removed", *r, tt.unknown, tt.errors)
			}
			if n := e.prov.tagLists[gone.RepoPath]; n != 1 {
				t.Errorf("the tag list of %s asked %d times in the run, want once", gone.RepoPath, n)
			}
		})
	}
}

// TestPruneTwoReplicas is the prune of two replicas' reconcile loops on one database: runs that
// overlap are refused, and the answers MANIFEST_UNKNOWN that one replica got count for the
// other, but two runs that follow each other within 10 minutes are not two answers.
func TestPruneTwoReplicas(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	pub := e.publish(t, "gone", "g")
	rr, err := e.fake.Repository(ctx, pub.RepoPath)
	must(t, err)
	desc, err := rr.Resolve(ctx, pub.Tag)
	must(t, err)
	must(t, rr.Delete(ctx, desc))
	a, b := e.w, reindex.New(reindex.Options{Index: e.st, Providers: provider.Map{"local": e.prov}, Inspector: e.inspect,
		Log: slog.New(slog.DiscardHandler), Now: e.clock.now})
	req := reindex.Request{Prune: true, Source: reindex.SourceReconcile, Principal: "anonymous"}

	held, err := a.Start(ctx, req)
	must(t, err)
	var conflict *index.ReindexConflictError
	if _, err := b.Start(ctx, req); !errors.As(err, &conflict) || conflict.Running.ID != held.ID {
		t.Fatalf("B's start while A runs: %v, want the conflict of run %d", err, held.ID)
	}
	run := func(w *reindex.Worker, name string, removed, pending int) {
		t.Helper()
		r, err := w.Run(ctx, held, req)
		must(t, err)
		if got := r.Repos[0]; got.Removed != removed || got.Pending != pending {
			t.Errorf("%s: %+v; want removed %d, pending %d", name, got, removed, pending)
		}
	}
	e.clock.later(2 * time.Hour)
	run(a, "A, the first answer", 0, 1)
	e.clock.later(time.Minute)
	held, err = b.Start(ctx, req)
	must(t, err)
	run(b, "B a minute later", 0, 1)
	e.clock.later(4 * time.Minute)
	held, err = a.Start(ctx, req)
	must(t, err)
	run(a, "A five minutes after its first", 0, 1)
	e.clock.later(5 * time.Minute)
	held, err = b.Start(ctx, req)
	must(t, err)
	run(b, "B ten minutes after A's first", 1, 0)
	if _, err := e.st.GetPackage(ctx, e.repo.ID, "gone"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the package after B's run: %v, want ErrNotFound", err)
	}
}

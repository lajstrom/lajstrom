package storetest

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The parts of the store that reindex relies on (#29; docs/spec/index-schema.md): the earliest
// version's time on the package, the degraded flag, the sweep (TouchVersions,
// ListUnseenVersions, DeleteEmptyPackages) and the runs with their lock.

// testPackageCreatedAt checks that every version write and delete keeps the package's CreatedAt
// the earliest CreatedAt of its versions, and that a package without versions keeps its own.
func testPackageCreatedAt(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "created")
	p := index.Package{RepoID: repo.ID, Name: "tool", NameOCI: "tool", CreatedAt: at(100)}
	must(t, s.UpsertPackage(ctx, &p))
	created := func(what string, want int) {
		t.Helper()
		got, err := s.GetPackage(ctx, repo.ID, "tool")
		must(t, err)
		same(t, what, at(want), got.CreatedAt)
	}
	version := func(v string, sec int) index.Version {
		t.Helper()
		ver := index.Version{PackageID: p.ID, Version: v, Tag: v, Digest: dg(v), ArtifactType: "a", CreatedAt: at(sec)}
		must(t, s.UpsertVersion(ctx, &ver))
		return ver
	}
	created("a package without versions", 100)
	v2 := version("2.0", 200)
	created("after the first version, later than the package", 200)
	v1 := version("1.0", 50)
	created("after an earlier version", 50)
	version("3.0", 300)
	created("after a later version", 50)
	again := v1
	again.IndexedAt = at(400)
	must(t, s.UpsertVersion(ctx, &again))
	created("after a refresh", 50)

	must(t, s.DeleteVersion(ctx, v1.ID))
	created("after the earliest version is deleted", 200)
	must(t, s.DeleteVersion(ctx, v2.ID))
	created("after the next one is deleted", 300)
	versions, err := s.ListVersions(ctx, p.ID)
	must(t, err)
	must(t, s.DeleteVersion(ctx, versions[0].ID))
	created("after the last version is deleted", 300)
	wantErr(t, "DeleteVersion of a missing version", s.DeleteVersion(ctx, v1.ID), index.ErrNotFound)

	conflict := index.Version{PackageID: p.ID, Version: "4.0", Tag: "4.0", Digest: dg("4.0"), ArtifactType: "a", CreatedAt: at(10)}
	must(t, s.UpsertVersion(ctx, &conflict))
	refused := index.Version{PackageID: p.ID, Version: "4.0", Tag: "4.0", Digest: dg("other"), ArtifactType: "a", CreatedAt: at(1)}
	if err := s.UpsertVersion(ctx, &refused); !errors.Is(err, index.ErrConflict) {
		t.Fatalf("UpsertVersion of a tag with another digest: %v, want ErrConflict", err)
	}
	created("after a refused version", 10)
}

// testDegraded checks that Degraded is stored on insert and on a refresh.
func testDegraded(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "apt", "degraded")
	p := newPackage(t, s, repo.ID, "hello")
	v := index.Version{PackageID: p.ID, Version: "1.0", Tag: "1.0", Digest: dg("1.0"), ArtifactType: "a", Degraded: true}
	must(t, s.UpsertVersion(ctx, &v))
	got, err := s.GetVersionByTag(ctx, p.ID, "1.0")
	must(t, err)
	same(t, "a degraded version", v, *got)
	v.Degraded, v.Metadata = false, json.RawMessage(`{"control":{}}`)
	must(t, s.UpsertVersion(ctx, &v))
	got, err = s.GetVersionByTag(ctx, p.ID, "1.0")
	must(t, err)
	same(t, "the version read again with its metadata", v, *got)
}

// testSweep checks TouchVersions, ListUnseenVersions and DeleteEmptyPackages: what reindex
// marks, what it finds unseen, and the packages it removes once their versions are gone.
func testSweep(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo, other := newRepo(t, s, "raw", "sweep"), newRepo(t, s, "raw", "other")
	pkgB, pkgA := newPackage(t, s, repo.ID, "b"), newPackage(t, s, repo.ID, "a")
	version := func(pkg index.Package, v string) index.Version {
		t.Helper()
		ver := index.Version{PackageID: pkg.ID, Version: v, Tag: v, Digest: dg(pkg.Name + v), ArtifactType: "a",
			CreatedAt: at(0), IndexedAt: at(10)}
		must(t, s.UpsertVersion(ctx, &ver))
		return ver
	}
	b1, a1, a2 := version(pkgB, "1"), version(pkgA, "1"), version(pkgA, "2")
	elsewhere := version(newPackage(t, s, other.ID, "a"), "1")
	pkgA.CreatedAt, pkgB.CreatedAt = at(0), at(0)

	unseen, err := s.ListUnseenVersions(ctx, repo.ID, at(10))
	must(t, err)
	same(t, "nothing is older than the run", []index.VersionMatch{}, unseen)

	// Ids past one statement's batch, most of them missing, are skipped.
	touch := []int64{b1.ID, a2.ID, elsewhere.ID}
	for i := range 1200 {
		touch = append(touch, int64(1_000_000+i))
	}
	must(t, s.TouchVersions(ctx, touch, at(20)))
	must(t, s.TouchVersions(ctx, nil, at(30)))
	unseen, err = s.ListUnseenVersions(ctx, repo.ID, at(20))
	must(t, err)
	same(t, "the one row the run did not see", []index.VersionMatch{{Version: a1, Package: pkgA}}, unseen)

	must(t, s.TouchVersions(ctx, []int64{a1.ID}, at(5)))
	unseen, err = s.ListUnseenVersions(ctx, repo.ID, at(25))
	must(t, err)
	b1.IndexedAt, a1.IndexedAt, a2.IndexedAt = at(20), at(5), at(20)
	same(t, "every row, by package name and id", []index.VersionMatch{
		{Version: a1, Package: pkgA}, {Version: a2, Package: pkgA}, {Version: b1, Package: pkgB},
	}, unseen)
	got, err := s.GetVersionByTag(ctx, pkgA.ID, "2")
	must(t, err)
	same(t, "TouchVersions changes IndexedAt only", a2, *got)

	empty := newPackage(t, s, other.ID, "empty")
	must(t, s.DeleteVersion(ctx, b1.ID))
	n, err := s.DeleteEmptyPackages(ctx, repo.ID)
	must(t, err)
	if n != 1 {
		t.Errorf("DeleteEmptyPackages removed %d packages, want 1", n)
	}
	pkgs, err := s.ListPackages(ctx, repo.ID, "")
	must(t, err)
	same(t, "the packages left", []string{"a"}, packageNames(pkgs))
	if _, err := s.GetPackage(ctx, other.ID, empty.Name); err != nil {
		t.Errorf("the empty package of another repo: %v", err)
	}
}

// packageNames returns the names of packages.
func packageNames(pkgs []index.Package) []string {
	out := []string{}
	for _, p := range pkgs {
		out = append(out, p.Name)
	}
	return out
}

// startRun starts a run of repoID (nil: every repo) with heartbeat sec, stale before staleSec.
func startRun(t *testing.T, s index.Writer, repoID *int64, sec, staleSec int) (index.ReindexRun, error) {
	t.Helper()
	r := index.ReindexRun{RepoID: repoID, Source: "cli", Principal: "user:admin", Replica: "host-" + strconv.Itoa(sec),
		StartedAt: at(sec), HeartbeatAt: at(sec)}
	err := s.StartReindexRun(t.Context(), &r, at(staleSec))
	return r, err
}

// wantRunConflict fails the test unless err is a *index.ReindexConflictError naming running.
func wantRunConflict(t *testing.T, what string, err error, running int64) {
	t.Helper()
	var conflict *index.ReindexConflictError
	switch {
	case !errors.As(err, &conflict):
		t.Errorf("%s: error = %v, want a *index.ReindexConflictError", what, err)
	case !errors.Is(err, index.ErrConflict):
		t.Errorf("%s: %v does not wrap ErrConflict", what, err)
	case conflict.Running.ID != running:
		t.Errorf("%s: the conflict names run %d, want %d", what, conflict.Running.ID, running)
	}
}

// testReindexRuns checks a run's life (start, heartbeat, finish, read back), the overlap of
// scopes, the takeover of a stale run and the latest run of a repo.
func testReindexRuns(t *testing.T, s index.Store) {
	ctx := t.Context()
	a, b := newRepo(t, s, "raw", "a"), newRepo(t, s, "raw", "b")
	if _, err := s.LatestReindexRun(ctx, a.ID); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("LatestReindexRun before any run: %v, want ErrNotFound", err)
	}

	all := index.ReindexRun{Source: "api", Principal: "user:admin", Replica: "lajstrom-a 7",
		Options: index.ReindexOptions{Prune: true}, StartedAt: at(0)}
	must(t, s.StartReindexRun(ctx, &all, at(-120)))
	if all.ID == 0 || all.State != index.ReindexRunning || all.HeartbeatAt != at(0) {
		t.Fatalf("StartReindexRun: %+v", all)
	}
	got, err := s.GetReindexRun(ctx, all.ID)
	must(t, err)
	same(t, "the running run", all, *got)

	_, err = startRun(t, s, nil, 10, -110)
	wantRunConflict(t, "a second run of every repo", err, all.ID)
	_, err = startRun(t, s, &a.ID, 10, -110)
	wantRunConflict(t, "a run of a repo while every repo runs", err, all.ID)

	must(t, s.HeartbeatReindexRun(ctx, all.ID, at(60)))
	all.HeartbeatAt = at(60)
	all.State, all.Report = index.ReindexDone, json.RawMessage(`{"repos": [{"repo": "raw/a", "added": 2}]}`)
	all.FinishedAt = ptr(at(70))
	must(t, s.FinishReindexRun(ctx, &all))
	got, err = s.GetReindexRun(ctx, all.ID)
	must(t, err)
	same(t, "the finished run", all, *got)
	wantErr(t, "HeartbeatReindexRun of a finished run", s.HeartbeatReindexRun(ctx, all.ID, at(80)), index.ErrNotFound)
	wantErr(t, "FinishReindexRun of a finished run", s.FinishReindexRun(ctx, &all), index.ErrNotFound)
	notRunning := all
	notRunning.State = index.ReindexRunning
	if err := s.FinishReindexRun(ctx, &notRunning); err == nil {
		t.Error("FinishReindexRun in state running: no error")
	}

	runA, err := startRun(t, s, &a.ID, 100, -20)
	must(t, err)
	runB, err := startRun(t, s, &b.ID, 100, -20)
	must(t, err)
	_, err = startRun(t, s, &a.ID, 110, -10)
	wantRunConflict(t, "a second run of a repo", err, runA.ID)
	_, err = startRun(t, s, nil, 110, -10)
	wantRunConflict(t, "a run of every repo while a repo runs", err, runA.ID)

	latest, err := s.LatestReindexRun(ctx, a.ID)
	must(t, err)
	same(t, "the latest run of a: its own", runA, *latest)
	latest, err = s.LatestReindexRun(ctx, 0)
	must(t, err)
	same(t, "the latest run of every repo", all.ID, latest.ID)

	// The heartbeat of a stopped at 100: a start that counts it stale closes it.
	runA2, err := startRun(t, s, &a.ID, 300, 150)
	must(t, err)
	got, err = s.GetReindexRun(ctx, runA.ID)
	must(t, err)
	if got.State != index.ReindexAbandoned || got.FinishedAt == nil || !strings.HasPrefix(got.Error, "no heartbeat since ") {
		t.Errorf("the stale run: state %q, finished %v, error %q", got.State, got.FinishedAt, got.Error)
	}
	wantErr(t, "HeartbeatReindexRun of an abandoned run", s.HeartbeatReindexRun(ctx, runA.ID, at(301)), index.ErrNotFound)
	if _, err := s.GetReindexRun(ctx, runB.ID); err != nil {
		t.Fatal(err)
	}
	latest, err = s.LatestReindexRun(ctx, b.ID)
	must(t, err)
	same(t, "b's run is untouched", runB, *latest)
	latest, err = s.LatestReindexRun(ctx, a.ID)
	must(t, err)
	same(t, "the latest run of a: the new one", runA2.ID, latest.ID)

	must(t, s.DeleteRepo(ctx, b.ID))
	wantErr(t, "a run of a deleted repo", func() error { _, err := s.GetReindexRun(ctx, runB.ID); return err }(), index.ErrNotFound)
}

// testReindexRunsKept checks that finishing a run keeps the ten newest finished runs of its
// scope and touches neither the running run nor other scopes.
func testReindexRunsKept(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo, other := newRepo(t, s, "raw", "kept"), newRepo(t, s, "raw", "other")
	otherRun, err := startRun(t, s, &other.ID, 0, -1)
	must(t, err)
	otherRun.State = index.ReindexFailed
	otherRun.Error = "registry unreachable"
	must(t, s.FinishReindexRun(ctx, &otherRun))
	var runs []index.ReindexRun
	for i := range 12 {
		r, err := startRun(t, s, &repo.ID, 10*(i+1), -1)
		must(t, err)
		r.State = index.ReindexDone
		must(t, s.FinishReindexRun(ctx, &r))
		runs = append(runs, r)
	}
	for i, r := range runs {
		_, err := s.GetReindexRun(ctx, r.ID)
		if i < 2 {
			wantErr(t, "run "+strconv.Itoa(i)+", beyond the ten newest", err, index.ErrNotFound)
		} else if err != nil {
			t.Errorf("run %d, one of the ten newest: %v", i, err)
		}
	}
	got, err := s.GetReindexRun(ctx, otherRun.ID)
	must(t, err)
	same(t, "the run of another scope", otherRun, *got)
}

// testReindexRunRace starts runs of one repo from several writers at once: exactly one runs,
// every other one gets the conflict that names it.
func testReindexRunRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "raw", "race")
	const writers = 8
	errs := make([]error, writers)
	started := make([]index.ReindexRun, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			started[i] = index.ReindexRun{RepoID: &repo.ID, Source: "api", Principal: "user:w", Replica: "r" + strconv.Itoa(i)}
			errs[i] = s.StartReindexRun(ctx, &started[i], at(-3600))
		})
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner >= 0 {
				t.Fatalf("writers %d and %d both started a run", winner, i)
			}
			winner = i
		}
	}
	if winner < 0 {
		t.Fatalf("no writer started a run: %v", errs)
	}
	for i, err := range errs {
		if i != winner {
			wantRunConflict(t, "writer "+strconv.Itoa(i), err, started[winner].ID)
		}
	}
}

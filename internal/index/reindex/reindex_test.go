package reindex_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

var alice = auth.Principal{Kind: auth.KindUser, Name: "alice"}

// env is one repo raw/scratch on a fake registry, the ingest pipeline with the raw plugin, and
// a worker whose inspector counts its calls.
type env struct {
	st      *sqlite.Store
	fake    *providertest.Fake
	prov    *wrapped
	pipe    *ingest.Pipeline
	inspect *counting
	w       *reindex.Worker
	repo    index.Repo
}

var memoryDBs atomic.Int64

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:reindex-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	e := &env{st: st, fake: &providertest.Fake{RegistryName: "local"}}
	e.prov = &wrapped{Fake: e.fake}
	e.repo = e.newRepo(t, "raw", "scratch")
	plugins := func(name string) (format.Plugin, bool) { return raw.Plugin{}, name == raw.Name }
	log := slog.New(slog.DiscardHandler)
	e.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": e.prov}, Index: st, Plugins: plugins, Log: log})
	e.inspect = &counting{Inspector: e.pipe}
	e.w = reindex.New(reindex.Options{Index: st, Providers: provider.Map{"local": e.prov}, Inspector: e.inspect, Log: log})
	return e
}

func (e *env) newRepo(t *testing.T, format, name string) index.Repo {
	t.Helper()
	r := index.Repo{Format: format, Name: name, Registry: "local", Prefix: format + "/" + name, Kind: "hosted"}
	must(t, e.st.CreateRepo(t.Context(), &r))
	return r
}

// publish stores content at path through ingest, as a PUT of the raw frontend does.
func (e *env) publish(t *testing.T, path, content string) *format.Published {
	t.Helper()
	pub, err := e.pipe.Upload(t.Context(), e.repo, rawUpload(path, content), alice)
	must(t, err)
	return pub
}

// bypass pushes the manifest of content at path straight to the registry, as oras would,
// without Lajstrom: blobs, then the manifest under its tag. skip names blobs not to push.
func (e *env) bypass(t *testing.T, repo index.Repo, path, content string, skip ...digest.Digest) ocispec.Descriptor {
	t.Helper()
	ctx := t.Context()
	built, err := raw.Plugin{}.Build(ctx, repo, rawUpload(path, content))
	must(t, err)
	rr, err := e.fake.Repository(ctx, built.RepoPath)
	must(t, err)
	for _, b := range built.Blobs {
		if slices.Contains(skip, b.Descriptor.Digest) {
			continue
		}
		rc, err := b.Open(ctx)
		must(t, err)
		must(t, rr.Push(ctx, b.Descriptor, rc))
		_ = rc.Close()
	}
	b, d, err := profile.Encode(built.Manifest)
	must(t, err)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: int64(len(b))}
	must(t, rr.PushReference(ctx, desc, bytes.NewReader(b), built.Tag))
	return desc
}

// pushManifest pushes any manifest or index under name:tag straight to the registry.
func (e *env) pushManifest(t *testing.T, name, tag string, v any, mediaType string) ocispec.Descriptor {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(b), Size: int64(len(b))}
	rr, err := e.fake.Repository(t.Context(), name)
	must(t, err)
	must(t, rr.PushReference(t.Context(), desc, bytes.NewReader(b), tag))
	return desc
}

func rawUpload(path, content string) format.Upload {
	return format.Upload{Params: map[string]string{raw.ParamPath: path}, Files: []format.UploadedFile{{
		Name: path, Size: int64(len(content)),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(content)), nil },
	}}}
}

// run starts and runs a reindex of the env's repo.
func (e *env) run(t *testing.T, req reindex.Request) *reindex.RepoReport {
	t.Helper()
	if req.Repo == nil {
		req.Repo = &e.repo
	}
	req.Source, req.Principal = "cli", "user:admin"
	run, err := e.w.Start(t.Context(), req)
	must(t, err)
	report, err := e.w.Run(t.Context(), run, req)
	must(t, err)
	if len(report.Repos) != 1 {
		t.Fatalf("report of %d repos", len(report.Repos))
	}
	return &report.Repos[0]
}

// derived is the derived rows of a repo by their natural keys: what reindex must restore.
type derived struct {
	Packages map[string]index.Package // by name; ID cleared
	Versions map[string]index.Version // by package and tag; ID, PackageID and IndexedAt cleared
	Files    map[string][]index.File  // by package and tag; ID and VersionID cleared
	OCIRepos []string
}

func (e *env) derived(t *testing.T) derived {
	t.Helper()
	ctx := t.Context()
	d := derived{Packages: map[string]index.Package{}, Versions: map[string]index.Version{}, Files: map[string][]index.File{}}
	pkgs, err := e.st.ListPackages(ctx, e.repo.ID, "")
	must(t, err)
	for _, p := range pkgs {
		vs, err := e.st.ListVersions(ctx, p.ID)
		must(t, err)
		for _, v := range vs {
			fs, err := e.st.ListFiles(ctx, v.ID)
			must(t, err)
			for i := range fs {
				fs[i].ID, fs[i].VersionID = 0, 0
			}
			key := p.Name + ":" + v.Tag
			d.Files[key] = fs
			v.ID, v.PackageID, v.IndexedAt = 0, 0, v.CreatedAt
			d.Versions[key] = v
		}
		p.ID = 0
		d.Packages[p.Name] = p
	}
	repos, err := e.st.ListOCIRepos(ctx, e.repo.ID)
	must(t, err)
	d.OCIRepos = []string{}
	for _, r := range repos {
		d.OCIRepos = append(d.OCIRepos, r.Path)
	}
	return d
}

// TestRebuild is I-01: what ingest wrote, dropped and rebuilt by a run from the registry, equal
// in every column but the ids and indexed_at; the index_cache of the repo is empty after it.
func TestRebuild(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.publish(t, "docs/readme.md", "hello")
	e.publish(t, "docs/readme.md", "hello again")
	e.publish(t, "bin/tool", "#!/bin/sh")
	before := e.derived(t)
	cache := index.Cache{RepoID: e.repo.ID, Scope: "", Kind: "listing", Content: []byte("old"), SHA256: "s"}
	must(t, e.st.PutIndexCache(ctx, &cache))

	must(t, e.st.ResetDerived(ctx, e.repo.ID))
	r := e.run(t, reindex.Request{Prune: true})
	if r.Added != 3 || r.Manifests != 3 || r.OCIRepositories != 2 || r.Removed != 0 || len(r.Errors) != 0 {
		t.Errorf("report: %+v", *r)
	}
	same(t, "the rebuilt rows", before, e.derived(t))
	if _, err := e.st.GetIndexCache(ctx, e.repo.ID, cache.Scope, cache.Kind); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("index_cache after the run: %v, want ErrNotFound", err)
	}
}

// TestKnownDigests checks the skip rule: a second run reads no manifest it knows, a verifying
// run reads every one again.
func TestKnownDigests(t *testing.T) {
	e := newEnv(t)
	e.publish(t, "a", "1")
	e.publish(t, "b", "2")
	r := e.run(t, reindex.Request{Prune: true})
	if r.Unchanged != 2 || r.Added != 0 || e.inspect.calls.Load() != 0 {
		t.Errorf("a run over known digests: %+v, %d inspections", *r, e.inspect.calls.Load())
	}
	r = e.run(t, reindex.Request{Verify: true})
	if r.Updated != 2 || r.Unchanged != 0 || e.inspect.calls.Load() != 2 {
		t.Errorf("a verifying run: %+v, %d inspections", *r, e.inspect.calls.Load())
	}
}

// TestBypassAndDelete is the drift test of #29: a manifest pushed straight to the backend is
// indexed; a manifest deleted in the backend loses its rows with prune, and keeps them without.
func TestBypassAndDelete(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	gone := e.publish(t, "gone.txt", "bye")
	e.bypass(t, e.repo, "pushed.txt", "by oras")
	rr, err := e.fake.Repository(ctx, gone.RepoPath)
	must(t, err)
	desc, err := rr.Resolve(ctx, gone.Tag)
	must(t, err)
	must(t, rr.Delete(ctx, desc))

	r := e.run(t, reindex.Request{})
	if r.Added != 1 || r.Removed != 0 {
		t.Errorf("without prune: %+v", *r)
	}
	if _, err := e.st.GetPackage(ctx, e.repo.ID, "gone.txt"); err != nil {
		t.Errorf("without prune the rows stay: %v", err)
	}
	r = e.run(t, reindex.Request{Prune: true})
	if r.Removed != 1 || r.Unchanged != 1 {
		t.Errorf("with prune: %+v", *r)
	}
	if _, err := e.st.GetPackage(ctx, e.repo.ID, "gone.txt"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the package of the deleted manifest: %v, want ErrNotFound", err)
	}
	if _, err := e.st.GetPackage(ctx, e.repo.ID, "pushed.txt"); err != nil {
		t.Errorf("the package pushed past Lajstrom: %v", err)
	}
}

// TestForeignAndReferrers is I-02 and E-04: a Helm chart under a managed path is reported and
// never indexed; a signature with a subject and the tag-schema index are referrers, not
// foreign, and orphans when their subject is not a version.
func TestForeignAndReferrers(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	pub := e.publish(t, "hello", "hello")
	chart := ocispec.Manifest{Versioned: specs2, MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{MediaType: "application/vnd.cncf.helm.config.v1+json", Digest: digest.FromString("c"), Size: 1}}
	chartDesc := e.pushManifest(t, "raw/scratch/charts/demo", "0.1.0", chart, ocispec.MediaTypeImageManifest)
	subject := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: pub.Digest, Size: 1}
	sig := ocispec.Manifest{Versioned: specs2, MediaType: ocispec.MediaTypeImageManifest, ArtifactType: "application/vnd.dev.cosign.artifact.sig.v1+json",
		Config: ocispec.DescriptorEmptyJSON, Subject: &subject}
	e.pushManifest(t, pub.RepoPath, "signature", sig, ocispec.MediaTypeImageManifest)
	unknown := digest.FromString("a manifest that is not ours")
	orphan := ocispec.Index{Versioned: specs2, MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("s1"), Size: 1},
			{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("s2"), Size: 1}}}
	orphanDesc := e.pushManifest(t, pub.RepoPath, "sha256-"+unknown.Encoded(), orphan, ocispec.MediaTypeImageIndex)

	r := e.run(t, reindex.Request{Prune: true})
	same(t, "foreign", []reindex.Finding{{Repository: "charts/demo", Tag: "0.1.0", Digest: chartDesc.Digest,
		MediaType: ocispec.MediaTypeImageManifest}}, r.Foreign)
	same(t, "orphan referrers", []reindex.Finding{{Repository: "hello", Tag: "sha256-" + unknown.Encoded(),
		Digest: orphanDesc.Digest, MediaType: ocispec.MediaTypeImageIndex, Subject: unknown}}, r.OrphanReferrers)
	if r.Referrers != 3 || r.Unchanged != 1 || len(r.Errors) != 0 {
		t.Errorf("report: %+v", *r)
	}
	if _, err := e.st.GetPackage(ctx, e.repo.ID, "charts/demo"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the chart was indexed: %v", err)
	}
	repos, err := e.st.ListOCIRepos(ctx, e.repo.ID)
	must(t, err)
	if !slices.ContainsFunc(repos, func(r index.OCIRepo) bool { return r.Path == "raw/scratch/charts/demo" }) {
		t.Errorf("oci_repos without the chart's repository: %+v", repos)
	}
}

var specs2 = specs.Versioned{SchemaVersion: 2}

// TestDrift checks that a tag re-pointed in the backend keeps the index's digest, is reported,
// and is not swept.
func TestDrift(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	pub := e.publish(t, "tool", "v1")
	other := ocispec.Manifest{Versioned: specs2, MediaType: ocispec.MediaTypeImageManifest, Config: ocispec.DescriptorEmptyJSON}
	moved := e.pushManifest(t, pub.RepoPath, pub.Tag, other, ocispec.MediaTypeImageManifest)

	r := e.run(t, reindex.Request{Prune: true})
	same(t, "drift", []reindex.Drift{{Package: "tool", Tag: pub.Tag, Index: pub.Digest, Registry: moved.Digest}}, r.Drift)
	pkg, err := e.st.GetPackage(ctx, e.repo.ID, "tool")
	must(t, err)
	v, err := e.st.GetVersionByTag(ctx, pkg.ID, pub.Tag)
	must(t, err)
	if v.Digest != pub.Digest || r.Removed != 0 {
		t.Errorf("the index after drift: digest %s, removed %d; want %s and 0", v.Digest, r.Removed, pub.Digest)
	}
}

// TestDegraded checks that a manifest whose metadata layer is missing is indexed from its
// annotations, flagged, and read again by the next run.
func TestDegraded(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	built, err := raw.Plugin{}.Build(ctx, e.repo, rawUpload("notes.txt", "n"))
	must(t, err)
	e.bypass(t, e.repo, "notes.txt", "n", built.Manifest.Layers[1].Digest)
	r := e.run(t, reindex.Request{Prune: true})
	if r.Added != 1 || r.Degraded != 1 {
		t.Errorf("first run: %+v", *r)
	}
	pkg, err := e.st.GetPackage(ctx, e.repo.ID, "notes.txt")
	must(t, err)
	v, err := e.st.GetVersionByTag(ctx, pkg.ID, built.Tag)
	must(t, err)
	if !v.Degraded || v.Metadata != nil {
		t.Errorf("the row: degraded %v, metadata %s", v.Degraded, v.Metadata)
	}
	r = e.run(t, reindex.Request{Prune: true})
	if r.Updated != 1 || r.Degraded != 1 || r.Unchanged != 0 {
		t.Errorf("second run reads it again: %+v", *r)
	}
}

// TestSweepKeepsWhatItCannotCheck checks the re-check of the sweep: a row the listing missed
// stays when its tag still names its digest, and when the registry cannot answer; a run whose
// listing fails sweeps nothing.
func TestSweepKeepsWhatItCannotCheck(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	hidden := e.publish(t, "hidden", "h")
	e.publish(t, "shown", "s")
	e.prov.hide = hidden.RepoPath
	r := e.run(t, reindex.Request{Prune: true})
	if r.Removed != 0 || r.Unchanged != 1 {
		t.Errorf("a row the listing missed, its tag in place: %+v", *r)
	}

	e.prov.failResolve = errors.New("connection reset")
	r = e.run(t, reindex.Request{Prune: true})
	if r.Removed != 0 || len(r.Errors) == 0 || !strings.Contains(r.Errors[0].Error, "connection reset") {
		t.Errorf("a registry that does not answer: %+v", *r)
	}

	e.prov.failResolve, e.prov.failList = nil, errors.New("catalog unavailable")
	must(t, e.fake.DeleteRepository(ctx, "raw/scratch/shown"))
	r = e.run(t, reindex.Request{Prune: true})
	if r.Removed != 0 || len(r.Errors) != 1 {
		t.Errorf("a listing that fails: %+v", *r)
	}
	for _, name := range []string{"hidden", "shown"} {
		if _, err := e.st.GetPackage(ctx, e.repo.ID, name); err != nil {
			t.Errorf("package %s: %v", name, err)
		}
	}
}

// TestUnsupportedFormat checks that a repo whose format has no plugin is reported once and
// swept never.
func TestUnsupportedFormat(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	repo := e.newRepo(t, "pypi", "internal")
	for _, name := range []string{"requests", "urllib3"} {
		m := ocispec.Manifest{Versioned: specs2, MediaType: ocispec.MediaTypeImageManifest, Config: ocispec.DescriptorEmptyJSON,
			Annotations: map[string]string{profile.AnnotationSchema: profile.SchemaVersion, profile.AnnotationName: name}}
		e.pushManifest(t, "pypi/internal/"+name, "1.0", m, ocispec.MediaTypeImageManifest)
	}
	pkg := index.Package{RepoID: repo.ID, Name: "kept", NameOCI: "kept"}
	must(t, e.st.UpsertPackage(ctx, &pkg))
	must(t, e.st.UpsertVersion(ctx, &index.Version{PackageID: pkg.ID, Version: "1", Tag: "1", Digest: digest.FromString("k"), ArtifactType: "a"}))
	r := e.run(t, reindex.Request{Repo: &repo, Prune: true})
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0].Error, "no plugin") || r.Removed != 0 {
		t.Errorf("report: %+v", *r)
	}
	if _, err := e.st.GetPackage(ctx, repo.ID, "kept"); err != nil {
		t.Errorf("a row of the repo: %v", err)
	}
}

// TestRuns checks the run around the scan: the lock refuses a second run, the outcome and the
// report are stored, the start is in the audit log.
func TestRuns(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.publish(t, "a", "1")
	req := reindex.Request{Repo: &e.repo, Prune: true, Source: "api", Principal: "user:admin"}
	run, err := e.w.Start(ctx, req)
	must(t, err)
	var conflict *index.ReindexConflictError
	if _, err := e.w.Start(ctx, reindex.Request{Source: "cli", Principal: "user:root"}); !errors.As(err, &conflict) || conflict.Running.ID != run.ID {
		t.Fatalf("a second start: %v, want the conflict naming run %d", err, run.ID)
	}
	report, err := e.w.Run(ctx, run, req)
	must(t, err)
	stored, err := e.st.LatestReindexRun(ctx, e.repo.ID)
	must(t, err)
	if stored.ID != run.ID || stored.State != index.ReindexDone || stored.FinishedAt == nil || stored.Error != "" {
		t.Errorf("the stored run: %+v", *stored)
	}
	var got reindex.Report
	must(t, json.Unmarshal(stored.Report, &got))
	same(t, "the stored report", *report, got)
	events, err := e.st.ListAudit(ctx, index.AuditFilter{Action: "repo.reindex"})
	must(t, err)
	if len(events) != 1 || events[0].Repo != "raw/scratch" || events[0].Principal != "user:admin" {
		t.Errorf("audit: %+v", events)
	}
	if _, err := e.w.Start(ctx, reindex.Request{Source: "cli", Principal: "user:root"}); err != nil {
		t.Errorf("a start after the run finished: %v", err)
	}
}

// counting is an Inspector that counts its calls, and takes slow longer for each.
type counting struct {
	reindex.Inspector
	calls atomic.Int64
	slow  time.Duration
}

func (c *counting) Inspect(ctx context.Context, repo index.Repo, pkgPath, tag string, m ocispec.Manifest, raw []byte, o ingest.InspectOptions) (*ingest.Rows, error) {
	c.calls.Add(1)
	time.Sleep(c.slow)
	return c.Inspector.Inspect(ctx, repo, pkgPath, tag, m, raw, o)
}

// wrapped is the fake registry with a catalog that can hide a repository or fail, and a
// Resolve and a tag list that can fail.
type wrapped struct {
	*providertest.Fake
	hide        string
	failList    error
	failResolve error
	failTags    error
}

func (w *wrapped) ListRepositories(ctx context.Context, prefix string) ([]string, error) {
	if w.failList != nil {
		return nil, w.failList
	}
	names, err := w.Fake.ListRepositories(ctx, prefix)
	return slices.DeleteFunc(names, func(n string) bool { return n == w.hide }), err
}

func (w *wrapped) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := w.Fake.Repository(ctx, name)
	return &resolving{Repository: rr, w: w}, err
}

type resolving struct {
	provider.Repository
	w *wrapped
}

func (r *resolving) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	if r.w.failResolve != nil {
		return ocispec.Descriptor{}, r.w.failResolve
	}
	return r.Repository.Resolve(ctx, reference)
}

func (r *resolving) Tags(ctx context.Context, last string, fn func(tags []string) error) error {
	if r.w.failTags != nil {
		return r.w.failTags
	}
	return r.Repository.Tags(ctx, last, fn)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func same(t *testing.T, what string, want, got any) {
	t.Helper()
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("%s: (-want +got)\n%s", what, d)
	}
}

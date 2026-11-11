package npm

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

var alice = auth.Principal{Kind: auth.KindUser, Name: "alice"}

var memoryDBs atomic.Int64

// pipeline is a repo npm/internal in an in-memory index, on a fake registry, with the ingest
// pipeline of the npm plugin.
type pipeline struct {
	st   *sqlite.Store
	fake *providertest.Fake
	pipe *ingest.Pipeline
	repo index.Repo
}

func newPipeline(t *testing.T) *pipeline {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:npm-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	p := &pipeline{st: st, fake: &providertest.Fake{RegistryName: "local"},
		repo: index.Repo{Format: Name, Name: "internal", Registry: "local", Prefix: "npm/internal", Kind: "hosted"}}
	must(t, st.CreateRepo(ctx, &p.repo))
	p.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": p.fake}, Index: st, Log: slog.New(slog.DiscardHandler),
		Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }})
	return p
}

// build returns the manifest and blobs of the tarball b.
func (p *pipeline) build(t *testing.T, b []byte) *format.Built {
	t.Helper()
	built, err := Plugin{}.Build(t.Context(), p.repo, upload(b, ""))
	must(t, err)
	return built
}

// push stores a built version as oras push and the facade do: its blobs straight to the
// registry, then the manifest through ingest.Ingest, the hook of PUT /v2/…/manifests/<tag>.
func (p *pipeline) push(t *testing.T, built *format.Built) *ingest.Result {
	t.Helper()
	ctx := t.Context()
	rr, err := p.fake.Repository(ctx, built.RepoPath)
	must(t, err)
	for _, blob := range built.Blobs {
		rc, err := blob.Open(ctx)
		must(t, err)
		must(t, rr.Push(ctx, blob.Descriptor, rc))
		_ = rc.Close()
	}
	raw, _, err := profile.Encode(built.Manifest)
	must(t, err)
	pkgPath := strings.TrimPrefix(built.RepoPath, "npm/internal/")
	res, err := p.pipe.Ingest(ctx, p.repo, pkgPath, built.Tag, built.Manifest, raw, alice)
	must(t, err)
	return res
}

// rows are the derived rows of the repo by package name and version, without ids and times of
// indexing.
func (p *pipeline) rows(t *testing.T) map[string]any {
	t.Helper()
	ctx := t.Context()
	out := map[string]any{}
	pkgs, err := p.st.ListPackages(ctx, p.repo.ID, "")
	must(t, err)
	for _, pkg := range pkgs {
		versions, err := p.st.ListVersions(ctx, pkg.ID)
		must(t, err)
		for _, v := range versions {
			files, err := p.st.ListFiles(ctx, v.ID)
			must(t, err)
			out[pkg.Name+"@"+v.Version] = []any{pkg, v, files}
		}
	}
	return out
}

// checkScoped checks the rows of @acme/widgets 1.2.0, which res stored: the package path, the
// metadata with the dependencies, the file with its integrity.
func (p *pipeline) checkScoped(t *testing.T, res *ingest.Result, info testutil.NpmInfo) {
	t.Helper()
	ctx := t.Context()
	pkg, err := p.st.GetPackage(ctx, p.repo.ID, "@acme/widgets")
	must(t, err)
	v, err := p.st.GetVersion(ctx, pkg.ID, "1.2.0", "")
	must(t, err)
	files, err := p.st.ListFiles(ctx, v.ID)
	must(t, err)
	if pkg.NameOCI != "acme/widgets" || v.Digest != res.Digest || v.Degraded || !bytes.Contains(v.Metadata, []byte(`"dependencies":{"widgets":"^1.0.0"}`)) ||
		len(files) != 1 || files[0].Name != "widgets-1.2.0.tgz" || files[0].Attrs[profile.AnnotationNpmIntegrity] != info.Integrity {
		t.Errorf("rows of @acme/widgets: %+v, %+v (%s), %+v", pkg, v, v.Metadata, files)
	}
}

// reindex runs a reindex of the repo with prune and returns its report.
func (p *pipeline) reindex(t *testing.T) reindex.RepoReport {
	t.Helper()
	return p.reindexAt(t, time.Time{})
}

// reindexAt is reindex on a clock at at; the zero time is the real clock.
func (p *pipeline) reindexAt(t *testing.T, at time.Time) reindex.RepoReport {
	t.Helper()
	o := reindex.Options{Index: p.st, Providers: provider.Map{"local": p.fake}, Inspector: p.pipe, Log: slog.New(slog.DiscardHandler)}
	if !at.IsZero() {
		o.Now = func() time.Time { return at }
	}
	w := reindex.New(o)
	req := reindex.Request{Repo: &p.repo, Prune: true, Source: "cli", Principal: "user:admin"}
	run, err := w.Start(t.Context(), req)
	must(t, err)
	report, err := w.Run(t.Context(), run, req)
	must(t, err)
	return report.Repos[0]
}

// TestPushReindex is the acceptance criterion of #304: versions pushed as oras push does, an
// unscoped one and a scoped one, and one uploaded through the frontend path, are indexed with
// their metadata and the layer annotations; the same push again is the idempotent repeat; with
// the derived rows dropped, reindex restores the same rows.
func TestPushReindex(t *testing.T) {
	ctx := t.Context()
	p := newPipeline(t)
	widgets, _ := testutil.NpmTarball(testutil.NpmOptions{})
	scoped, info := testutil.NpmTarball(testutil.NpmOptions{Name: "@acme/widgets", Version: "1.2.0", Dependencies: map[string]string{"widgets": "^1.0.0"}})
	p.push(t, p.build(t, widgets))
	built := p.build(t, scoped)
	res := p.push(t, built)
	if res.RepoPath != "npm/internal/acme/widgets" || res.Tag != "1.2.0" || res.Existed {
		t.Errorf("push of @acme/widgets: %+v", res)
	}
	if again := p.push(t, built); !again.Existed || again.Digest != res.Digest {
		t.Errorf("the same push again: %+v", again)
	}
	next, _ := testutil.NpmTarball(testutil.NpmOptions{Version: "1.1.0"})
	if _, err := p.pipe.Upload(ctx, p.repo, upload(next, ""), alice); err != nil {
		t.Fatal(err)
	}

	p.checkScoped(t, res, info)

	before := p.rows(t)
	if len(before) != 3 {
		t.Fatalf("%d versions, want 3", len(before))
	}
	must(t, p.st.ResetDerived(ctx, p.repo.ID))
	if r := p.reindex(t); r.Added != 3 || len(r.Errors) != 0 || len(r.Foreign) != 0 || r.Degraded != 0 {
		t.Errorf("reindex: %+v", r)
	}
	ignore := cmp.Options{cmpopts.IgnoreFields(index.Package{}, "ID"), cmpopts.IgnoreFields(index.Version{}, "ID", "PackageID", "IndexedAt"),
		cmpopts.IgnoreFields(index.File{}, "ID", "VersionID")}
	if d := cmp.Diff(before, p.rows(t), ignore); d != "" {
		t.Errorf("the rows after reindex (-before +after)\n%s", d)
	}
}

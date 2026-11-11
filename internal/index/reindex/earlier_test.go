package reindex_test

import (
	"bytes"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// bypassAt pushes the raw manifest of content at path straight to the OCI repository name, as
// an earlier Lajstrom wrote it.
func (e *env) bypassAt(t *testing.T, name, path, content string) {
	t.Helper()
	ctx := t.Context()
	built, err := raw.Plugin{}.Build(ctx, e.repo, rawUpload(path, content))
	must(t, err)
	rr, err := e.fake.Repository(ctx, name)
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

// earlierPaths are raw paths of every kind whose OCI name ADR 0017 changes.
var earlierPaths = []string{".gitignore", "site/_config.yml", "netlify/blobs", "x/blobs/sha256/0f", "a--b.txt", "cache/my.sync", "x/index.json"}

// TestRebuildEarlierNames: raw files that an earlier Lajstrom wrote at the OCI names of before
// ADR 0017, one of each kind that it changes, pushed straight to the registry. A run finds each
// as its package at that name; a new version through ingest lands in the same OCI repository;
// and a run from an empty index finds both versions in one package, every row as ingest wrote
// it (written in the new form, read in both).
func TestRebuildEarlierNames(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	earlier := map[string]string{}
	for _, p := range earlierPaths {
		pkgPath, err := raw.Plugin{}.EscapeName(p)
		must(t, err)
		if earlier[p] = profile.LegacyPackagePath(pkgPath); earlier[p] == pkgPath {
			t.Fatalf("%s: the package path %s has no earlier form", p, pkgPath)
		}
		e.bypassAt(t, "raw/scratch/"+earlier[p], p, "one "+p)
	}
	if r := e.run(t, reindex.Request{}); r.Added != len(earlierPaths) || len(r.Errors) != 0 || len(r.Foreign) != 0 {
		t.Fatalf("the run over the earlier names: %+v", *r)
	}
	for _, p := range earlierPaths {
		pkg, err := e.st.GetPackage(ctx, e.repo.ID, p)
		must(t, err)
		if pkg.NameOCI != earlier[p] {
			t.Errorf("%s: NameOCI %s, want %s", p, pkg.NameOCI, earlier[p])
		}
		if pub := e.publish(t, p, "two "+p); pub.RepoPath != "raw/scratch/"+earlier[p] {
			t.Errorf("%s: a new version went to %s, want raw/scratch/%s", p, pub.RepoPath, earlier[p])
		}
	}
	before := e.derived(t)
	must(t, e.st.ResetDerived(ctx, e.repo.ID))
	if r := e.run(t, reindex.Request{}); r.Added != 2*len(earlierPaths) || len(r.Errors) != 0 {
		t.Fatalf("the run from an empty index: %+v", *r)
	}
	after := e.derived(t)
	same(t, "the rows rebuilt", before, after)
	versions := map[string]int{}
	for key := range after.Versions {
		versions[key[:strings.LastIndex(key, ":")]]++
	}
	for _, p := range earlierPaths {
		if pkg := after.Packages[p]; pkg.NameOCI != earlier[p] || versions[p] != 2 {
			t.Errorf("%s after the rebuild: NameOCI %s, %d versions; want %s and 2", p, pkg.NameOCI, versions[p], earlier[p])
		}
	}
}

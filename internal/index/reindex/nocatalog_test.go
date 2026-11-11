package reindex_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestNoCatalog is #37 on a registry without a catalog (ListRepositories unsupported): a run
// reads the OCI repositories that oci_repos records and checks each by its tag list. One whose
// tag list fails keeps its row and its versions; one the registry no longer knows keeps its
// versions, which only MANIFEST_UNKNOWN removes (#446), and its row for the grace age; one
// pushed past Lajstrom is not read.
func TestNoCatalog(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.prov.failList = fmt.Errorf("no catalog: %w", provider.ErrUnsupported)
	gone := e.publish(t, "docs/gone.txt", "gone")
	e.publish(t, "docs/kept.txt", "kept")
	e.bypass(t, e.repo, "pushed.txt", "by oras")
	both := []string{"raw/scratch/docs/gone.txt", "raw/scratch/docs/kept.txt"}

	r := e.run(t, reindex.Request{Prune: true})
	if r.OCIRepositories != 2 || r.Unchanged != 2 || r.Added != 0 || len(r.Errors) != 0 {
		t.Errorf("a run over oci_repos: %+v", *r)
	}

	e.prov.failTags = errors.New("connection reset")
	r = e.run(t, reindex.Request{Prune: true})
	if r.Removed != 0 || len(r.Errors) != 2 || !strings.Contains(r.Errors[0].Error, "connection reset") {
		t.Errorf("tag lists that fail: %+v", *r)
	}
	same(t, "oci_repos after tag lists that fail", both, e.derived(t).OCIRepos)

	e.prov.failTags = nil
	must(t, e.fake.DeleteRepository(ctx, gone.RepoPath))
	r = e.run(t, reindex.Request{Prune: true})
	if r.Removed != 0 || r.Unchanged != 1 || len(r.Errors) != 1 || !strings.Contains(r.Errors[0].Error, "NAME_UNKNOWN") {
		t.Errorf("a repository the registry does not know: %+v", *r)
	}
	same(t, "oci_repos after it", both, e.derived(t).OCIRepos)
	e.clock.later(2 * time.Hour)
	r = e.run(t, reindex.Request{Prune: true})
	if r.Removed != 0 || r.Unchanged != 1 || len(r.Errors) != 1 {
		t.Errorf("a repository the registry does not know, two hours later: %+v", *r)
	}
	same(t, "oci_repos past the grace age", both[1:], e.derived(t).OCIRepos)
	if _, err := e.st.GetPackage(ctx, e.repo.ID, "docs/gone.txt"); err != nil {
		t.Errorf("the package of a repository the registry does not know: %v", err)
	}
}

// TestNoCatalogRecordsAgain: on a registry without a catalog, the sweep that finds a version
// by its tag records its OCI repository again, which a tag list that failed for longer than the
// grace age, or a lagging one, let go (#446).
func TestNoCatalogRecordsAgain(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.prov.failList = fmt.Errorf("no catalog: %w", provider.ErrUnsupported)
	e.publish(t, "docs/kept.txt", "kept")
	_, err := e.st.PruneOCIRepos(ctx, e.repo.ID, time.Now().Add(time.Hour))
	must(t, err)
	same(t, "oci_repos let go", []string{}, e.derived(t).OCIRepos)
	r := e.run(t, reindex.Request{Prune: true})
	if r.OCIRepositories != 0 || r.Removed != 0 || len(r.Errors) != 0 {
		t.Errorf("a run without the row: %+v", *r)
	}
	same(t, "oci_repos after the sweep", []string{"raw/scratch/docs/kept.txt"}, e.derived(t).OCIRepos)
	if r = e.run(t, reindex.Request{Prune: true}); r.OCIRepositories != 1 || r.Unchanged != 1 {
		t.Errorf("the next run: %+v", *r)
	}
}

// TestAttach is the first attach of a prefix on a registry without a catalog (#37): once the
// derived rows are lost a run finds nothing; a run with the repositories to attach brings back
// what ingest wrote and what was pushed past Lajstrom, skips a repository the registry does not
// know, and records the others, so that the next run finds them without the list.
func TestAttach(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.prov.failList = fmt.Errorf("no catalog: %w", provider.ErrUnsupported)
	e.publish(t, "docs/readme.md", "hello")
	e.publish(t, "bin/tool", "#!/bin/sh")
	e.bypass(t, e.repo, "pushed.txt", "by oras")
	must(t, e.st.ResetDerived(ctx, e.repo.ID))

	r := e.run(t, reindex.Request{Prune: true})
	if r.OCIRepositories != 0 || r.Manifests != 0 {
		t.Errorf("a run without the list: %+v", *r)
	}
	attach := []string{"raw/scratch/docs/readme.md", "raw/scratch/bin/tool", "raw/scratch/pushed.txt",
		"raw/scratch/docs/readme.md", "raw/scratch/nothing"}
	r = e.run(t, reindex.Request{Prune: true, Repositories: attach})
	if r.OCIRepositories != 4 || r.Added != 3 || len(r.Errors) != 0 {
		t.Errorf("a run with the list: %+v", *r)
	}
	same(t, "oci_repos after the attach", []string{"raw/scratch/bin/tool", "raw/scratch/docs/readme.md", "raw/scratch/pushed.txt"},
		e.derived(t).OCIRepos)
	r = e.run(t, reindex.Request{Prune: true})
	if r.OCIRepositories != 3 || r.Unchanged != 3 || len(r.Errors) != 0 {
		t.Errorf("the next run: %+v", *r)
	}
}

// TestRebuildTagSchema checks that a rebuild does not report the referrers of the versions it
// writes as orphans: the sha256-<hex> index of a registry without the referrers API is read
// while the subject's row is being written, here slowly.
func TestRebuildTagSchema(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	pub := e.publish(t, "hello", "hello")
	sig := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("signature"), Size: 1,
		ArtifactType: "application/vnd.dev.cosign.artifact.sig.v1+json"}
	e.pushManifest(t, pub.RepoPath, "sha256-"+pub.Digest.Encoded(),
		ocispec.Index{Versioned: specs2, MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{sig}},
		ocispec.MediaTypeImageIndex)
	must(t, e.st.ResetDerived(ctx, e.repo.ID))
	e.inspect.slow = 50 * time.Millisecond
	r := e.run(t, reindex.Request{Prune: true})
	if r.Added != 1 || r.Referrers != 1 || len(r.OrphanReferrers) != 0 || len(r.Errors) != 0 {
		t.Errorf("report: %+v", *r)
	}
}

// TestAttachRefused checks that Start refuses repositories to attach without a repo and outside
// the repo's path, before it takes the lock.
func TestAttachRefused(t *testing.T) {
	e := newEnv(t)
	other := e.newRepo(t, "raw", "other")
	for _, c := range []struct {
		name string
		req  reindex.Request
		want string
	}{
		{"without a repo", reindex.Request{Repositories: []string{"raw/scratch/a"}}, "need the repo they belong to"},
		{"of another repo", reindex.Request{Repo: &other, Repositories: []string{"raw/scratch/a"}},
			`"raw/scratch/a" is not an OCI repository of raw/other: its name must start with raw/other/`},
		{"a longer path", reindex.Request{Repo: &e.repo, Repositories: []string{"raw/scratchy/a"}}, "is not an OCI repository"},
		{"the path itself", reindex.Request{Repo: &e.repo, Repositories: []string{"raw/scratch/"}}, "is not an OCI repository"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.req.Source, c.req.Principal = "cli", "user:admin"
			if _, err := e.w.Start(t.Context(), c.req); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Start: %v, want %q", err, c.want)
			}
		})
	}
	if _, err := e.w.Start(t.Context(), reindex.Request{Source: "cli", Principal: "user:admin"}); err != nil {
		t.Errorf("a run of every repo after the refusals: %v", err)
	}
}

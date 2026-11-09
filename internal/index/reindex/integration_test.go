//go:build integration

package reindex_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegration runs reindex against the registry at LAJSTROM_TEST_REGISTRY (host:port, plain
// HTTP; LAJSTROM_TEST_REGISTRY_PREFIX, _USERNAME and _PASSWORD when set), the acceptance criteria
// of #29 in order on one repo with a random name:
//
//   - I-01: three raw files published through ingest, the derived rows dropped, a run: every
//     column back but the ids and indexed_at;
//   - drift: a profile manifest pushed past Lajstrom is indexed, a manifest deleted in the
//     backend loses its rows (when the registry deletes manifests), a tag re-pointed in the
//     backend is reported and keeps the index's digest (when tags are mutable);
//   - I-02 and E-04: a Helm chart under the repo's path is foreign, in oci_repos and the report,
//     never indexed; a signature with a subject is a referrer, not in the report.
func TestIntegration(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	prov, err := generic.New(config.Registry{
		Name: "local", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")},
	}, slog.New(slog.DiscardHandler))
	must(t, err)
	caps, err := prov.Capabilities(ctx)
	must(t, err)

	e := newEnv(t)
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	e.repo = e.newRepo(t, "raw", "it-"+hex.EncodeToString(suffix))
	e.repo.Prefix = ""
	must(t, e.st.UpdateRepo(ctx, &e.repo))
	plugins := func(name string) (format.Plugin, bool) { return raw.Plugin{}, name == raw.Name }
	log := slog.New(slog.DiscardHandler)
	e.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": prov}, Index: e.st, Plugins: plugins, Log: log})
	e.w = reindex.New(reindex.Options{Index: e.st, Providers: provider.Map{"local": prov}, Inspector: e.pipe, Log: log})
	base := "raw/" + e.repo.Name

	// I-01.
	e.publish(t, "docs/readme.md", "hello "+e.repo.Name)
	gone := e.publish(t, "docs/old.md", "old "+e.repo.Name)
	moved := e.publish(t, "bin/tool", "tool "+e.repo.Name)
	before := e.derived(t)
	must(t, e.st.ResetDerived(ctx, e.repo.ID))
	r := e.run(t, reindex.Request{Prune: true})
	if r.Added != 3 || r.Manifests != 3 || len(r.Errors) != 0 || len(r.Foreign) != 0 {
		t.Fatalf("I-01 run: %+v", *r)
	}
	same(t, "I-01: the rebuilt rows", before, e.derived(t))

	// The registry changes behind Lajstrom's back.
	pushBypass(ctx, t, prov, e.repo, "pushed.txt", "by oras "+e.repo.Name)
	chart := pushChart(ctx, t, prov, base+"/charts/demo", "0.1.0")
	subject := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: gone.Digest, Size: manifestSize(ctx, t, prov, gone)}
	pushJSON(ctx, t, prov, base+"/docs/readme.md", "signature", ocispec.MediaTypeImageManifest, ocispec.Manifest{
		Versioned: specs2, MediaType: ocispec.MediaTypeImageManifest, ArtifactType: "application/vnd.dev.cosign.artifact.sig.v1+json",
		Config: pushEmpty(ctx, t, prov, base+"/docs/readme.md"), Subject: &subject,
		Layers: []ocispec.Descriptor{pushBlob(ctx, t, prov, base+"/docs/readme.md", "application/vnd.dev.cosign.simplesigning.v1+json", []byte(`{"critical":{}}`))},
	})
	wantRemoved := 0
	if caps.DeleteManifest {
		rr, err := prov.Repository(ctx, gone.RepoPath)
		must(t, err)
		desc, err := rr.Resolve(ctx, gone.Tag)
		must(t, err)
		must(t, rr.Delete(ctx, desc))
		wantRemoved = 1
	}
	var drifted digest.Digest
	if !caps.ImmutableTags {
		drifted = pushJSON(ctx, t, prov, moved.RepoPath, moved.Tag, ocispec.MediaTypeImageManifest, ocispec.Manifest{
			Versioned: specs2, MediaType: ocispec.MediaTypeImageManifest, Config: pushEmpty(ctx, t, prov, moved.RepoPath),
			Layers: []ocispec.Descriptor{pushBlob(ctx, t, prov, moved.RepoPath, "application/octet-stream", []byte("other "+e.repo.Name))},
		}).Digest
	}

	r = e.run(t, reindex.Request{Prune: true})
	if r.Added != 1 || r.Removed != wantRemoved || r.Referrers != 1 || len(r.OrphanReferrers) != 0 || len(r.Errors) != 0 {
		t.Errorf("drift run: %+v", *r)
	}
	same(t, "I-02: the foreign manifest", []reindex.Finding{{Repository: "charts/demo", Tag: "0.1.0", Digest: chart,
		MediaType: ocispec.MediaTypeImageManifest}}, r.Foreign)
	if drifted != "" {
		same(t, "the re-pointed tag", []reindex.Drift{{Package: "bin/tool", Tag: moved.Tag, Index: moved.Digest, Registry: drifted}}, r.Drift)
	}
	if _, err := e.st.GetPackage(ctx, e.repo.ID, "pushed.txt"); err != nil {
		t.Errorf("the manifest pushed past Lajstrom: %v", err)
	}
	if _, err := e.st.GetPackage(ctx, e.repo.ID, "charts/demo"); err == nil {
		t.Error("the Helm chart was indexed")
	}
	repos, err := e.st.ListOCIRepos(ctx, e.repo.ID)
	must(t, err)
	found := false
	for _, r := range repos {
		found = found || r.Path == base+"/charts/demo"
	}
	if !found {
		t.Errorf("oci_repos without the chart: %+v", repos)
	}
	_, err = e.st.GetPackage(ctx, e.repo.ID, "docs/old.md")
	if caps.DeleteManifest && err == nil {
		t.Error("the rows of the deleted manifest are still there")
	}
}

// pushBypass pushes the raw manifest of content at path straight to the registry.
func pushBypass(ctx context.Context, t *testing.T, prov provider.Provider, repo index.Repo, path, content string) {
	t.Helper()
	built, err := raw.Plugin{}.Build(ctx, repo, rawUpload(path, content))
	must(t, err)
	rr, err := prov.Repository(ctx, built.RepoPath)
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

// pushChart pushes a manifest shaped like a Helm chart and returns its digest.
func pushChart(ctx context.Context, t *testing.T, prov provider.Provider, name, tag string) digest.Digest {
	t.Helper()
	cfg := pushBlob(ctx, t, prov, name, "application/vnd.cncf.helm.config.v1+json", []byte(`{"name":"demo","version":"0.1.0"}`))
	layer := pushBlob(ctx, t, prov, name, "application/vnd.cncf.helm.chart.content.v1.tar+gzip", []byte("not really a tarball"))
	return pushJSON(ctx, t, prov, name, tag, ocispec.MediaTypeImageManifest, ocispec.Manifest{
		Versioned: specs2, MediaType: ocispec.MediaTypeImageManifest, Config: cfg, Layers: []ocispec.Descriptor{layer}}).Digest
}

func pushBlob(ctx context.Context, t *testing.T, prov provider.Provider, name, mediaType string, b []byte) ocispec.Descriptor {
	t.Helper()
	rr, err := prov.Repository(ctx, name)
	must(t, err)
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(b), Size: int64(len(b))}
	if ok, err := rr.Exists(ctx, desc); err != nil || !ok {
		must(t, rr.Push(ctx, desc, bytes.NewReader(b)))
	}
	return desc
}

func pushEmpty(ctx context.Context, t *testing.T, prov provider.Provider, name string) ocispec.Descriptor {
	t.Helper()
	return pushBlob(ctx, t, prov, name, ocispec.MediaTypeEmptyJSON, ocispec.DescriptorEmptyJSON.Data)
}

func pushJSON(ctx context.Context, t *testing.T, prov provider.Provider, name, tag, mediaType string, v any) ocispec.Descriptor {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	rr, err := prov.Repository(ctx, name)
	must(t, err)
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(b), Size: int64(len(b))}
	must(t, rr.PushReference(ctx, desc, bytes.NewReader(b), tag))
	return desc
}

func manifestSize(ctx context.Context, t *testing.T, prov provider.Provider, pub *format.Published) int64 {
	t.Helper()
	rr, err := prov.Repository(ctx, pub.RepoPath)
	must(t, err)
	desc, err := rr.Resolve(ctx, pub.Tag)
	must(t, err)
	return desc.Size
}

// TestScale is the last criterion of #29 and N-06: LAJSTROM_TEST_REINDEX_MANIFESTS raw manifests
// (10000 for the criterion) in packages of 100 versions, pushed straight to the registry, then
// a run that rebuilds them from an empty index, and a second run over known digests. It logs
// both rates for docs/operations.md and, from 10 000 manifests on, fails a rebuild slower than
// 12 ms a manifest (10 000 in two minutes). The manifests are deleted at the end where the
// registry deletes manifests.
func TestScale(t *testing.T) {
	addr, n := os.Getenv("LAJSTROM_TEST_REGISTRY"), 0
	if s := os.Getenv("LAJSTROM_TEST_REINDEX_MANIFESTS"); s != "" {
		var err error
		if n, err = strconv.Atoi(s); err != nil || n < 1 {
			t.Fatalf("LAJSTROM_TEST_REINDEX_MANIFESTS=%q is not a positive number", s)
		}
	}
	if addr == "" || n == 0 {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port and LAJSTROM_TEST_REINDEX_MANIFESTS=10000")
	}
	ctx := t.Context()
	prov, err := generic.New(config.Registry{Name: "local", Type: generic.Type, URL: "http://" + addr,
		Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX")}, slog.New(slog.DiscardHandler))
	must(t, err)
	caps, err := prov.Capabilities(ctx)
	must(t, err)
	e := newEnv(t)
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	e.repo = e.newRepo(t, "raw", "scale-"+hex.EncodeToString(suffix))
	e.repo.Prefix = ""
	must(t, e.st.UpdateRepo(ctx, &e.repo))
	plugins := func(name string) (format.Plugin, bool) { return raw.Plugin{}, name == raw.Name }
	e.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": prov}, Index: e.st, Plugins: plugins,
		Log: slog.New(slog.DiscardHandler)})
	e.w = reindex.New(reindex.Options{Index: e.st, Providers: provider.Map{"local": prov}, Inspector: e.pipe,
		Log: slog.New(slog.DiscardHandler)})

	began := time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 32)
	failed := atomic.Int64{}
	for i := range n {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			path := fmt.Sprintf("pkg-%03d/file.bin", i/100)
			if err := pushRaw(ctx, prov, e.repo, path, fmt.Sprintf("%s %d", e.repo.Name, i)); err != nil {
				if failed.Add(1) == 1 {
					t.Errorf("push %d: %v", i, err)
				}
			}
		})
	}
	wg.Wait()
	if failed.Load() > 0 {
		t.Fatalf("%d pushes failed", failed.Load())
	}
	t.Logf("pushed %d manifests in %s", n, time.Since(began).Round(time.Millisecond))
	if caps.DeleteManifest {
		t.Cleanup(func() { deleteAll(context.Background(), t, prov, "raw/"+e.repo.Name+"/") })
	}

	began = time.Now()
	r := e.run(t, reindex.Request{Prune: true})
	rebuild := time.Since(began)
	if r.Added != n || len(r.Errors) != 0 {
		t.Fatalf("rebuild: %d added of %d, %d errors (%+v)", r.Added, n, len(r.Errors), r.Errors[:min(3, len(r.Errors))])
	}
	began = time.Now()
	r = e.run(t, reindex.Request{Prune: true})
	again := time.Since(began)
	if r.Unchanged != n {
		t.Errorf("second run: %d unchanged of %d", r.Unchanged, n)
	}
	t.Logf("rebuild of %d manifests: %s (%.0f/s); a run over known digests: %s (%.0f/s)", n,
		rebuild.Round(time.Millisecond), float64(n)/rebuild.Seconds(), again.Round(time.Millisecond), float64(n)/again.Seconds())
	if n >= 10000 && rebuild > time.Duration(n)*12*time.Millisecond {
		t.Errorf("rebuild of %d manifests took %s, more than 12 ms a manifest", n, rebuild)
	}
}

// pushRaw pushes the raw manifest of content at path straight to the registry.
func pushRaw(ctx context.Context, prov provider.Provider, repo index.Repo, path, content string) error {
	built, err := raw.Plugin{}.Build(ctx, repo, rawUpload(path, content))
	if err != nil {
		return err
	}
	rr, err := prov.Repository(ctx, built.RepoPath)
	if err != nil {
		return err
	}
	for _, b := range built.Blobs {
		if ok, err := rr.Exists(ctx, b.Descriptor); err == nil && ok {
			continue // the empty config, pushed by another version of the package
		}
		rc, err := b.Open(ctx)
		if err != nil {
			return err
		}
		err = rr.Push(ctx, b.Descriptor, rc)
		_ = rc.Close()
		if err != nil {
			if ok, xerr := rr.Exists(ctx, b.Descriptor); xerr != nil || !ok { // not a concurrent push of the same blob
				return err
			}
		}
	}
	b, d, err := profile.Encode(built.Manifest)
	if err != nil {
		return err
	}
	return rr.PushReference(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: int64(len(b))},
		bytes.NewReader(b), built.Tag)
}

// deleteAll deletes every manifest under prefix, best effort.
func deleteAll(ctx context.Context, t *testing.T, prov provider.Provider, prefix string) {
	names, err := prov.ListRepositories(ctx, prefix)
	if err != nil {
		t.Logf("cleanup: %v", err)
		return
	}
	for _, name := range names {
		rr, err := prov.Repository(ctx, name)
		if err != nil {
			continue
		}
		_ = rr.Tags(ctx, "", func(tags []string) error {
			for _, tag := range tags {
				if desc, err := rr.Resolve(ctx, tag); err == nil {
					_ = rr.Delete(ctx, desc)
				}
			}
			return nil
		})
	}
}

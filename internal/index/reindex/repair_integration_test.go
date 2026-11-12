//go:build integration

package reindex_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegrationRepairReferrers is TestRepairReferrers against the registry at
// LAJSTROM_TEST_REGISTRY when it has no referrers API (distribution): the generic provider writes
// the subject's tag-schema index for three signatures pushed through ingest. With the index then
// deleted by hand, nothing lists them, and a run of the reconcile loop pushes it again: the
// provider lists the three, and the next run pushes nothing. A registry with the API lists
// referrers itself, and the test is skipped there.
func TestIntegrationRepairReferrers(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	prov, err := generic.New(config.Registry{
		Name: "local", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")},
	}, slog.New(slog.DiscardHandler))
	must(t, err)
	if caps, err := prov.Capabilities(ctx); err != nil || caps.Referrers {
		t.Skipf("the registry has the referrers API (%v): there is no tag-schema index to repair", err)
	}
	e := newEnv(t)
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	e.repo = e.newRepo(t, "raw", "it-"+hex.EncodeToString(suffix))
	e.repo.Prefix = ""
	must(t, e.st.UpdateRepo(ctx, &e.repo))
	plugins := func(name string) (format.Plugin, bool) { return raw.Plugin{}, name == raw.Name }
	log := slog.New(slog.DiscardHandler)
	e.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": prov}, Index: e.st, Plugins: plugins, Log: log})
	e.w = reindex.New(reindex.Options{Index: e.st, Providers: provider.Map{"local": prov}, Inspector: e.pipe, Repairer: e.pipe, Log: log})
	t.Cleanup(func() { deleteAll(context.Background(), t, prov, "raw/"+e.repo.Name+"/") })

	pub := e.publish(t, "notes", "the subject of "+e.repo.Name)
	subject := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: pub.Digest, Size: manifestSize(ctx, t, prov, pub)}
	var refs []digest.Digest
	for i := range 3 {
		body, err := json.Marshal(ocispec.Manifest{Versioned: specs2, MediaType: ocispec.MediaTypeImageManifest,
			ArtifactType: "application/vnd.dev.cosign.artifact.sig.v1+json", Config: pushEmpty(ctx, t, prov, pub.RepoPath), Subject: &subject,
			Layers: []ocispec.Descriptor{pushBlob(ctx, t, prov, pub.RepoPath, "application/vnd.dev.cosign.simplesigning.v1+json",
				fmt.Appendf(nil, `{"critical":{},"n":%d}`, i))}})
		must(t, err)
		desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(body), Size: int64(len(body))}
		_, err = e.pipe.PutReferrer(ctx, e.repo, ingest.Referrer{Path: "notes", Desc: desc, Manifest: body, Subject: pub.Digest}, alice)
		must(t, err)
		refs = append(refs, desc.Digest)
	}
	rr, err := prov.Repository(ctx, pub.RepoPath)
	must(t, err)
	listed := func() []digest.Digest {
		var out []digest.Digest
		err := rr.Referrers(ctx, subject, "", func(page []ocispec.Descriptor) error {
			for _, d := range page {
				out = append(out, d.Digest)
			}
			return nil
		})
		if err != nil && !errors.Is(err, provider.ErrNotFound) {
			t.Fatal(err)
		}
		return out
	}
	same(t, "the referrers the provider lists", refs, listed())
	idx, err := rr.Resolve(ctx, "sha256-"+pub.Digest.Encoded())
	must(t, err)
	must(t, rr.Delete(ctx, idx))
	same(t, "the referrers listed with the index lost", []digest.Digest(nil), listed())

	req := reindex.Request{Repo: &e.repo, Prune: true, Source: reindex.SourceReconcile, Principal: "anonymous"}
	r := e.runAs(t, req)
	if len(r.Errors) != 0 || len(r.RepairedReferrers) != 1 || r.RepairedReferrers[0].Repository != "notes" {
		t.Fatalf("the reconcile run: errors %+v, repaired %+v", r.Errors, r.RepairedReferrers)
	}
	same(t, "the referrers it added", refs, r.RepairedReferrers[0].Added)
	same(t, "the referrers listed after the run", refs, listed())
	if again := e.runAs(t, req); len(again.RepairedReferrers) != 0 || len(again.Errors) != 0 {
		t.Errorf("the next run: repaired %+v, errors %+v", again.RepairedReferrers, again.Errors)
	}
}

//go:build integration

package ingest_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// alwaysTagAtOnce is how many referrers of one subject TestIntegrationReferrersAlwaysTag pushes
// at once.
const alwaysTagAtOnce = 12

// TestIntegrationReferrersAlwaysTag (#326) runs PutReferrer and Delete of an oci repo against the
// registry at LAJSTROM_TEST_REGISTRY with referrers.alwaysTag: referrers of one subject pushed at
// once by digest are all listed by the referrers API and by the subject's sha256-<hex> index,
// which the pushes rewrite under the referrers lock, and the provider lists each once; a delete
// removes one from both. On a registry without the API the index alone lists them, as without the
// option. The index is SQLite in memory, whose referrers lock is the process's.
func TestIntegrationReferrersAlwaysTag(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg := config.Registry{Name: "local", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")}}
	plain, err := generic.New(cfg, slog.New(slog.DiscardHandler))
	must(t, err)
	cfg.Referrers.AlwaysTag = true
	prov, err := generic.New(cfg, slog.New(slog.DiscardHandler))
	must(t, err)
	caps, err := prov.Capabilities(ctx)
	must(t, err)
	if _, err := plain.Capabilities(ctx); err != nil {
		t.Fatal(err)
	}

	e := newEnv(t)
	run := make([]byte, 4)
	_, _ = rand.Read(run)
	name := "it-" + hex.EncodeToString(run)
	repo := index.Repo{Format: "oci", Name: name, Registry: "local", Prefix: "oci/" + name, Kind: "hosted"}
	must(t, e.st.CreateRepo(ctx, &repo))
	e.pipe = ingest.New(e.options(prov))
	oci := "oci/" + name + "/app"
	rr, err := prov.Repository(ctx, oci)
	must(t, err)

	push := func(b []byte, mediaType string) ocispec.Descriptor {
		d := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(b), Size: int64(len(b))}
		must(t, rr.Push(ctx, d, bytes.NewReader(b)))
		return d
	}
	push(ocispec.DescriptorEmptyJSON.Data, ocispec.MediaTypeEmptyJSON)
	layer := push([]byte("the subject "+name), "application/octet-stream")
	body, err := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		ArtifactType: "application/vnd.example.it", Config: ocispec.DescriptorEmptyJSON, Layers: []ocispec.Descriptor{layer}})
	must(t, err)
	subject := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(body), Size: int64(len(body))}
	must(t, rr.PushReference(ctx, subject, bytes.NewReader(body), "v1"))

	refs := make([]ingest.Referrer, alwaysTagAtOnce)
	want := make([]digest.Digest, alwaysTagAtOnce)
	for i := range refs {
		l := fmt.Sprintf("signature %d of %s", i, name)
		push([]byte(l), bundleType)
		refs[i] = signature(t, subject, l)
		refs[i].Path = "app"
		want[i] = refs[i].Desc.Digest
	}
	var wg sync.WaitGroup
	for i := range refs {
		wg.Go(func() {
			if _, err := e.pipe.PutReferrer(ctx, repo, refs[i], alice); err != nil {
				t.Errorf("referrer %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	check := func(when string, want []digest.Digest) {
		t.Helper()
		want = slices.Sorted(slices.Values(want))
		same(t, when+": listed by the provider with alwaysTag", want, listed(ctx, t, prov, oci, subject))
		same(t, when+": listed by the sha256-<hex> index", want, indexed(ctx, t, rr, subject))
		if caps.Referrers {
			same(t, when+": listed by the referrers API", want, listed(ctx, t, plain, oci, subject))
		}
	}
	check("after the pushes", want)

	_, err = e.pipe.Delete(ctx, repo, format.Deletion{Path: "app", Reference: want[0].String()}, alice)
	must(t, err)
	check("after the delete of one", want[1:])

	_, err = e.pipe.Delete(ctx, repo, format.Deletion{Path: "app", Reference: subject.Digest.String()}, alice)
	must(t, err)
	if d, err := rr.Resolve(ctx, "sha256-"+subject.Digest.Encoded()); err == nil {
		t.Errorf("the subject's index %s is left after the subject's delete", d.Digest)
	}
}

// listed returns the digests of the referrers of subject in the repository name of prov, sorted.
func listed(ctx context.Context, t *testing.T, prov provider.Provider, name string, subject ocispec.Descriptor) []digest.Digest {
	t.Helper()
	rr, err := prov.Repository(ctx, name)
	must(t, err)
	var out []digest.Digest
	must(t, rr.Referrers(ctx, subject, "", func(page []ocispec.Descriptor) error {
		for _, d := range page {
			out = append(out, d.Digest)
		}
		return nil
	}))
	slices.Sort(out)
	return out
}

// indexed returns the digests the sha256-<hex> index of subject lists in rr, sorted.
func indexed(ctx context.Context, t *testing.T, rr provider.Repository, subject ocispec.Descriptor) []digest.Digest {
	t.Helper()
	_, m := fetchManifest(ctx, t, rr, "sha256-"+subject.Digest.Encoded())
	_ = m
	desc, err := rr.Resolve(ctx, "sha256-"+subject.Digest.Encoded())
	must(t, err)
	rc, err := rr.Fetch(ctx, desc)
	must(t, err)
	defer func() { _ = rc.Close() }()
	var idx ocispec.Index
	must(t, json.NewDecoder(rc).Decode(&idx))
	var out []digest.Digest
	for _, d := range idx.Manifests {
		out = append(out, d.Digest)
	}
	slices.Sort(out)
	return out
}

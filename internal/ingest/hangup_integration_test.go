//go:build integration

package ingest_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestIntegrationDeleteReferrerClientGone is TestDeleteReferrerClientGone against the registry
// of TestIntegration when it has no referrers API (distribution): the provider writes the
// subject's tag-schema index for two referrers, and a client that hangs up right after the
// delete of one took that index still leaves the other listed (#711). A registry with the API
// keeps the list itself, and the test is skipped there.
func TestIntegrationDeleteReferrerClientGone(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e, prov := integrationEnv(t, addr)
	caps, err := prov.Capabilities(ctx)
	must(t, err)
	if caps.Referrers {
		t.Skip("the registry has the referrers API: there is no tag-schema index to keep")
	}
	pub, err := e.pipe.Upload(ctx, e.repo, upload("notes", "1.0", file{"notes", "the subject of " + e.repo.Name}), alice)
	must(t, err)
	name := "raw/" + e.repo.Name + "/notes"
	rr, err := prov.Repository(ctx, name)
	must(t, err)
	subject, err := rr.Resolve(ctx, pub.Digest.String())
	must(t, err)
	var refs []digest.Digest
	for _, layer := range []string{"signature", "attestation"} {
		must(t, rr.Push(ctx, ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromString(layer), Size: int64(len(layer))},
			bytes.NewReader([]byte(layer))))
		r := signature(t, subject, layer)
		_, err := e.pipe.PutReferrer(ctx, e.repo, r, alice)
		must(t, err)
		refs = append(refs, r.Desc.Digest)
	}
	idx, err := rr.Resolve(ctx, "sha256-"+subject.Digest.Encoded())
	must(t, err)

	request, hangUp := context.WithCancel(ctx)
	defer hangUp()
	_, err = ingest.New(e.options(hangingUp{prov, idx.Digest, hangUp})).Delete(request, e.repo,
		format.Deletion{Path: "notes", Reference: refs[0].String()}, alice)
	if request.Err() == nil {
		t.Fatal("the client did not hang up")
	}
	if err != nil {
		t.Errorf("Delete after the client hung up: %v", err)
	}
	if _, err := rr.Resolve(ctx, refs[0].String()); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("the deleted signature resolves: %v", err)
	}
	var listed []digest.Digest
	must(t, rr.Referrers(ctx, subject, "", func(page []ocispec.Descriptor) error {
		for _, d := range page {
			listed = append(listed, d.Digest)
		}
		return nil
	}))
	same(t, "the referrers the subject's tag-schema index lists", []digest.Digest{refs[1]}, listed)
}

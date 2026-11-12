//go:build integration

package ingest_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestIntegrationDeleteReferrerIndexRepair is TestDeleteReferrerIndexNotPushed against the
// registry of TestIntegration when it has no referrers API (distribution): the provider writes
// the subject's tag-schema index for three referrers. A delete of one whose push of the index
// without it fails pushes the index back, a 503, and the three stay listed. With the index then
// lost, as a delete before #711's fix could leave it, the retry rebuilds it from the audit log:
// the referrer goes, and the other two are listed. A registry with the API keeps the list
// itself, and the test is skipped there, as on a registry whose manifests Lajstrom does not
// delete (skipWithoutDeletes).
func TestIntegrationDeleteReferrerIndexRepair(t *testing.T) {
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
	skipWithoutDeletes(t, caps)
	pub, err := e.pipe.Upload(ctx, e.repo, upload("notes", "1.0", file{"notes", "the subject of " + e.repo.Name}), alice)
	must(t, err)
	rr, err := prov.Repository(ctx, "raw/"+e.repo.Name+"/notes")
	must(t, err)
	subject, err := rr.Resolve(ctx, pub.Digest.String())
	must(t, err)
	var refs []digest.Digest
	for _, layer := range []string{"signature", "attestation", "sbom"} {
		must(t, rr.Push(ctx, ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromString(layer), Size: int64(len(layer))},
			bytes.NewReader([]byte(layer))))
		r := signature(t, subject, layer)
		_, err := e.pipe.PutReferrer(ctx, e.repo, r, alice)
		must(t, err)
		refs = append(refs, r.Desc.Digest)
	}
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
	tag := "sha256-" + subject.Digest.Encoded()
	del := format.Deletion{Path: "notes", Reference: refs[0].String()}

	var fail atomic.Int32
	fail.Store(1)
	_, err = ingest.New(e.options(failingIndex{prov, tag, &fail})).Delete(ctx, e.repo, del, alice)
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("Delete with the index push failing: %v, want provider.ErrUnavailable", err)
	}
	same(t, "the referrers listed after the index was pushed back", refs, listed())

	idx, err := rr.Resolve(ctx, tag)
	must(t, err)
	must(t, rr.Delete(ctx, idx))
	same(t, "the referrers listed with the index lost", []digest.Digest(nil), listed())
	_, err = e.pipe.Delete(ctx, e.repo, del, alice)
	must(t, err)
	if _, err := rr.Resolve(ctx, refs[0].String()); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("the deleted signature resolves: %v", err)
	}
	same(t, "the referrers listed after the retry", refs[1:], listed())
}

// skipWithoutDeletes skips a test of deletes on a registry whose manifests the provider does not
// delete (Capabilities.DeleteManifest false: Nexus's docker format, from the vendor table, or a
// registry that refused a delete). Lajstrom refuses every delete there on purpose, before asking
// the registry (#352): what the test checks cannot happen (#800).
func skipWithoutDeletes(t *testing.T, caps provider.Capabilities) {
	t.Helper()
	if !caps.DeleteManifest {
		t.Skip("the registry does not delete manifests (Capabilities.DeleteManifest false): Lajstrom refuses the delete on purpose")
	}
}

// TestIntegrationPutReferrerSubjectDeleted is TestPutReferrerSubjectDeleted against the registry
// of TestIntegration, with the referrers API (Zot) or without (distribution, where the provider
// writes the subject's tag-schema index): a signature pushed before the delete goes with its
// subject, and one whose push waited across the delete after its checks is ErrInvalid; nothing
// of either is left, the sha256-<hex> index included (#806).
func TestIntegrationPutReferrerSubjectDeleted(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e, prov := integrationEnv(t, addr)
	pub, err := e.pipe.Upload(ctx, e.repo, upload("notes", "1.0", file{"notes", "the subject of " + e.repo.Name}), alice)
	must(t, err)
	rr, err := prov.Repository(ctx, "raw/"+e.repo.Name+"/notes")
	must(t, err)
	subject, err := rr.Resolve(ctx, pub.Digest.String())
	must(t, err)
	for _, layer := range []string{"signature", "late"} {
		must(t, rr.Push(ctx, ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromString(layer), Size: int64(len(layer))},
			bytes.NewReader([]byte(layer))))
	}
	early := signature(t, subject, "signature")
	_, err = e.pipe.PutReferrer(ctx, e.repo, early, alice)
	must(t, err)

	g := newExistsGate(prov)
	late := signature(t, subject, "late")
	err = raceSubjectDelete(ctx, t, e, ingest.New(e.options(g)), g, late, subject.Digest)
	if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), subject.Digest.String()) {
		t.Errorf("PutReferrer after its subject's delete: %v, want ErrInvalid naming %s", err, subject.Digest)
	}
	for _, ref := range []string{subject.Digest.String(), early.Desc.Digest.String(), late.Desc.Digest.String(), "sha256-" + subject.Digest.Encoded()} {
		if _, err := rr.Resolve(ctx, ref); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("the registry has %s after the delete: %v", ref, err)
		}
	}
	var listed []ocispec.Descriptor
	err = rr.Referrers(ctx, subject, "", func(page []ocispec.Descriptor) error { listed = append(listed, page...); return nil })
	if err != nil && !errors.Is(err, provider.ErrNotFound) {
		t.Fatal(err)
	}
	same(t, "the referrers listed after the delete", []ocispec.Descriptor(nil), listed)
}

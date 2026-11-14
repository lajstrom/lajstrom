//go:build integration

package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/attest/attesttest"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
)

// TestIntegrationAttachments is TestPutReferrerAttachments against the registry of
// TestIntegration, with the referrers API (Zot) or the tag schema (distribution): two sigstore
// bundles of a version, one signed by the trusted key and one by another, are indexed verified
// and untrusted once the registry serves them; the delete of one takes its row, where the
// registry deletes manifests.
func TestIntegrationAttachments(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e, prov := integrationEnv(t, addr)
	scanner, stranger := attesttest.NewKey(t), attesttest.NewKey(t)
	trust, err := attest.NewTrust([]attest.Entry{scanner.Entry(t, "scanner")})
	must(t, err)
	opts := e.options(prov)
	opts.Trust = trust
	e.pipe = ingest.New(opts)
	pub, err := e.pipe.Upload(ctx, e.repo, upload("notes", "1.0", file{"notes", "the subject of " + e.repo.Name}), alice)
	must(t, err)
	rr, err := prov.Repository(ctx, "raw/"+e.repo.Name+"/notes")
	must(t, err)
	subject, err := rr.Resolve(ctx, pub.Digest.String())
	must(t, err)
	must(t, rr.Push(ctx, ocispec.DescriptorEmptyJSON, bytes.NewReader(ocispec.DescriptorEmptyJSON.Data)))
	statement := attesttest.Statement(subject.Digest, cyclonedx, `{"bomFormat":"CycloneDX"}`)
	var refs []digest.Digest
	for _, key := range []*attesttest.Key{scanner, stranger} {
		b := attesttest.Bundle(t, statement, key)
		layer := ocispec.Descriptor{MediaType: attesttest.BundleMediaType, Digest: digest.FromBytes(b), Size: int64(len(b))}
		must(t, rr.Push(ctx, layer, bytes.NewReader(b)))
		m, err := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
			ArtifactType: attesttest.BundleMediaType, Config: ocispec.DescriptorEmptyJSON, Subject: &subject, Layers: []ocispec.Descriptor{layer},
			Annotations: map[string]string{"dev.sigstore.bundle.predicateType": cyclonedx}})
		must(t, err)
		r := ingest.Referrer{Path: "notes", Manifest: m, Subject: subject.Digest,
			Desc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(m), Size: int64(len(m))}}
		_, err = e.pipe.PutReferrer(ctx, e.repo, r, alice)
		must(t, err)
		refs = append(refs, r.Desc.Digest)
	}
	v, err := e.st.GetVersionByDigest(ctx, e.repo.ID, subject.Digest)
	must(t, err)
	labels := func() map[digest.Digest]string {
		rows, err := e.st.ListAttachments(ctx, []int64{v.ID})
		must(t, err)
		out := map[digest.Digest]string{}
		for _, a := range rows {
			out[a.Digest] = a.Verification
		}
		return out
	}
	same(t, "the labels", map[digest.Digest]string{refs[0]: index.AttachmentVerified, refs[1]: index.AttachmentUntrusted}, labels())

	caps, err := prov.Capabilities(ctx)
	must(t, err)
	skipWithoutDeletes(t, caps)
	_, err = e.pipe.Delete(ctx, e.repo, format.Deletion{Path: "notes", Reference: refs[1].String()}, alice)
	must(t, err)
	same(t, "the labels after the delete of one", map[digest.Digest]string{refs[0]: index.AttachmentVerified}, labels())
}
